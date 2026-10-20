package coding

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeEnv is an in-memory ExecutionEnv. Its existence is the point of the seam:
// before ExecutionEnv the read tool could only be tested against a real
// filesystem.
type fakeEnv struct {
	dir   string
	files map[string][]byte
	execs []string
}

func newFakeEnv(dir string) *fakeEnv {
	return &fakeEnv{dir: dir, files: map[string][]byte{}}
}

func (f *fakeEnv) Cwd() string { return f.dir }

func (f *fakeEnv) AbsolutePath(_ context.Context, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(f.dir, path), nil
}
func (f *fakeEnv) JoinPath(_ context.Context, parts []string) (string, error) {
	return filepath.Join(parts...), nil
}
func (f *fakeEnv) ReadBinaryFile(_ context.Context, path string) ([]byte, error) {
	if data, ok := f.files[path]; ok {
		return data, nil
	}
	// A real filesystem raises EISDIR for a directory; so does this one, which
	// is what lets the read tool report pi's text without stat-ing local disk.
	prefix := strings.TrimSuffix(path, "/") + "/"
	for p := range f.files {
		if strings.HasPrefix(p, prefix) {
			return nil, errors.New("EISDIR: illegal operation on a directory, read")
		}
	}
	return nil, os.ErrNotExist
}
func (f *fakeEnv) ReadTextFile(ctx context.Context, path string) (string, error) {
	data, err := f.ReadBinaryFile(ctx, path)
	return string(data), err
}
func (f *fakeEnv) OpenTextLineReader(ctx context.Context, path string) (TextLineReader, error) {
	text, err := f.ReadTextFile(ctx, path)
	if err != nil {
		return nil, err
	}
	return newTextLineReader(io.NopCloser(strings.NewReader(text))), nil
}

// Shares the real reader, so the double cannot drift from LocalEnv on the
// line-counting question this file pins.
func (f *fakeEnv) ReadTextLines(ctx context.Context, path string, maxLines int) ([]string, error) {
	reader, err := f.OpenTextLineReader(ctx, path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var lines []string
	for maxLines <= 0 || len(lines) < maxLines {
		line, err := reader.ReadLine(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, line.Text)
	}
	return lines, nil
}
func (f *fakeEnv) WriteFile(_ context.Context, path string, content []byte) error {
	f.files[path] = content
	return nil
}
func (f *fakeEnv) AppendFile(_ context.Context, path string, content []byte) error {
	f.files[path] = append(f.files[path], content...)
	return nil
}
func (f *fakeEnv) RenameFile(_ context.Context, src, dst string) error {
	data, ok := f.files[src]
	if !ok {
		return os.ErrNotExist
	}
	f.files[dst] = data
	delete(f.files, src)
	return nil
}
func (f *fakeEnv) Stat(_ context.Context, path string) (FileInfo, error) {
	if data, ok := f.files[path]; ok {
		return FileInfo{Path: path, Name: filepath.Base(path), Kind: FileKindFile, Size: int64(len(data))}, nil
	}
	// Any path that is a prefix of a known file is a directory.
	for p := range f.files {
		if strings.HasPrefix(p, strings.TrimSuffix(path, "/")+"/") {
			return FileInfo{Path: path, Name: filepath.Base(path), Kind: FileKindDirectory}, nil
		}
	}
	return FileInfo{}, os.ErrNotExist
}
func (f *fakeEnv) ListDir(_ context.Context, path string) ([]FileInfo, error) {
	var out []FileInfo
	prefix := strings.TrimSuffix(path, "/") + "/"
	for p, data := range f.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, FileInfo{Path: p, Name: filepath.Base(p), Kind: FileKindFile, Size: int64(len(data))})
		}
	}
	return out, nil
}
func (f *fakeEnv) CanonicalPath(_ context.Context, path string) (string, error) { return path, nil }
func (f *fakeEnv) Exists(_ context.Context, path string) (bool, error) {
	_, ok := f.files[path]
	return ok, nil
}
func (f *fakeEnv) Exec(_ context.Context, command string, _ *ShellExecOptions) (ShellResult, error) {
	f.execs = append(f.execs, command)
	return ShellResult{Stdout: "fake:" + command}, nil
}
func (f *fakeEnv) Cleanup() error { return nil }

var _ ExecutionEnv = (*fakeEnv)(nil)

// The seam's whole purpose: the read tool reads from wherever the env says,
// with no filesystem involved.
func TestReadToolUsesInjectedEnv(t *testing.T) {
	dir := "/virtual/project"
	env := newFakeEnv(dir)
	env.files[filepath.Join(dir, "hello.txt")] = []byte("line one\nline two\n")

	tool := readToolOps(env.Cwd(), ptr(EnvReadOperations(env)), nil)
	res, err := tool.Execute(context.Background(), "1",
		map[string]any{"path": "hello.txt"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, "line one") || !strings.Contains(text, "line two") {
		t.Fatalf("read tool did not return the injected content, got:\n%s", text)
	}
	// Nothing was written to disk: /virtual does not exist.
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fake env must not touch the real filesystem (stat %s = %v)", dir, err)
	}
}

// A directory must report pi's EISDIR text through the env's FileKind rather
// than through os.FileInfo.IsDir.
func TestReadToolEnvRejectsDirectory(t *testing.T) {
	dir := "/virtual/project"
	env := newFakeEnv(dir)
	env.files[filepath.Join(dir, "sub", "a.txt")] = []byte("x")

	tool := readToolOps(env.Cwd(), ptr(EnvReadOperations(env)), nil)
	_, err := tool.Execute(context.Background(), "1", map[string]any{"path": "sub"}, nil)
	if err == nil || !strings.Contains(err.Error(), "EISDIR") {
		t.Fatalf("reading a directory must report EISDIR, got %v", err)
	}
}

// LocalEnv must behave exactly as the tool did before the seam existed —
// otherwise the refactor changed behavior rather than relocating it.
func TestReadToolLocalEnvMatchesDirectRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	viaWrapper := readTool(dir)
	viaEnv := readToolOps(dir, ptr(EnvReadOperations(NewLocalEnv(dir))), nil)

	a, err := viaWrapper.Execute(context.Background(), "1", map[string]any{"path": "file.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := viaEnv.Execute(context.Background(), "1", map[string]any{"path": "file.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resultText(a) != resultText(b) {
		t.Fatalf("LocalEnv diverged from the direct read:\n--- wrapper ---\n%s\n--- env ---\n%s",
			resultText(a), resultText(b))
	}
}

func TestLocalEnvFileSystemRoundTrip(t *testing.T) {
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()

	if err := env.WriteFile(ctx, "nested/deep/f.txt", []byte("one\ntwo\nthree\n")); err != nil {
		t.Fatalf("WriteFile must create parent directories: %v", err)
	}
	if got, err := env.ReadTextFile(ctx, "nested/deep/f.txt"); err != nil || got != "one\ntwo\nthree\n" {
		t.Fatalf("ReadTextFile = %q, %v", got, err)
	}
	if lines, err := env.ReadTextLines(ctx, "nested/deep/f.txt", 2); err != nil || len(lines) != 2 {
		t.Fatalf("ReadTextLines(maxLines=2) = %v, %v", lines, err)
	}
	if err := env.AppendFile(ctx, "nested/deep/f.txt", []byte("four\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.ReadTextFile(ctx, "nested/deep/f.txt"); !strings.HasSuffix(got, "four\n") {
		t.Fatalf("AppendFile did not append: %q", got)
	}

	info, err := env.Stat(ctx, "nested/deep/f.txt")
	if err != nil || info.Kind != FileKindFile || info.Name != "f.txt" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if dirInfo, err := env.Stat(ctx, "nested/deep"); err != nil || dirInfo.Kind != FileKindDirectory {
		t.Fatalf("Stat on a directory = %+v, %v", dirInfo, err)
	}

	if ok, err := env.Exists(ctx, "nested/deep/f.txt"); err != nil || !ok {
		t.Fatalf("Exists on a present file = %v, %v", ok, err)
	}
	// A missing path is (false, nil) — not an error. This is pi's contract and
	// the distinction callers depend on.
	if ok, err := env.Exists(ctx, "nope.txt"); err != nil || ok {
		t.Fatalf("Exists on a missing file = %v, %v; want false, nil", ok, err)
	}

	if err := env.RenameFile(ctx, "nested/deep/f.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := env.Exists(ctx, "nested/deep/f.txt"); ok {
		t.Fatal("RenameFile left the source behind")
	}

	entries, err := env.ListDir(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if !slicesContains(names, "moved.txt") {
		t.Fatalf("ListDir = %v, want it to contain moved.txt", names)
	}

	// A relative path resolves against Cwd, an absolute one does not.
	if got, _ := env.AbsolutePath(ctx, "x.txt"); got != filepath.Join(dir, "x.txt") {
		t.Fatalf("AbsolutePath(relative) = %q", got)
	}
	if got, _ := env.AbsolutePath(ctx, "/tmp/x.txt"); got != filepath.Clean("/tmp/x.txt") {
		t.Fatalf("AbsolutePath(absolute) = %q", got)
	}
}

func TestLocalEnvExec(t *testing.T) {
	if _, err := GetShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()

	var streamed strings.Builder
	res, err := env.Exec(ctx, "printf 'hi'", &ShellExecOptions{
		OnStdout: func(chunk string) { streamed.WriteString(chunk) },
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hi" || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v, want stdout \"hi\" exit 0", res)
	}
	if streamed.String() != "hi" {
		t.Fatalf("OnStdout saw %q, want %q", streamed.String(), "hi")
	}

	// A non-zero exit is a RESULT, not an error — pi reports exitCode in the
	// success arm of its Result.
	res, err = env.Exec(ctx, "exit 3", nil)
	if err != nil {
		t.Fatalf("a failing command must not be an error, got %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}

	// Explicit env entries override the inherited ambient value.
	t.Setenv("PI_EXECENV_PROBE", "ambient")
	res, err = env.Exec(ctx, "printf '%s' \"$PI_EXECENV_PROBE\"", &ShellExecOptions{
		Env: map[string]string{"PI_EXECENV_PROBE": "explicit"},
	})
	if err != nil || res.Stdout != "explicit" {
		t.Fatalf("explicit env must win, got %q (%v)", res.Stdout, err)
	}
}

// A process killed by a signal reports pi's conventional 128 + signal number,
// not success and not Go's -1 (upstream c2d3dc55b, pi#8992). Skipped on
// Windows exactly as upstream skips it on win32.
//
// The command signals `$$` — the shell we wait on — rather than its process
// group, so this exercises the wait status of the child itself and never
// touches killProcessTree, which only runs when the context cancels. SIGKILL
// pins upstream's own golden of 137; SIGTERM is here so the assertion is the
// 128 + signum arithmetic rather than a single memorized constant.
func TestLocalEnvExecSignalKilledExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals; upstream skips this case on win32")
	}
	if _, err := GetShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	env := NewLocalEnv(t.TempDir())

	tests := []struct {
		name    string
		command string
		want    int
	}{
		{name: "SIGKILL", command: "kill -9 $$", want: 128 + 9},
		{name: "SIGTERM", command: "kill -15 $$", want: 128 + 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := env.Exec(context.Background(), tt.command, nil)
			if err != nil {
				t.Fatalf("a signal-killed command is a result, not an error: %v", err)
			}
			if res.ExitCode != tt.want {
				t.Fatalf("ExitCode = %d, want %d", res.ExitCode, tt.want)
			}
		})
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// pi's harness readTextLines yields one entry per LINE: a newline-terminated
// file ends after its last line, with no phantom empty entry. Both of pi's
// implementations agree on that — the readline loop it used through
// `96617628e`, and the strict-LF `NodeTextLineReader` that replaced it in
// `3e4bc2680` (then packages/agent/src/harness/env/nodejs.ts, now
// packages/durable/src/env/node.ts).
//
// The port had reused splitLines here, whose trailing-empty element is the
// READ TOOL's line counting (`content.split("\n")`), not readTextLines'. The
// two are different functions and the shared helper hid it.
func TestLocalEnvReadTextLinesHasNoPhantomTrailingLine(t *testing.T) {
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()
	if err := env.WriteFile(ctx, "f.txt", []byte("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	lines, err := env.ReadTextLines(ctx, "f.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "one" || lines[1] != "two" {
		t.Fatalf("ReadTextLines = %#v, want [one two]", lines)
	}
}

// The counterpart: an unterminated final line is still a line.
func TestLocalEnvReadTextLinesKeepsUnterminatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()
	if err := env.WriteFile(ctx, "f.txt", []byte("one\ntwo")); err != nil {
		t.Fatal(err)
	}
	lines, err := env.ReadTextLines(ctx, "f.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[1] != "two" {
		t.Fatalf("ReadTextLines = %#v, want [one two]", lines)
	}
}

// Transliterated from pi's own packages/agent/test/harness/text-line-reader.test.ts
// at 3e4bc2680 — the commit that introduced the reader. 7fd478a2e deleted that
// file, and pi's line-reader cases now live in
// packages/durable/test/env-node.test.ts. The cases pi chose are
// the contract: what counts as a line, what "terminated" means, and how the
// decoder behaves at a chunk boundary and on malformed input.
func TestTextLineReaderMatchesPiContract(t *testing.T) {
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()

	read := func(t *testing.T, content []byte) []TextLine {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "text.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		reader, err := env.OpenTextLineReader(ctx, "text.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		var lines []TextLine
		for {
			line, err := reader.ReadLine(ctx)
			if errors.Is(err, io.EOF) {
				return lines
			}
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
	}

	t.Run("unicode, blank lines and a torn final line", func(t *testing.T) {
		got := read(t, []byte("hé🙂\n\n\n終\ntorn"))
		want := []TextLine{
			{Text: "hé🙂", Terminated: true},
			{Text: "", Terminated: true},
			{Text: "", Terminated: true},
			{Text: "終", Terminated: true},
			{Text: "torn", Terminated: false},
		}
		if len(got) != len(want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("line %d = %#v, want %#v", i, got[i], want[i])
			}
		}
	})

	t.Run("empty file", func(t *testing.T) {
		if got := read(t, nil); len(got) != 0 {
			t.Fatalf("got %#v, want no lines", got)
		}
	})

	t.Run("multibyte split across the 64 KiB chunk boundary", func(t *testing.T) {
		first := strings.Repeat("a", 64*1024-1) + "🙂" + strings.Repeat("é", 40_000)
		got := read(t, []byte(first+"\n終"))
		if len(got) != 2 || got[0].Text != first || !got[0].Terminated {
			t.Fatalf("first line did not survive the chunk boundary (len %d)", len(got))
		}
		if got[1] != (TextLine{Text: "終"}) {
			t.Fatalf("second line = %#v, want {終 false}", got[1])
		}
	})

	// pi decodes with a non-fatal TextDecoder, so a bad byte and a truncated
	// sequence each become U+FFFD rather than reaching the caller raw.
	t.Run("malformed and incomplete UTF-8 are replaced", func(t *testing.T) {
		got := read(t, []byte{0xff, 0x0a, 0xe2, 0x82})
		want := []TextLine{{Text: "�", Terminated: true}, {Text: "�"}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	// pi: "\r" is NOT stripped -- the reader is strict-LF by design, because
	// Node's readline could not report final-line termination.
	t.Run("a CR is part of the line", func(t *testing.T) {
		got := read(t, []byte("one\r\ntwo\r\n"))
		if len(got) != 2 || got[0].Text != "one\r" || got[1].Text != "two\r" {
			t.Fatalf("got %#v, want the CRs preserved", got)
		}
	})
}

// pi: a pre-aborted context must not consume the buffered line, and close is
// idempotent while a read after close is an error.
func TestTextLineReaderCancellationAndClose(t *testing.T) {
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := env.OpenTextLineReader(ctx, "text.txt")
	if err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadLine(ctx); err != nil || line.Text != "one" {
		t.Fatalf("first line = %#v, %v", line, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ReadLine(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must fail the read, got %v", err)
	}
	if line, err := reader.ReadLine(ctx); err != nil || line.Text != "two" {
		t.Fatalf("the cancelled read must not consume a line: got %#v, %v", line, err)
	}

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close must be idempotent, got %v", err)
	}
	if _, err := reader.ReadLine(ctx); err == nil {
		t.Fatal("a read after close must fail")
	}
}

// A missing file is an error at open, not at the first read.
func TestTextLineReaderMissingFile(t *testing.T) {
	env := NewLocalEnv(t.TempDir())
	if _, err := env.OpenTextLineReader(context.Background(), "missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opening a missing file = %v, want os.ErrNotExist", err)
	}
}

// maxLines stops the read; pi's loop does the same rather than reading the
// whole file and truncating.
func TestLocalEnvReadTextLinesStopsAtMaxLines(t *testing.T) {
	dir := t.TempDir()
	env := NewLocalEnv(dir)
	ctx := context.Background()
	if err := env.WriteFile(ctx, "f.txt", []byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}
	lines, err := env.ReadTextLines(ctx, "f.txt", 2)
	if err != nil || len(lines) != 2 || lines[1] != "two" {
		t.Fatalf("ReadTextLines(maxLines=2) = %#v, %v", lines, err)
	}
}
