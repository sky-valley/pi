package env

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/coding"
	"github.com/sky-valley/pi/internal/jstext"
)

// Durable's env conformance suite (packages/durable/src/testing/
// env-conformance.ts at d2e7d8b7c), which upstream's conformance.test.ts runs
// against RemoteExecutionEnv over a pipe, and its listFiles cases against
// LocalExecutionEnv. Every case gets an environment in a fresh directory.

// conformanceEnv is a RemoteExecutionEnv in a fresh directory, on one
// connection shared by the cases, as upstream shares it.
func conformanceEnv(t *testing.T, connection *Connection) *RemoteExecutionEnv {
	t.Helper()
	return NewRemoteExecutionEnv(RemoteExecutionEnvOptions{
		Connection: connection, ID: "pi-env:test", Cwd: tempDir(t, "pi-env-conformance-"),
		Watch: RemoteWatchOptions{PollIntervalMS: 100},
	})
}

var shell = []string{"sh", "-c"}

func withShell(script ...string) []string { return append(append([]string(nil), shell...), script...) }

func execCollect(env *RemoteExecutionEnv, command []string, cwd string) (RunResult, error, string, string) {
	var stdout, stderr strings.Builder
	result, err := env.RunProgram(background(), command, &RunOptions{Cwd: cwd, OnOutput: collectInto(&stdout, &stderr)})
	return result, err, stdout.String(), stderr.String()
}

func collectInto(stdout, stderr *strings.Builder) func(string, OutputInfo) error {
	return func(text string, info OutputInfo) error {
		if info.Stream == "stderr" {
			stderr.WriteString(text)
		} else {
			stdout.WriteString(text)
		}
		return nil
	}
}

func TestConformance(t *testing.T) {
	connection := newConnection(t)
	cases := []struct {
		name string
		run  func(t *testing.T, env *RemoteExecutionEnv)
	}{
		{"binary reader reads byte ranges of the opened file", conformBinaryRanges},
		{"binary reader scans lines like decoding the whole file", conformScanLines},
		{"binary reader keeps reading the file it opened after a rename", conformReaderAfterRename},
		{"binary reader refuses directories, missing files and aborted opens", conformReaderRefuses},
		{"directory reader pages every entry exactly once", conformDirPages},
		{"directory reader reports the end and refuses use after close", conformDirEnd},
		{"directory reader refuses missing paths and files", conformDirRefuses},
		{"directory reader skips entries removed during enumeration", conformDirSkipsRemoved},
		{"watch reports a missing file's creation, changes, replacement and removal", conformWatchFile},
		{"watch reports a missing target whose ancestors are created", conformWatchAncestors},
		{"watch follows directories created together with their contents", conformWatchNewDirectories},
		{"watch keeps watching a path whose parent is renamed and recreated", conformWatchParentRenamed},
		{"watch skips excluded entries and reports a rename out of them", conformWatchExcluded},
		{"watch keeps recursive coverage where a non-recursive target overlaps", conformWatchOverlap},
		{"watch follows a directory replaced at the same path", conformWatchReplacedDirectory},
		{"watch stops reporting once stopped", conformWatchStopped},
		{"argv exec passes arguments to the program without shell parsing", conformArgv},
		{"exec reports the stream of every chunk in both forms", conformStreams},
		{"argv exec honors cwd and exit codes", conformCwdAndExitCodes},
		{"argv exec reports missing programs and empty argv as spawn errors", conformSpawnErrors},
		{"windowed exec keeps the exact tail and counts what it skips", conformWindow},
		{"argv exec distinguishes timeout from abort", conformTimeoutAndAbort},
		{"binary reader follows symlinks unless noFollow refuses the final one", conformReaderSymlinks},
		{"watch reports changes to the file a watched symbolic link points to", conformWatchSymlink},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.Contains(c.name, "symlink") {
				t.Skip("Git Bash's ln -s copies unless native symlinks are enabled")
			}
			c.run(t, conformanceEnv(t, connection))
		})
	}
	for _, c := range listFilesCases {
		t.Run(c.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.Contains(c.name, "symbolic") {
				t.Skip("Git Bash's ln -s copies unless native symlinks are enabled")
			}
			c.run(t, remoteListing{conformanceEnv(t, connection)})
		})
	}
}

// TestLocalExecutionEnvConformance runs the listFiles cases against
// LocalExecutionEnv, the rest of whose conformance is coding.LocalEnv's.
func TestLocalExecutionEnvConformance(t *testing.T) {
	connection := newConnection(t)
	for _, c := range listFilesCases {
		t.Run(c.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.Contains(c.name, "symbolic") {
				t.Skip("Git Bash's ln -s copies unless native symlinks are enabled")
			}
			c.run(t, localListing{NewLocalExecutionEnv(LocalExecutionEnvOptions{Connection: connection, Cwd: tempDir(t, "pi-env-local-conformance-")})})
		})
	}
}

func conformBinaryRanges(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.WriteFile(background(), "data.txt", []byte("hello world")))
	reader, err := env.OpenBinaryReader(background(), "data.txt", nil)
	mustDo(t, err)
	info, err := reader.Info(background())
	mustDo(t, err)
	if info.Name != "data.txt" || info.Kind != coding.FileKindFile || info.Size != 11 {
		t.Fatalf("info %+v", info)
	}
	read := func(offset, length int64) string {
		data, err := reader.Read(background(), offset, length)
		mustDo(t, err)
		return string(data)
	}
	if read(0, 5) != "hello" || read(6, 100) != "world" || read(11, 4) != "" || read(50, 1) != "" || read(3, 0) != "" {
		t.Fatal("ranges differ")
	}
	if _, err := reader.Read(background(), -1, 1); errorCode(err) != "invalid" {
		t.Fatalf("negative offset: %v", err)
	}
	if _, err := reader.Read(abortedContext(), 0, 1); errorCode(err) != "aborted" {
		t.Fatalf("aborted read: %v", err)
	}
	reader.Close()
	reader.Close()
	if _, err := reader.Read(background(), 0, 1); errorCode(err) != "invalid" {
		t.Fatalf("read after close: %v", err)
	}
	if _, err := reader.Info(background()); errorCode(err) != "invalid" {
		t.Fatalf("info after close: %v", err)
	}
}

func conformScanLines(t *testing.T, env *RemoteExecutionEnv) {
	// A byte-order mark, an invalid sequence before a newline, an empty line,
	// a later U+FEFF, and no final newline.
	data := []byte{0xef, 0xbb, 0xbf, 0x61, 0x0a, 0xe2, 0x82, 0x0a, 0x0a, 0xef, 0xbb, 0xbf, 0x62, 0x0a, 0xc3, 0xa9}
	mustDo(t, env.WriteFile(background(), "lines.txt", data))
	lines := strings.Split(jstext.DecodeText(data), "\n")
	reader, err := env.OpenBinaryReader(background(), "lines.txt", nil)
	mustDo(t, err)
	defer reader.Close()
	decodeRange := func(from, to int64) string {
		if from > 0 {
			return jstext.DecodeUTF8(data[from:to])
		}
		return jstext.DecodeText(data[from:to])
	}
	for _, r := range [][2]int64{{0, 0}, {0, 1}, {1, 3}, {2, 3}, {3, 0}, {4, 9}} {
		scan, err := reader.ScanLines(background(), LineRange{StartLine: r[0], EndLine: r[1]})
		mustDo(t, err)
		end := len(lines)
		if r[1] != 0 {
			end = min(int(r[1]), len(lines))
		}
		selected := strings.Join(lines[r[0]:end], "\n")
		if scan.Newlines != int64(len(lines)-1) || decodeRange(scan.Start, scan.End) != selected ||
			scan.SelectedBytes != int64(len(selected)) || decodeRange(scan.Start, scan.FirstLineEnd) != lines[r[0]] ||
			scan.FirstLineBytes != int64(len(lines[r[0]])) {
			t.Fatalf("lines %v: %+v", r, scan)
		}
	}
	scan, err := reader.ScanLines(background(), LineRange{StartLine: 9})
	mustDo(t, err)
	if scan.Start != int64(len(data)) || scan.End != int64(len(data)) || scan.SelectedBytes != 0 {
		t.Fatalf("past the last line: %+v", scan)
	}
	if _, err := reader.ScanLines(background(), LineRange{StartLine: 2, EndLine: 2}); errorCode(err) != "invalid" {
		t.Fatalf("empty range: %v", err)
	}
}

func conformReaderAfterRename(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.WriteFile(background(), "a.txt", []byte("one")))
	reader, err := env.OpenBinaryReader(background(), "a.txt", nil)
	mustDo(t, err)
	defer reader.Close()
	mustDo(t, env.RenameFile(background(), "a.txt", "b.txt"))
	mustDo(t, env.WriteFile(background(), "a.txt", []byte("two")))
	if data, err := reader.Read(background(), 0, 10); err != nil || string(data) != "one" {
		t.Fatalf("read %q, %v", data, err)
	}
}

func conformReaderRefuses(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.CreateDir(background(), "dir", nil))
	mustDo(t, env.WriteFile(background(), "file.txt", []byte("x")))
	for _, c := range []struct {
		ctx  context.Context
		path string
		code string
	}{{background(), "dir", "is_directory"}, {background(), "missing.txt", "not_found"}, {abortedContext(), "file.txt", "aborted"}} {
		if _, err := env.OpenBinaryReader(c.ctx, c.path, nil); errorCode(err) != c.code {
			t.Fatalf("%s: %v, want %s", c.path, err, c.code)
		}
	}
}

// readAllPages reads a directory in pages of maxEntries.
func readAllPages(t *testing.T, env *RemoteExecutionEnv, path string, maxEntries int) ([][]coding.FileInfo, bool) {
	reader, err := env.OpenDirReader(background(), path)
	mustDo(t, err)
	defer reader.Close()
	var pages [][]coding.FileInfo
	for page := 0; page < 1000; page++ {
		entries, done, err := reader.Next(background(), maxEntries)
		mustDo(t, err)
		pages = append(pages, entries)
		if done {
			return pages, true
		}
	}
	return pages, false
}

func conformDirPages(t *testing.T, env *RemoteExecutionEnv) {
	names := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
	for _, name := range names {
		mustDo(t, env.WriteFile(background(), name, []byte(name)))
	}
	mustDo(t, env.CreateDir(background(), "sub", nil))
	pages, done := readAllPages(t, env, ".", 2)
	if !done {
		t.Fatal("the reader did not reach the end")
	}
	var all []coding.FileInfo
	for _, page := range pages {
		if len(page) > 2 {
			t.Fatalf("page of %d", len(page))
		}
		all = append(all, page...)
	}
	var got []string
	byName := map[string]coding.FileInfo{}
	for _, entry := range all {
		got = append(got, entry.Name)
		byName[entry.Name] = entry
	}
	sort.Strings(got)
	if want := append(append([]string(nil), names...), "sub"); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries %v", got)
	}
	if byName["sub"].Kind != coding.FileKindDirectory || byName["a.txt"].Kind != coding.FileKindFile || byName["a.txt"].Size != 5 {
		t.Fatalf("entries %+v", byName)
	}
}

func conformDirEnd(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.CreateDir(background(), "empty", nil))
	reader, err := env.OpenDirReader(background(), "empty")
	mustDo(t, err)
	for i := 0; i < 2; i++ {
		if entries, done, err := reader.Next(background(), 10); err != nil || len(entries) != 0 || !done {
			t.Fatalf("next %d: %v %v %v", i, entries, done, err)
		}
	}
	if _, _, err := reader.Next(background(), 0); errorCode(err) != "invalid" {
		t.Fatalf("maxEntries 0: %v", err)
	}
	if _, _, err := reader.Next(abortedContext(), 1); errorCode(err) != "aborted" {
		t.Fatalf("aborted: %v", err)
	}
	reader.Close()
	reader.Close()
	if _, _, err := reader.Next(background(), 1); errorCode(err) != "invalid" {
		t.Fatalf("after close: %v", err)
	}
}

func conformDirRefuses(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.WriteFile(background(), "file.txt", []byte("x")))
	for _, c := range []struct {
		ctx  context.Context
		path string
		code string
	}{{background(), "missing", "not_found"}, {background(), "file.txt", "not_directory"}, {abortedContext(), ".", "aborted"}} {
		if _, err := env.OpenDirReader(c.ctx, c.path); errorCode(err) != c.code {
			t.Fatalf("%s: %v, want %s", c.path, err, c.code)
		}
	}
}

func conformDirSkipsRemoved(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.CreateDir(background(), "dir", nil))
	for _, name := range []string{"x", "y", "z"} {
		mustDo(t, env.WriteFile(background(), "dir/"+name, []byte(name)))
	}
	reader, err := env.OpenDirReader(background(), "dir")
	mustDo(t, err)
	defer reader.Close()
	for _, name := range []string{"x", "y", "z"} {
		mustDo(t, env.Remove(background(), "dir/"+name, nil))
	}
	var entries []coding.FileInfo
	for page := 0; page < 10; page++ {
		next, done, err := reader.Next(background(), 10)
		mustDo(t, err)
		entries = append(entries, next...)
		if done {
			break
		}
	}
	if len(entries) != 0 {
		t.Fatalf("entries %+v", entries)
	}
}

// watchSession watches targets while a case changes files.
type watchSession struct {
	t       *testing.T
	env     *RemoteExecutionEnv
	mu      sync.Mutex
	changes []FileChange
}

func (w *watchSession) all() []FileChange {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]FileChange(nil), w.changes...)
}

func (w *watchSession) absolute(path string) string {
	absolute, err := w.env.AbsolutePath(background(), path)
	mustDo(w.t, err)
	return absolute
}

// covers: an overflow, or a reported path at or above path.
func covers(change FileChange, path string) bool {
	if change.Kind == FileChangeOverflow {
		return true
	}
	if change.Kind != FileChangePaths {
		return false
	}
	for _, reported := range change.Paths {
		if path == reported || strings.HasPrefix(path, reported+"/") || strings.HasPrefix(path, reported+"\\") {
			return true
		}
	}
	return false
}

// expectChange waits up to three seconds for a change, after change runs,
// that reports path.
func (w *watchSession) expectChange(path string, change func()) {
	w.t.Helper()
	target := w.absolute(path)
	from := len(w.all())
	change()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, c := range w.all()[from:] {
			if covers(c, target) {
				return
			}
		}
		if time.Now().After(deadline) {
			got, _ := json.Marshal(w.all()[from:])
			w.t.Fatalf("no change reported %s; got %s", target, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func watching(t *testing.T, env *RemoteExecutionEnv, targets []WatchTarget, run func(w *watchSession)) {
	w := &watchSession{t: t, env: env}
	watcher, err := env.Watch(background(), targets, func(change FileChange) {
		w.mu.Lock()
		w.changes = append(w.changes, change)
		w.mu.Unlock()
	})
	mustDo(t, err)
	defer watcher.Stop()
	run(w)
}

func write(t *testing.T, env *RemoteExecutionEnv, path, content string) {
	t.Helper()
	mustDo(t, env.WriteFile(background(), path, []byte(content)))
}

func conformWatchFile(t *testing.T, env *RemoteExecutionEnv) {
	watching(t, env, []WatchTarget{{Path: "AGENTS.md"}}, func(w *watchSession) {
		w.expectChange("AGENTS.md", func() { write(t, env, "AGENTS.md", "one") })
		w.expectChange("AGENTS.md", func() { write(t, env, "AGENTS.md", "two!") })
		// Editors replace a file by renaming a new one over it.
		w.expectChange("AGENTS.md", func() {
			write(t, env, "AGENTS.md.tmp", "three")
			mustDo(t, env.RenameFile(background(), "AGENTS.md.tmp", "AGENTS.md"))
		})
		w.expectChange("AGENTS.md", func() { write(t, env, "AGENTS.md", "four") })
		w.expectChange("AGENTS.md", func() { mustDo(t, env.Remove(background(), "AGENTS.md", nil)) })
	})
}

func conformWatchAncestors(t *testing.T, env *RemoteExecutionEnv) {
	watching(t, env, []WatchTarget{{Path: "a/b/c/AGENTS.md"}}, func(w *watchSession) {
		w.expectChange("a/b/c/AGENTS.md", func() { write(t, env, "a/b/c/AGENTS.md", "x") })
	})
}

func conformWatchNewDirectories(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.CreateDir(background(), "skills", nil))
	watching(t, env, []WatchTarget{{Path: "skills", Recursive: true}}, func(w *watchSession) {
		// Written before any watcher on the new directories can exist.
		w.expectChange("skills/a/b/SKILL.md", func() { write(t, env, "skills/a/b/SKILL.md", "one") })
		w.expectChange("skills/a/b/SKILL.md", func() { write(t, env, "skills/a/b/SKILL.md", "two!") })
		w.expectChange("skills/a/b/c/SKILL.md", func() { write(t, env, "skills/a/b/c/SKILL.md", "deeper") })
	})
}

func conformWatchParentRenamed(t *testing.T, env *RemoteExecutionEnv) {
	write(t, env, "proj/.pi/skills/x.md", "x")
	watching(t, env, []WatchTarget{{Path: "proj/.pi/skills", Recursive: true}}, func(w *watchSession) {
		w.expectChange("proj/.pi/skills", func() { mustDo(t, env.RenameFile(background(), "proj/.pi", "proj/old")) })
		w.expectChange("proj/.pi/skills/y.md", func() { write(t, env, "proj/.pi/skills/y.md", "y") })
		w.expectChange("proj/.pi/skills/y.md", func() { write(t, env, "proj/.pi/skills/y.md", "yy") })
	})
}

func conformWatchExcluded(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.CreateDir(background(), "skills", nil))
	targets := []WatchTarget{{Path: "skills", Recursive: true, Exclude: WatchExclude{Hidden: true, Names: []string{"node_modules"}}}}
	watching(t, env, targets, func(w *watchSession) {
		write(t, env, "skills/node_modules/dep/SKILL.md", "dep")
		write(t, env, "skills/.SKILL.md.tmp", "draft")
		w.expectChange("skills/SKILL.md", func() {
			mustDo(t, env.RenameFile(background(), "skills/.SKILL.md.tmp", "skills/SKILL.md"))
		})
		hidden := []string{w.absolute("skills/node_modules"), w.absolute("skills/.SKILL.md.tmp")}
		for _, change := range w.all() {
			if change.Kind != FileChangePaths {
				continue
			}
			for _, path := range change.Paths {
				for _, excluded := range hidden {
					if path == excluded || strings.HasPrefix(path, excluded) {
						t.Fatalf("excluded %s", path)
					}
				}
			}
		}
	})
}

func conformWatchOverlap(t *testing.T, env *RemoteExecutionEnv) {
	write(t, env, "skills/a/one.md", "one")
	watching(t, env, []WatchTarget{{Path: "skills"}, {Path: "skills", Recursive: true}}, func(w *watchSession) {
		w.expectChange("skills/a/two.md", func() { write(t, env, "skills/a/two.md", "two") })
	})
}

func conformWatchReplacedDirectory(t *testing.T, env *RemoteExecutionEnv) {
	write(t, env, "skills/a/x.md", "x")
	watching(t, env, []WatchTarget{{Path: "skills", Recursive: true}}, func(w *watchSession) {
		w.expectChange("skills/a", func() {
			mustDo(t, env.RenameFile(background(), "skills/a", "skills-old"))
			mustDo(t, env.CreateDir(background(), "skills/a", nil))
		})
		w.expectChange("skills/a/y.md", func() { write(t, env, "skills/a/y.md", "y") })
		w.expectChange("skills/a/y.md", func() { write(t, env, "skills/a/y.md", "yy") })
	})
}

func conformWatchStopped(t *testing.T, env *RemoteExecutionEnv) {
	var mu sync.Mutex
	var changes []FileChange
	watcher, err := env.Watch(background(), []WatchTarget{{Path: "file.txt"}}, func(change FileChange) {
		mu.Lock()
		changes = append(changes, change)
		mu.Unlock()
	})
	mustDo(t, err)
	if mode := watcher.Mode(); mode != "native" && mode != "polling" {
		t.Fatalf("mode %q", mode)
	}
	watcher.Stop()
	watcher.Stop()
	write(t, env, "file.txt", "x")
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 0 {
		t.Fatalf("changes after stop: %+v", changes)
	}
}

func conformArgv(t *testing.T, env *RemoteExecutionEnv) {
	hostile := "it's $(touch pwned) `touch pwned` *; touch pwned"
	result, err, stdout, _ := execCollect(env, withShell(`printf "%s|%s" "$1" "$2"`, "argv0", hostile, "a b"), "")
	mustDo(t, err)
	if result.ExitCode != 0 || stdout != hostile+"|a b" {
		t.Fatalf("exit %d, stdout %q", result.ExitCode, stdout)
	}
	if exists, err := env.Exists(background(), "pwned"); err != nil || exists {
		t.Fatalf("pwned exists: %v %v", exists, err)
	}
}

func conformStreams(t *testing.T, env *RemoteExecutionEnv) {
	script := "printf out; printf err >&2; printf more"
	result, err, stdout, stderr := execCollect(env, withShell(script), "")
	mustDo(t, err)
	if result.ExitCode != 0 || stdout != "outmore" || stderr != "err" {
		t.Fatalf("argv: %d %q %q", result.ExitCode, stdout, stderr)
	}
	var out, errOut strings.Builder
	shellResult, err := env.RunShell(background(), script, &RunOptions{OnOutput: collectInto(&out, &errOut)})
	mustDo(t, err)
	if shellResult.ExitCode != 0 || out.String() != "outmore" || errOut.String() != "err" {
		t.Fatalf("string: %d %q %q", shellResult.ExitCode, out.String(), errOut.String())
	}
}

func conformCwdAndExitCodes(t *testing.T, env *RemoteExecutionEnv) {
	mustDo(t, env.CreateDir(background(), "sub", nil))
	result, err, _, _ := execCollect(env, withShell("printf x > made.txt; exit 3"), "sub")
	mustDo(t, err)
	if result.ExitCode != 3 {
		t.Fatalf("exit %d", result.ExitCode)
	}
	if text, err := env.ReadTextFile(background(), "sub/made.txt"); err != nil || text != "x" {
		t.Fatalf("made.txt %q %v", text, err)
	}
}

func conformSpawnErrors(t *testing.T, env *RemoteExecutionEnv) {
	if _, err := env.RunProgram(background(), []string{"pi-durable-conformance-missing-program"}, nil); errorCode(err) != "spawn_error" {
		t.Fatalf("missing program: %v", err)
	}
	if _, err := env.RunProgram(background(), []string{}, nil); errorCode(err) != "spawn_error" {
		t.Fatalf("empty argv: %v", err)
	}
}

func conformWindow(t *testing.T, env *RemoteExecutionEnv) {
	const lines = 2000
	window := &OutputWindow{MaxBytes: 200, MaxLines: 5, MinIntervalMS: 0, BytesPerSecond: 1_000_000_000}
	bytes, newlines := int64(0), int64(0)
	tail := ""
	result, err := env.RunProgram(background(), withShell("i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done"), &RunOptions{
		Window: window,
		OnOutput: func(text string, info OutputInfo) error {
			if info.Skipped != nil {
				bytes += info.Skipped.Bytes
				newlines += info.Skipped.Newlines
				if !(int64(len(text)) > window.MaxBytes || int64(strings.Count(text, "\n")) > window.MaxLines) {
					t.Errorf("a skip is followed by %q, not more than the window", text)
				}
				tail = ""
			}
			bytes += int64(len(text))
			newlines += int64(strings.Count(text, "\n"))
			tail += text
			return nil
		},
	})
	mustDo(t, err)
	var expected strings.Builder
	var last []string
	for i := 0; i < lines; i++ {
		line := "line-" + itoa(i) + "\n"
		expected.WriteString(line)
		if i >= lines-int(window.MaxLines) {
			last = append(last, line)
		}
	}
	if result.ExitCode != 0 || bytes != int64(expected.Len()) || newlines != lines || !strings.HasSuffix(tail, strings.Join(last, "")) {
		t.Fatalf("exit %d, %d bytes, %d newlines, tail %q", result.ExitCode, bytes, newlines, tail)
	}
}

func itoa(i int) string { return jstext.NumberToString(float64(i)) }

func conformTimeoutAndAbort(t *testing.T, env *RemoteExecutionEnv) {
	if _, err := env.RunProgram(background(), withShell("sleep 2"), &RunOptions{Timeout: 0.1}); errorCode(err) != "timeout" {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := env.RunProgram(ctx, withShell("sleep 2"), nil); errorCode(err) != "aborted" {
		t.Fatalf("abort: %v", err)
	}
}

func conformReaderSymlinks(t *testing.T, env *RemoteExecutionEnv) {
	write(t, env, "target.txt", "target")
	mustDo(t, env.CreateDir(background(), "sub", nil))
	write(t, env, "sub/inner.txt", "inner")
	linked, err := env.RunProgram(background(), withShell("ln -s target.txt link.txt && ln -s sub dirlink"), nil)
	if err != nil || linked.ExitCode != 0 {
		t.Fatalf("ln: %+v %v", linked, err)
	}
	readAll := func(path string, options *BinaryReaderOptions) string {
		reader, err := env.OpenBinaryReader(background(), path, options)
		mustDo(t, err)
		defer reader.Close()
		data, err := reader.Read(background(), 0, 10)
		mustDo(t, err)
		return string(data)
	}
	if got := readAll("link.txt", nil); got != "target" {
		t.Fatalf("followed %q", got)
	}
	if _, err := env.OpenBinaryReader(background(), "link.txt", &BinaryReaderOptions{NoFollow: true}); errorCode(err) != "invalid" {
		t.Fatalf("noFollow on a link: %v", err)
	}
	// Only the final component is refused; earlier symlinked directories still
	// resolve.
	if got := readAll("dirlink/inner.txt", &BinaryReaderOptions{NoFollow: true}); got != "inner" {
		t.Fatalf("through a linked directory %q", got)
	}
}

func conformWatchSymlink(t *testing.T, env *RemoteExecutionEnv) {
	write(t, env, "data/real.md", "one")
	mustDo(t, env.CreateDir(background(), "config", nil))
	linked, err := env.RunProgram(background(), withShell("ln -s ../data/real.md config/AGENTS.md"), nil)
	if err != nil || linked.ExitCode != 0 {
		t.Fatalf("ln: %+v %v", linked, err)
	}
	watching(t, env, []WatchTarget{{Path: "config/AGENTS.md"}}, func(w *watchSession) {
		w.expectChange("config/AGENTS.md", func() { write(t, env, "data/real.md", "two!") })
	})
}

// listingEnv is what the listFiles cases need of an environment.
type listingEnv interface {
	writeFile(t *testing.T, path, content string)
	createDir(t *testing.T, path string)
	shell(t *testing.T, script string) int
	absolutePath(t *testing.T, path string) string
	ListFiles(ctx context.Context, path string, options *ListFilesOptions) (FileListing, error)
}

type remoteListing struct{ *RemoteExecutionEnv }

func (e remoteListing) writeFile(t *testing.T, path, content string) {
	mustDo(t, e.WriteFile(background(), path, []byte(content)))
}
func (e remoteListing) createDir(t *testing.T, path string) {
	mustDo(t, e.CreateDir(background(), path, nil))
}
func (e remoteListing) shell(t *testing.T, script string) int {
	result, err := e.RunProgram(background(), withShell(script), nil)
	mustDo(t, err)
	return result.ExitCode
}
func (e remoteListing) absolutePath(t *testing.T, path string) string {
	absolute, err := e.AbsolutePath(background(), path)
	mustDo(t, err)
	return absolute
}

type localListing struct{ *LocalExecutionEnv }

func (e localListing) writeFile(t *testing.T, path, content string) {
	mustDo(t, e.WriteFile(background(), path, []byte(content)))
}
func (e localListing) createDir(t *testing.T, path string) {
	mustDo(t, os.MkdirAll(filepath.Join(e.Cwd(), path), 0o755))
}
func (e localListing) shell(t *testing.T, script string) int {
	result, err := e.Exec(background(), script, nil)
	mustDo(t, err)
	return result.ExitCode
}
func (e localListing) absolutePath(t *testing.T, path string) string {
	absolute, err := e.AbsolutePath(background(), path)
	mustDo(t, err)
	return absolute
}

// listed is a listing as "<kind letter> <path>" lines.
func listed(t *testing.T, env listingEnv, path string, options *ListFilesOptions) ([]string, bool) {
	t.Helper()
	listing, err := env.ListFiles(background(), path, options)
	mustDo(t, err)
	letter := map[coding.FileKind]string{coding.FileKindFile: "f", coding.FileKindDirectory: "d", coding.FileKindSymlink: "l"}
	entries := []string{}
	for _, entry := range listing.Entries {
		entries = append(entries, letter[entry.Kind]+" "+entry.Path)
	}
	return entries, listing.Truncated
}

func expectListing(t *testing.T, env listingEnv, path string, options *ListFilesOptions, want []string, wantTruncated bool) {
	t.Helper()
	got, truncated := listed(t, env, path, options)
	if !reflect.DeepEqual(got, want) || truncated != wantTruncated {
		t.Fatalf("listing %s: %v (truncated %v), want %v (truncated %v)", path, got, truncated, want, wantTruncated)
	}
}

var listFilesCases = []struct {
	name string
	run  func(t *testing.T, env listingEnv)
}{
	{"listFiles lists files and directories depth first in name order", func(t *testing.T, env listingEnv) {
		for _, file := range []string{"b.txt", "a/z.txt", "a/b/c.txt", ".hidden/x", "A.txt"} {
			env.writeFile(t, file, "")
		}
		env.createDir(t, "empty")
		expectListing(t, env, ".", nil, []string{"d .hidden", "f .hidden/x", "f A.txt", "d a", "d a/b", "f a/b/c.txt", "f a/z.txt", "f b.txt", "d empty"}, false)
		got, _ := listed(t, env, ".", &ListFilesOptions{Hidden: new(false)})
		if want := []string{"f A.txt", "d a", "d a/b", "f a/b/c.txt", "f a/z.txt", "f b.txt", "d empty"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("without hidden: %v", got)
		}
		want := []string{"d b", "f b/c.txt", "f z.txt"}
		expectListing(t, env, "a", nil, want, false)
		expectListing(t, env, env.absolutePath(t, "a"), nil, want, false)
		expectListing(t, env, "empty", nil, []string{}, false)
	}},
	{"listFiles skips what git ignores in work trees, .git, and excluded names", func(t *testing.T, env listingEnv) {
		for _, file := range []string{"keep.txt", "out/x.js", "sub/keep.txt", "sub/drop.log", "node_modules/m/i.js"} {
			env.writeFile(t, file, "")
		}
		env.writeFile(t, ".gitignore", "out/\n*.log\n")
		env.writeFile(t, "sub/.gitignore", "!keep.txt\n")
		// Not a work tree yet: .gitignore files have no effect.
		if got, _ := listed(t, env, ".", nil); !contains(got, "f out/x.js") {
			t.Fatalf("outside a work tree: %v", got)
		}
		env.writeFile(t, ".git/HEAD", "ref: refs/heads/main\n")
		env.writeFile(t, ".git/info/exclude", "keep.txt\n")
		got, _ := listed(t, env, ".", &ListFilesOptions{Exclude: []string{"node_modules"}})
		if want := []string{"f .gitignore", "d sub", "f sub/.gitignore", "f sub/keep.txt"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("in a work tree: %v", got)
		}
		// A directory inside the work tree applies its parents' rules.
		if got, _ := listed(t, env, "sub", nil); !reflect.DeepEqual(got, []string{"f .gitignore", "f keep.txt"}) {
			t.Fatalf("sub: %v", got)
		}
		all, _ := listed(t, env, ".", &ListFilesOptions{Gitignore: new(false)})
		if !contains(all, "f out/x.js") || !contains(all, "f node_modules/m/i.js") || !contains(all, "f keep.txt") {
			t.Fatalf("without gitignore: %v", all)
		}
		for _, entry := range all {
			if strings.Contains(entry, ".git/") || entry == "d .git" {
				t.Fatalf(".git listed: %v", all)
			}
		}
	}},
	{"listFiles stops at its bounds and reports truncated", func(t *testing.T, env listingEnv) {
		for _, file := range []string{"a", "b", "c"} {
			env.writeFile(t, file, "")
		}
		expectListing(t, env, ".", &ListFilesOptions{MaxEntries: new(int64(2))}, []string{"f a", "f b"}, true)
		expectListing(t, env, ".", &ListFilesOptions{MaxEntries: new(int64(3))}, []string{"f a", "f b", "f c"}, false)
		// Each entry counts its path plus two bytes.
		expectListing(t, env, ".", &ListFilesOptions{MaxBytes: new(int64(6))}, []string{"f a", "f b"}, true)
		expectListing(t, env, ".", &ListFilesOptions{MaxEntries: new(int64(0))}, []string{}, true)
		if _, err := env.ListFiles(background(), ".", &ListFilesOptions{MaxEntries: new(int64(-1))}); errorCode(err) != "invalid" {
			t.Fatalf("maxEntries -1: %v", err)
		}
	}},
	{"listFiles fails on missing directories, files, and aborts", func(t *testing.T, env listingEnv) {
		env.writeFile(t, "file.txt", "")
		for _, c := range []struct {
			ctx  context.Context
			path string
			code string
		}{{background(), "missing", "not_found"}, {background(), "file.txt", "not_directory"}, {abortedContext(), ".", "aborted"}} {
			if _, err := env.ListFiles(c.ctx, c.path, nil); errorCode(err) != c.code {
				t.Fatalf("%s: %v, want %s", c.path, err, c.code)
			}
		}
	}},
	{"listFiles lists symbolic links without following them", func(t *testing.T, env listingEnv) {
		env.writeFile(t, "dir/inner.txt", "")
		if code := env.shell(t, "ln -s dir link && ln -s missing dangling && ln -s .. dir/loop"); code != 0 {
			t.Fatalf("ln exit %d", code)
		}
		expectListing(t, env, ".", nil, []string{"l dangling", "d dir", "f dir/inner.txt", "l dir/loop", "l link"}, false)
		// The listed directory itself may be a link.
		expectListing(t, env, "link", nil, []string{"f inner.txt", "l loop"}, false)
	}},
}

func contains(list []string, item string) bool {
	for _, entry := range list {
		if entry == item {
			return true
		}
	}
	return false
}
