package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Each tool's Operations seam must actually be consulted — a seam the tool
// ignores is worse than no seam, because it reads as pluggable and is not.

func TestReadToolOperationsAreConsulted(t *testing.T) {
	var readCalls, accessCalls, detectCalls int
	ops := ReadOperations{
		ReadFile: func(_ context.Context, p string) ([]byte, error) {
			readCalls++
			return []byte("injected content\n"), nil
		},
		Access:              func(context.Context, string) error { accessCalls++; return nil },
		DetectImageMimeType: func(context.Context, string) string { detectCalls++; return "" },
	}
	tool := readToolOps("/nowhere", &ops, nil)
	res, err := tool.Execute(context.Background(), "1", map[string]any{"path": "x.txt"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(resultText(res), "injected content") {
		t.Fatalf("injected ReadFile was not used: %q", resultText(res))
	}
	if readCalls == 0 || accessCalls == 0 || detectCalls == 0 {
		t.Fatalf("ops not all consulted: read=%d access=%d detect=%d", readCalls, accessCalls, detectCalls)
	}
}

// pi calls access BEFORE reading, so an access failure must stop the read.
func TestReadToolAccessFailureStopsTheRead(t *testing.T) {
	denied := errors.New("EACCES: permission denied")
	ops := ReadOperations{
		Access: func(context.Context, string) error { return denied },
		ReadFile: func(context.Context, string) ([]byte, error) {
			t.Fatal("ReadFile ran after Access failed")
			return nil, nil
		},
	}
	tool := readToolOps("/nowhere", &ops, nil)
	if _, err := tool.Execute(context.Background(), "1", map[string]any{"path": "x"}, nil); !errors.Is(err, denied) {
		t.Fatalf("err = %v, want the Access error", err)
	}
}

func TestWriteToolOperationsAreConsulted(t *testing.T) {
	var wrotePath, wroteContent, madeDir string
	ops := WriteOperations{
		WriteFile: func(_ context.Context, p, content string) error { wrotePath, wroteContent = p, content; return nil },
		Mkdir:     func(_ context.Context, d string) error { madeDir = d; return nil },
	}
	tool := writeToolOps("/nowhere", &ops)
	if _, err := tool.Execute(context.Background(), "1",
		map[string]any{"path": "sub/new.txt", "content": "hello"}, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.HasSuffix(wrotePath, filepath.Join("sub", "new.txt")) || wroteContent != "hello" {
		t.Fatalf("WriteFile got (%q, %q)", wrotePath, wroteContent)
	}
	if !strings.HasSuffix(madeDir, "sub") {
		t.Fatalf("Mkdir got %q, want the parent directory", madeDir)
	}
	// Nothing reached the real filesystem.
	if _, err := os.Stat("/nowhere"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("injected write must not touch disk (stat = %v)", err)
	}
}

func TestEditToolOperationsAreConsulted(t *testing.T) {
	var wrote string
	ops := EditOperations{
		ReadFile:  func(context.Context, string) ([]byte, error) { return []byte("alpha\nbeta\n"), nil },
		WriteFile: func(_ context.Context, _ string, content string) error { wrote = content; return nil },
		Access:    func(context.Context, string) error { return nil },
	}
	tool := editToolOps("/nowhere", &ops)
	_, err := tool.Execute(context.Background(), "1", map[string]any{
		"path":  "x.txt",
		"edits": []any{map[string]any{"oldText": "beta", "newText": "gamma"}},
	}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if wrote != "alpha\ngamma\n" {
		t.Fatalf("edit wrote %q, want %q", wrote, "alpha\ngamma\n")
	}
}

// pi resolves ops as `options?.operations ?? defaults` — WHOLE-OBJECT
// replacement. Supplying operations replaces ALL of them; there is no
// per-member merge. An earlier draft of this port filled member-by-member,
// which would have given an injected remote filesystem a silent LOCAL fallback
// for anything its author left out. This pins the correct semantics.
func TestOperationsReplaceWholesaleNotPerMember(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("on disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only DetectImageMimeType is supplied. ReadFile and Access are therefore
	// NIL — not local defaults — so the tool must not silently read the disk.
	tool := readToolOps(dir, &ReadOperations{
		DetectImageMimeType: func(context.Context, string) string { return "" },
	}, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("a partial override must NOT fall back to local defaults")
		}
	}()
	_, _ = tool.Execute(context.Background(), "1", map[string]any{"path": "f.txt"}, nil)
}

// nil operations is pi's absent `operations?` and DOES use the defaults.
func TestNilOperationsUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("on disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := readToolOps(dir, nil, nil).Execute(context.Background(), "1",
		map[string]any{"path": "f.txt"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(resultText(res), "on disk") {
		t.Fatalf("nil operations must use the local defaults: %q", resultText(res))
	}
}

func ptr[T any](v T) *T { return &v }

// The local default reproduces Node's EISDIR text, because that emulation
// belongs to the filesystem implementation rather than to the tool.
func TestDefaultReadOperationsReportsEISDIR(t *testing.T) {
	dir := t.TempDir()
	if _, err := DefaultReadOperations().ReadFile(context.Background(), dir); err == nil ||
		!strings.Contains(err.Error(), "EISDIR") {
		t.Fatalf("reading a directory = %v, want pi's EISDIR text", err)
	}
}

// ls and find consult their seams too.
func TestLsToolOperationsAreConsulted(t *testing.T) {
	ops := LsOperations{
		Exists:  func(context.Context, string) (bool, error) { return true, nil },
		Stat:    func(_ context.Context, p string) (bool, error) { return !strings.HasSuffix(p, ".txt"), nil },
		Readdir: func(context.Context, string) ([]string, error) { return []string{"beta.txt", "alpha"}, nil },
	}
	res, err := lsToolOps("/nowhere", &ops).Execute(context.Background(), "1", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := resultText(res)
	// The injected listing is what shows, and Stat decides the trailing slash.
	if !strings.Contains(text, "alpha/") || !strings.Contains(text, "beta.txt") {
		t.Fatalf("injected listing not used: %q", text)
	}
	if _, err := os.Stat("/nowhere"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("injected ls must not touch disk (stat = %v)", err)
	}
}

func TestFindToolExistsIsConsulted(t *testing.T) {
	var asked string
	ops := FindOperations{
		Exists: func(_ context.Context, p string) (bool, error) { asked = p; return false, nil },
	}
	_, err := findToolOps("/nowhere", &ops).Execute(context.Background(), "1",
		map[string]any{"pattern": "*.go"}, nil)
	if err == nil {
		t.Fatal("a missing root must be an error")
	}
	if asked == "" {
		t.Fatal("find never consulted Exists")
	}
}

// --- the shell seam ---

func fakeShellOps(out string, exit *int, err error) BashOperations {
	return BashOperations{
		Exec: func(_ context.Context, _ string, _ string, o BashExecOptions) (*int, error) {
			if o.OnData != nil && out != "" {
				o.OnData([]byte(out))
			}
			return exit, err
		},
	}
}

func TestShellToolUsesInjectedExec(t *testing.T) {
	zero := 0
	ops := fakeShellOps("from the injected shell\n", &zero, nil)
	tool := shellToolOps("/nowhere", bashShellConfig, nil, &ops)
	res, err := tool.Execute(context.Background(), "1", map[string]any{"command": "whatever"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(resultText(res), "from the injected shell") {
		t.Fatalf("injected exec output not used: %q", resultText(res))
	}
}

// pi's ops.exec throws `aborted`; the tool must render "Command aborted" and
// keep whatever output arrived first.
func TestShellToolRendersAbort(t *testing.T) {
	ops := fakeShellOps("partial\n", nil, ErrShellAborted)
	tool := shellToolOps("/nowhere", bashShellConfig, nil, &ops)
	_, err := tool.Execute(context.Background(), "1", map[string]any{"command": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Command aborted") {
		t.Fatalf("err = %v, want Command aborted", err)
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Fatalf("abort must keep the output produced so far: %v", err)
	}
}

// pi's `timeout:<secs>` carries the RAW value, so 0.5 renders "0.5".
func TestShellToolRendersTimeoutWithRawSeconds(t *testing.T) {
	ops := fakeShellOps("", nil, &ShellTimeoutError{Seconds: 0.5})
	tool := shellToolOps("/nowhere", bashShellConfig, nil, &ops)
	_, err := tool.Execute(context.Background(), "1", map[string]any{"command": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Command timed out after 0.5 seconds") {
		t.Fatalf("err = %v, want the raw 0.5", err)
	}
}

// pi a8b3dd199 (#9577) 'should reject a null exit code from custom operations':
// the local shell no longer reports one — a signal termination is 128 + signal —
// so a nil exit code now means a custom BashOperations lost the status, and the
// command FAILS with the partial output preserved (bash.ts:366-368).
func TestShellToolNilExitCodeFails(t *testing.T) {
	ops := fakeShellOps("partial\n", nil, nil)
	tool := shellToolOps("/nowhere", bashShellConfig, nil, &ops)
	_, err := tool.Execute(context.Background(), "1", map[string]any{"command": "x"}, nil)
	if err == nil {
		t.Fatal("a nil exit code must be an error")
	}
	// pi asserts this with /partial\s+Command terminated without an exit code$/.
	want := regexp.MustCompile(`partial\s+Command terminated without an exit code$`)
	if got := err.Error(); !want.MatchString(got) {
		t.Fatalf("nil-exit error text %q does not match %v", got, want)
	}
}

func TestShellToolNonZeroExitIsReported(t *testing.T) {
	three := 3
	ops := fakeShellOps("oops\n", &three, nil)
	tool := shellToolOps("/nowhere", bashShellConfig, nil, &ops)
	res, err := tool.Execute(context.Background(), "1", map[string]any{"command": "x"}, nil)
	if err != nil || !res.IsError || resultText(res) != "oops\n\n\nCommand exited with code 3" {
		t.Fatalf("got %+v, err %v, want an error result reporting exit code 3", res, err)
	}
}

// --- the grep seam ---

func TestGrepToolUsesInjectedOperations(t *testing.T) {
	ops := GrepOperations{
		IsDirectory: func(context.Context, string) (bool, error) { return false, nil },
		ReadFile: func(context.Context, string) ([]byte, error) {
			return []byte("alpha\nNEEDLE here\ngamma\n"), nil
		},
	}
	res, err := grepToolOps("/nowhere", &ops).Execute(context.Background(), "1",
		map[string]any{"pattern": "NEEDLE", "path": "f.txt"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(resultText(res), "NEEDLE here") {
		t.Fatalf("injected grep content not searched: %q", resultText(res))
	}
	if _, err := os.Stat("/nowhere"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("injected grep must not touch disk (stat = %v)", err)
	}
}

// LocalBashOperations must still be the real thing: this runs a command.
func TestLocalBashOperationsRunsAndReportsExit(t *testing.T) {
	if _, _, _, err := getShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ops := LocalBashOperations(bashShellConfig)
	var got strings.Builder
	code, err := ops.Exec(context.Background(), "printf 'hey'; exit 7", t.TempDir(), BashExecOptions{
		OnData: func(p []byte) { got.Write(p) },
		Env:    os.Environ(),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code == nil || *code != 7 {
		t.Fatalf("exit code = %v, want 7", code)
	}
	if got.String() != "hey" {
		t.Fatalf("streamed %q, want %q", got.String(), "hey")
	}
}

// pi a8b3dd199 (#9577) 'should map signal-killed commands to 128 plus the
// signal number': the local shell reports a signal termination with the
// conventional 128 + signal rather than pi's old null exit code.
func TestLocalBashOperationsSignalExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX signals")
	}
	if _, _, _, err := getShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ops := LocalBashOperations(bashShellConfig)
	for _, tt := range []struct {
		signal string
		want   int
	}{
		{signal: "KILL", want: 137},
		{signal: "TERM", want: 143},
	} {
		t.Run(tt.signal, func(t *testing.T) {
			code, err := ops.Exec(context.Background(), "kill -"+tt.signal+" $$", t.TempDir(), BashExecOptions{
				Env: os.Environ(),
			})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if code == nil || *code != tt.want {
				t.Fatalf("exit code = %v, want %d", code, tt.want)
			}
		})
	}
}

// D68: a signal Node cannot NAME. pi reads Node's close event, where such a
// signal arrives as code 0 / signal null, so pi's `code ?? …` short-circuits to
// 0 and the bash tool reports a killed command as successful. The port reads the
// raw wait status and keeps 128 + the signal number, which is pi's stated intent
// (a8b3dd199: "so callers do not mistake the termination for a successful
// command"). Pinned so the divergence cannot be flipped back toward pi's
// accident without a deliberate edit.
//
// Signal 7 is the measured gap on darwin (SIGEMT, absent from Node's 31-entry
// table); on Linux 7 is SIGBUS and Node names it, while the gap moves to the
// realtime signals. The Go answer is 135 either way, because it never consults
// a name table — which is the whole point.
func TestLocalBashOperationsUnnamedSignalStillFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX signals")
	}
	if _, _, _, err := getShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ops := LocalBashOperations(bashShellConfig)
	code, err := ops.Exec(context.Background(), "kill -7 $$", t.TempDir(), BashExecOptions{Env: os.Environ()})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code == nil || *code != 135 {
		t.Fatalf("exit code = %v, want 135 (128+7); pi would report 0 here on darwin", code)
	}
}

// Abort must win over timeout when both fired — pi's precedence.
func TestLocalBashOperationsAbortWinsOverTimeout(t *testing.T) {
	if _, _, _, err := getShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ops := LocalBashOperations(bashShellConfig)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := ops.Exec(ctx, "sleep 5", t.TempDir(), BashExecOptions{
		TimeoutSeconds: 0.2, // would also fire
		Env:            os.Environ(),
	})
	if !errors.Is(err, ErrShellAborted) {
		t.Fatalf("err = %v, want ErrShellAborted to win over the timeout", err)
	}
}

func TestLocalBashOperationsTimeout(t *testing.T) {
	if _, _, _, err := getShellConfig(""); err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ops := LocalBashOperations(bashShellConfig)
	_, err := ops.Exec(context.Background(), "sleep 5", t.TempDir(), BashExecOptions{
		TimeoutSeconds: 0.2,
		Env:            os.Environ(),
	})
	var te *ShellTimeoutError
	if !errors.As(err, &te) || te.Seconds != 0.2 {
		t.Fatalf("err = %v, want *ShellTimeoutError{0.2}", err)
	}
}

// The precedence in classifyShellRun is the contract, and it is NOT reachable
// from a live-process test: making a real command both abort and time out is a
// race, so such a test passes whichever way the branches are ordered — verified
// by mutation, which is why this table exists.
func TestClassifyShellRunPrecedence(t *testing.T) {
	canceled := context.Canceled
	deadline := context.DeadlineExceeded

	// Both fired: abort must win.
	if _, err := classifyShellRun(canceled, deadline, nil, 2); !errors.Is(err, ErrShellAborted) {
		t.Fatalf("abort+timeout = %v, want abort to win", err)
	}
	// Timeout alone.
	var te *ShellTimeoutError
	_, err := classifyShellRun(nil, deadline, nil, 0.5)
	if !errors.As(err, &te) || te.Seconds != 0.5 {
		t.Fatalf("timeout alone = %v, want *ShellTimeoutError{0.5}", err)
	}
	// Abort alone.
	if _, err := classifyShellRun(canceled, nil, nil, 0); !errors.Is(err, ErrShellAborted) {
		t.Fatalf("abort alone = %v", err)
	}
	// Clean exit.
	if code, err := classifyShellRun(nil, nil, nil, 0); err != nil || code == nil || *code != 0 {
		t.Fatalf("clean run = (%v, %v), want (0, nil)", code, err)
	}
	// A spawn failure that is not an ExitError passes through untouched.
	boom := errors.New("exec: not found")
	if _, err := classifyShellRun(nil, nil, boom, 0); !errors.Is(err, boom) {
		t.Fatalf("spawn failure = %v, want it passed through", err)
	}
	// Abort is checked BEFORE the exit status: a non-zero exit alongside an
	// abort must still report the abort.
	if _, err := classifyShellRun(canceled, nil, errors.New("exit status 1"), 0); !errors.Is(err, ErrShellAborted) {
		t.Fatalf("abort with a failing exit = %v, want abort", err)
	}
}
