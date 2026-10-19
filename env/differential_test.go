package env

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sky-valley/pi/coding"
)

// Upstream's differential.test.ts (d2e7d8b7c) runs random file operation
// sequences against RemoteExecutionEnv and NodeExecutionEnv on the same
// machine and compares the results. Here NodeExecutionEnv's results are
// captured (testdata/differential/capture.mts, pi-durable 1.3.0) and the Go
// RemoteExecutionEnv must give the same ones for the same sequences.
//
// Not ported: "gives the same read and bash tool results" runs Durable's
// read and bash tools, which have no Go port yet (Scope queue row 8).

type differentialGolden struct {
	Node      string          `json:"node"`
	Platform  string          `json:"platform"`
	Devices   json.RawMessage `json:"devices"`
	Names     json.RawMessage `json:"names"`
	Sequences []struct {
		Seed  int `json:"seed"`
		Steps []struct {
			Label  string          `json:"label"`
			Result json.RawMessage `json:"result"`
		} `json:"steps"`
	} `json:"sequences"`
}

func loadDifferential(t *testing.T) differentialGolden {
	t.Helper()
	path := filepath.Join("testdata", "differential", runtime.GOOS+".json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no NodeExecutionEnv results for %s; capture %s with testdata/differential/capture.mts", runtime.GOOS, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	var golden differentialGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Sequences) == 0 {
		t.Fatalf("%s has no sequences; recapture it", path)
	}
	return golden
}

// random is upstream's PRNG (mulberry32), in uint32 arithmetic.
func random(seed uint32) func() float64 {
	state := seed
	return func() float64 {
		state += 0x6d2b79f5
		t := state
		t = (t ^ (t >> 15)) * (t | 1)
		t ^= t + (t^(t>>7))*(t|61)
		return float64(t^(t>>14)) / 4294967296
	}
}

var (
	differentialNames    = []string{"a.txt", "b", "dir", "dir/c.txt", "dir/sub", "dir/sub/d.bin", "missing", "a.txt/x"}
	differentialContents = []any{
		"",
		"hello\n",
		"\ufeffbom\r\nline",
		[]byte{0xe2, 0x82, 0x0a, 0xff, 0xef, 0xbb, 0xbf},
		strings.Repeat("x", 70_000),
		strings.Repeat("é", 300_001),
	}
)

func contentBytes(content any) []byte {
	if s, ok := content.(string); ok {
		return []byte(s)
	}
	return content.([]byte)
}

// result is a Result as upstream's normalize renders it.
func result(value any, err error) any {
	if err != nil {
		return map[string]any{"ok": false, "code": errorCode(err)}
	}
	if value == nil {
		return map[string]any{"ok": true}
	}
	return map[string]any{"ok": true, "value": value}
}

func infoJSON(info coding.FileInfo) any {
	return map[string]any{"name": info.Name, "path": info.Path, "kind": string(info.Kind), "size": info.Size, "mtimeMs": "mtime"}
}

func bytesJSON(data []byte) any { return map[string]any{"bytes": hex.EncodeToString(data)} }

// randomOperation consumes next exactly as upstream's does.
func randomOperation(next func() float64) (string, func(env *RemoteExecutionEnv) any) {
	var args []string
	pick := func(value any) any {
		switch v := value.(type) {
		case []byte:
			args = append(args, `"<bytes>"`)
		case string:
			if n := len(utf16.Encode([]rune(v))); n > 20 {
				args = append(args, fmt.Sprintf(`"<%d chars>"`, n))
			} else {
				quoted, _ := json.Marshal(v)
				args = append(args, string(quoted))
			}
		default:
			quoted, _ := json.Marshal(v)
			args = append(args, string(quoted))
		}
		return value
	}
	index := func(n int) int { return int(math.Floor(next() * float64(n))) }
	name := func() string { return pick(differentialNames[index(len(differentialNames))]).(string) }
	content := func() any { return pick(differentialContents[index(len(differentialContents))]) }
	flag := func() bool { return pick(next() < 0.5).(bool) }
	small := func(limit int) int { return pick(index(limit)).(int) }
	ctx := background()
	choices := []struct {
		label string
		make  func() func(env *RemoteExecutionEnv) any
	}{
		{"writeFile", func() func(*RemoteExecutionEnv) any {
			n, c := name(), content()
			return func(env *RemoteExecutionEnv) any { return result(nil, env.WriteFile(ctx, n, contentBytes(c))) }
		}},
		{"appendFile", func() func(*RemoteExecutionEnv) any {
			n, c := name(), content()
			return func(env *RemoteExecutionEnv) any { return result(nil, env.AppendFile(ctx, n, contentBytes(c))) }
		}},
		{"readTextFile", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any { return result(env.ReadTextFile(ctx, n)) }
		}},
		{"readBinaryFile", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any {
				data, err := env.ReadBinaryFile(ctx, n)
				return result(bytesJSON(data), err)
			}
		}},
		{"readTextLines", func() func(*RemoteExecutionEnv) any {
			n, m := name(), small(4)
			return func(env *RemoteExecutionEnv) any {
				if m == 0 {
					// Durable's maxLines 0 reads nothing, without opening the
					// file; coding.FileSystem's 0 means no limit.
					return result([]string{}, nil)
				}
				return result(env.ReadTextLines(ctx, n, m))
			}
		}},
		{"fileInfo", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any {
				info, err := env.Stat(ctx, n)
				return result(infoJSON(info), err)
			}
		}},
		{"exists", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any { return result(env.Exists(ctx, n)) }
		}},
		{"listDir", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any {
				infos, err := env.ListDir(ctx, n)
				list := []any{}
				for _, info := range infos {
					list = append(list, infoJSON(info))
				}
				return result(list, err)
			}
		}},
		{"createDir", func() func(*RemoteExecutionEnv) any {
			n, r := name(), flag()
			return func(env *RemoteExecutionEnv) any {
				return result(nil, env.CreateDir(ctx, n, &CreateDirOptions{Recursive: &r}))
			}
		}},
		{"remove", func() func(*RemoteExecutionEnv) any {
			n, r, f := name(), flag(), flag()
			return func(env *RemoteExecutionEnv) any {
				return result(nil, env.Remove(ctx, n, &RemoveOptions{Recursive: r, Force: f}))
			}
		}},
		{"renameFile", func() func(*RemoteExecutionEnv) any {
			a, b := name(), name()
			return func(env *RemoteExecutionEnv) any { return result(nil, env.RenameFile(ctx, a, b)) }
		}},
		{"truncateFile", func() func(*RemoteExecutionEnv) any {
			n, size := name(), small(10)
			return func(env *RemoteExecutionEnv) any { return result(nil, env.TruncateFile(ctx, n, int64(size))) }
		}},
		{"flushFile", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any { return result(nil, env.FlushFile(ctx, n)) }
		}},
		{"canonicalPath", func() func(*RemoteExecutionEnv) any {
			n := name()
			return func(env *RemoteExecutionEnv) any { return result(env.CanonicalPath(ctx, n)) }
		}},
		{"openBinaryReader", func() func(*RemoteExecutionEnv) any {
			n, noFollow, offset, length := name(), flag(), small(8), small(8)
			return func(env *RemoteExecutionEnv) any {
				reader, err := env.OpenBinaryReader(ctx, n, &BinaryReaderOptions{NoFollow: noFollow})
				if err != nil {
					return result(nil, err)
				}
				defer reader.Close()
				data, err := reader.Read(ctx, int64(offset), int64(length))
				read := result(bytesJSON(data), err)
				scan, err := reader.ScanLines(ctx, LineRange{})
				return []any{read, result(scan, err)}
			}
		}},
	}
	choice := choices[index(len(choices))]
	run := choice.make()
	return choice.label + "(" + strings.Join(args, ", ") + ")", run
}

// normalizeResult renders a result as upstream normalizes it, with root as
// <root>, then compacts it as capture.mts does.
func normalizeResult(t *testing.T, value any, root string) any {
	t.Helper()
	var buf strings.Builder
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	escaped := func(path string) string {
		quoted, _ := json.Marshal(path)
		return string(quoted[1 : len(quoted)-1])
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		text = strings.ReplaceAll(text, escaped(real), "<root>")
	}
	text = strings.ReplaceAll(text, escaped(root), "<root>")
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatal(err)
	}
	return compact(parsed)
}

// compact is capture.mts's: a string of more than 256 UTF-16 code units
// becomes its SHA-256 and UTF-8 length.
func compact(value any) any {
	switch v := value.(type) {
	case string:
		if len(utf16.Encode([]rune(v))) <= 256 {
			return v
		}
		sum := sha256.Sum256([]byte(v))
		return map[string]any{"sha256": hex.EncodeToString(sum[:]), "length": float64(len(v))}
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = compact(item)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for key, field := range v {
			out[key] = compact(field)
		}
		return out
	}
	return value
}

func TestGivesNodeExecutionEnvsResultsForRandomFileOperationSequences(t *testing.T) {
	golden := loadDifferential(t)
	connection := newConnection(t)
	for _, sequence := range golden.Sequences {
		next := random(uint32(sequence.Seed))
		root := tempDir(t, "pi-env-remote-")
		env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: root})
		for step, want := range sequence.Steps {
			label, run := randomOperation(next)
			if label != want.Label {
				t.Fatalf("seed %d step %d: the sequence is %s, NodeExecutionEnv ran %s", sequence.Seed, step, label, want.Label)
			}
			got := normalizeResult(t, run(env), root)
			var expected any
			_ = json.Unmarshal(want.Result, &expected)
			if !reflect.DeepEqual(got, expected) {
				gotJSON, _ := json.Marshal(got)
				t.Fatalf("seed %d step %d %s:\n got %s\nwant %s", sequence.Seed, step, label, gotJSON, want.Result)
			}
		}
	}
}

func TestReadsDevicesFIFOsAndDirectoriesLikeNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs or /dev/null on Windows")
	}
	golden := loadDifferential(t)
	env, _ := environment(t)
	root := env.Cwd()
	ctx := background()
	fifo := filepath.Join(root, "fifo")
	if made, err := env.RunProgram(ctx, []string{"mkfifo", fifo}, nil); err != nil || made.ExitCode != 0 {
		t.Fatalf("mkfifo: %+v %v", made, err)
	}
	mustDo(t, env.CreateDir(ctx, "dir", nil))
	// A writer for each blocking open of the FIFO.
	write := func(text string) {
		cmd := exec.Command("sh", "-c", `printf '%s' "$1" > "$2"`, "sh", text, fifo)
		mustDo(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}
	write("fifo text\n")
	fifoText, fifoTextErr := env.ReadTextFile(ctx, "fifo")
	write("fifo bytes")
	fifoBytes, fifoBytesErr := env.ReadBinaryFile(ctx, "fifo")
	write("x")
	fifoLines, fifoLinesErr := env.ReadTextLines(ctx, "fifo", 0)
	dirReader, dirReaderErr := env.OpenTextLineReader(ctx, "dir")
	var dirLine any
	if dirReaderErr == nil {
		line, err := dirReader.ReadLine(ctx)
		if errors.Is(err, io.EOF) {
			dirLine = result(nil, nil)
		} else {
			dirLine = result(map[string]any{"text": line.Text, "terminated": line.Terminated}, err)
		}
		dirReader.Close()
	}
	nullText, nullTextErr := env.ReadTextFile(ctx, "/dev/null")
	nullBytes, nullBytesErr := env.ReadBinaryFile(ctx, "/dev/null")
	nullLines, nullLinesErr := env.ReadTextLines(ctx, "/dev/null", 0)
	dirText, dirTextErr := env.ReadTextFile(ctx, "dir")
	dirBytes, dirBytesErr := env.ReadBinaryFile(ctx, "dir")
	got := normalizeResult(t, map[string]any{
		"nullText":        result(nullText, nullTextErr),
		"nullBytes":       result(bytesJSON(nullBytes), nullBytesErr),
		"nullLines":       result(nullLines, nullLinesErr),
		"fifoText":        result(fifoText, fifoTextErr),
		"fifoBytes":       result(bytesJSON(fifoBytes), fifoBytesErr),
		"fifoLines":       result(fifoLines, fifoLinesErr),
		"dirReaderOpened": dirReaderErr == nil,
		"dirLine":         dirLine,
		"dirText":         result(dirText, dirTextErr),
		"dirBytes":        result(bytesJSON(dirBytes), dirBytesErr),
	}, root)
	var expected any
	_ = json.Unmarshal(golden.Devices, &expected)
	if !reflect.DeepEqual(got, expected) {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("got  %s\nwant %s", gotJSON, golden.Devices)
	}
}

// TestListsNamesThatAreNotUTF8LikeNode: names that are not UTF-8 are decoded
// with replacement characters and lstat'ed under that name, so listDir fails
// and a directory reader skips them. The pages' split depends on the order
// the file system returns, which differs between file systems, so the pages
// are compared by size and contents, not order.
func TestListsNamesThatAreNotUTF8LikeNode(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		t.Skip("macOS and Windows file systems refuse names that are not UTF-8")
	}
	golden := loadDifferential(t)
	env, _ := environment(t)
	root := env.Cwd()
	for _, name := range [][]byte{{0x62}, {0x61, 0xff}, {0xc3, 0xa9}, {0x61, 0xfe, 0x7a}, {0x7a}} {
		mustDo(t, os.WriteFile(root+"/"+string(name), []byte("x"), 0o644))
	}
	ctx := background()
	infos, listErr := env.ListDir(ctx, ".")
	reader, err := env.OpenDirReader(ctx, ".")
	mustDo(t, err)
	var pages []any
	for done := false; !done; {
		entries, last, err := reader.Next(ctx, 2)
		page := []any{}
		for _, info := range entries {
			page = append(page, infoJSON(info))
		}
		pages = append(pages, result(map[string]any{"entries": page, "done": last}, err))
		done = err != nil || last
	}
	reader.Close()
	list := []any{}
	for _, info := range infos {
		list = append(list, infoJSON(info))
	}
	got := normalizeResult(t, map[string]any{"list": result(list, listErr), "pages": pages}, root).(map[string]any)
	var want map[string]any
	mustDo(t, json.Unmarshal(golden.Names, &want))
	if !reflect.DeepEqual(got["list"], want["list"]) {
		t.Fatalf("listDir %v, Node %v", got["list"], want["list"])
	}
	// Every page but the last is full; together they hold the same entries.
	entries := func(pages any) (sizes []int, all []string) {
		for _, page := range pages.([]any) {
			value := page.(map[string]any)["value"].(map[string]any)
			list := value["entries"].([]any)
			sizes = append(sizes, len(list))
			for _, entry := range list {
				data, _ := json.Marshal(entry)
				all = append(all, string(data))
			}
		}
		sort.Strings(all)
		return sizes, all
	}
	gotSizes, gotEntries := entries(got["pages"])
	wantSizes, wantEntries := entries(want["pages"])
	if !reflect.DeepEqual(gotSizes, wantSizes) || !reflect.DeepEqual(gotEntries, wantEntries) {
		t.Fatalf("pages %v %v, Node %v %v", gotSizes, gotEntries, wantSizes, wantEntries)
	}
}
