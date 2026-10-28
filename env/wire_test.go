package env

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The frames a client sends are the protocol: testdata/wire/frames.json holds
// those upstream's TypeScript client sends for a scenario of operations
// (capture.mts), recorded by a tee in front of the daemon. The Go client runs
// the same scenario and must send the same frames, byte for byte: types, ids,
// JSON (field order included) and payloads.

func TestSendsUpstreamsFrames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records the daemon's input with sh and tee")
	}
	data, err := os.ReadFile("testdata/wire/frames.json")
	mustDo(t, err)
	var golden struct {
		Frames []wireFrame `json:"frames"`
	}
	mustDo(t, json.Unmarshal(data, &golden))
	if len(golden.Frames) == 0 {
		t.Fatal("testdata/wire/frames.json has no frames; recapture it with capture.mts")
	}

	root, err := filepath.EvalSymlinks(tempDir(t, "pi-env-wire-"))
	mustDo(t, err)
	record := filepath.Join(tempDir(t, "pi-env-wire-record-"), "record.bin")
	connection := NewConnection(ConnectionOptions{Command: []string{"sh", "-c", `r=$0 d=$1; shift; tee "$r" | exec "$d" "$@"`, record, testDaemon}})
	defer connection.Close()
	env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:wire", Cwd: root, ShellEnv: map[string]string{"Z": "z"}})
	shellEnv := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{
		Connection: connection, ID: "pi-env:wire", Cwd: root, ShellPath: "/bin/sh",
		Watch: RemoteWatchOptions{Mode: "polling", PollIntervalMS: 100, MaxDirectories: 50},
	})
	ctx := background()
	info, err := connection.Info(ctx)
	mustDo(t, err)
	mustDo(t, env.WriteFile(ctx, "a.txt", []byte("hello")))
	mustDo(t, env.AppendFile(ctx, "a.txt", []byte(" world")))
	big := make([]byte, 600*1024)
	for i := range big {
		big[i] = byte(i % 253)
	}
	mustDo(t, env.WriteFile(ctx, "big.bin", big))
	_, err = env.ReadTextFile(ctx, "a.txt")
	mustDo(t, err)
	_, err = env.ReadBinaryFile(ctx, "big.bin")
	mustDo(t, err)
	_, err = env.Stat(ctx, "a.txt")
	mustDo(t, err)
	_, err = env.ListDir(ctx, ".")
	mustDo(t, err)
	dirReader, err := env.OpenDirReader(ctx, ".")
	mustDo(t, err)
	_, _, err = dirReader.Next(ctx, 10)
	mustDo(t, err)
	dirReader.Close()
	reader, err := env.OpenBinaryReader(ctx, "a.txt", &BinaryReaderOptions{NoFollow: true})
	mustDo(t, err)
	_, err = reader.Read(ctx, 0, 5)
	mustDo(t, err)
	_, err = reader.ScanLines(ctx, LineRange{StartLine: 0, EndLine: 1})
	mustDo(t, err)
	_, err = reader.Info(ctx)
	mustDo(t, err)
	reader.Close()
	lines, err := env.OpenTextLineReader(ctx, "a.txt")
	mustDo(t, err)
	_, err = lines.ReadLine(ctx)
	mustDo(t, err)
	lines.Close()
	_, err = env.ReadTextLines(ctx, "a.txt", 1)
	mustDo(t, err)
	mustDo(t, env.TruncateFile(ctx, "a.txt", 3))
	mustDo(t, env.FlushFile(ctx, "a.txt"))
	mustDo(t, env.RenameFile(ctx, "a.txt", "b.txt"))
	_, err = env.CanonicalPath(ctx, "b.txt")
	mustDo(t, err)
	_, err = env.Exists(ctx, "b.txt")
	mustDo(t, err)
	mustDo(t, env.CreateDir(ctx, "d/e", nil))
	mustDo(t, env.CreateDir(ctx, "f", &CreateDirOptions{Recursive: new(false)}))
	mustDo(t, env.Remove(ctx, "d", &RemoveOptions{Recursive: true, Force: true}))
	_, err = env.CreateTempDir(ctx, "x-")
	mustDo(t, err)
	_, err = env.CreateTempFile(ctx, &TempFileOptions{Prefix: "p-", Suffix: ".s"})
	mustDo(t, err)
	_, err = env.RunProgram(ctx, []string{"sh", "-c", "exit 3"}, &RunOptions{
		Cwd: "f", Env: map[string]string{"A": "1"}, InheritEnv: new(false), Timeout: 5,
		Spill:  &SpillOptions{AfterBytes: 10, AfterLines: 2},
		Window: &OutputWindow{MaxBytes: 100, MaxLines: 5, MinIntervalMS: 10, BytesPerSecond: 1000},
	})
	mustDo(t, err)
	_, err = shellEnv.RunShell(ctx, "printf hi", &RunOptions{Env: map[string]string{"B": "2"}})
	mustDo(t, err)
	_, err = env.ListFiles(ctx, ".", &ListFilesOptions{
		Hidden: new(false), Gitignore: new(false), Exclude: []string{"node_modules"},
		MaxEntries: new(int64(10)), MaxBytes: new(int64(1000)), TimeoutMS: new(int64(5000)),
	})
	mustDo(t, err)
	_, err = env.ListFiles(ctx, ".", nil)
	mustDo(t, err)
	watcher, err := shellEnv.Watch(ctx, []WatchTarget{
		{Path: "w", Recursive: true, Exclude: WatchExclude{Hidden: true, Names: []string{"n"}}},
		{Path: "b.txt"},
	}, func(FileChange) {})
	mustDo(t, err)
	watcher.Stop()
	abortCtx, abort := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, abort)
	_, _ = env.RunProgram(abortCtx, []string{"sleep", "5"}, nil)
	killed := make(chan struct{})
	go func() {
		defer close(killed)
		_, _ = env.RunProgram(ctx, []string{"sleep", "5"}, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	mustDo(t, env.Cleanup())
	<-killed
	terminal, err := OpenTerminal(ctx, connection, TerminalOptions{
		Cwd: root, Argv: []string{"sh", "-c", "read x; exit 0"}, Env: map[string]string{"TERM": "xterm"},
		InheritEnv: new(true), Cols: 80, Rows: 24, OnData: func([]byte) {},
	})
	mustDo(t, err)
	terminal.Resize(100, 30)
	_, err = terminal.Foreground(ctx)
	mustDo(t, err)
	terminal.Write([]byte("ok\n"))
	_, err = terminal.Wait()
	mustDo(t, err)
	connection.Close()

	stream, err := os.ReadFile(record)
	mustDo(t, err)
	frames := readWireFrames(stream, root, info.Tmpdir)
	for i := 0; i < max(len(frames), len(golden.Frames)); i++ {
		switch {
		case i >= len(frames):
			t.Fatalf("frame %d missing: upstream sends %+v", i, golden.Frames[i])
		case i >= len(golden.Frames):
			t.Fatalf("frame %d is extra: %+v", i, frames[i])
		case frames[i] != golden.Frames[i]:
			t.Fatalf("frame %d:\n got %+v\nwant %+v", i, frames[i], golden.Frames[i])
		}
	}
}

type wireFrame struct {
	Type    int    `json:"type"`
	ID      int    `json:"id"`
	JSON    string `json:"json"`
	Payload string `json:"payload"`
}

var (
	randomTempDir  = regexp.MustCompile(`x-[A-Za-z0-9]{6}`)
	randomTempFile = regexp.MustCompile(`tmp-[A-Za-z0-9]{6}`)
	randomUUIDName = regexp.MustCompile(`p-[0-9a-f-]{36}\.s`)
)

// readWireFrames parses a recorded client stream, without pings, masked as
// capture.mts masks it.
func readWireFrames(stream []byte, root, tmpdir string) []wireFrame {
	escaped := func(path string) string {
		quoted, _ := json.Marshal(path)
		return string(quoted[1 : len(quoted)-1])
	}
	var frames []wireFrame
	for offset := 0; offset+4 <= len(stream); {
		length := int(binary.BigEndian.Uint32(stream[offset:]))
		kind := int(stream[offset+4])
		id := int(binary.BigEndian.Uint32(stream[offset+5:]))
		jsonLength := int(binary.BigEndian.Uint32(stream[offset+9:]))
		text := string(stream[offset+13 : offset+13+jsonLength])
		payload := stream[offset+13+jsonLength : offset+4+length]
		offset += 4 + length
		if kind == 6 {
			continue
		}
		text = strings.ReplaceAll(text, escaped(root), "<root>")
		// A prefix only: the temporary directory may be /tmp, and names may
		// start with tmp.
		text = strings.ReplaceAll(text, `"`+escaped(tmpdir), `"<tmpdir>`)
		text = randomTempDir.ReplaceAllString(text, "x-<random>")
		text = randomTempFile.ReplaceAllString(text, "tmp-<random>")
		text = randomUUIDName.ReplaceAllString(text, "p-<uuid>.s")
		sum := sha256.Sum256(payload)
		frames = append(frames, wireFrame{Type: kind, ID: id, JSON: text, Payload: hex.EncodeToString(sum[:])})
	}
	return frames
}
