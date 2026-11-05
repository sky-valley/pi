package coding

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Upstream 06aa6563e (pi test/suite/regressions/11613-bash-timeout-after-shell-exit.test.ts):
// the bash tool waits for its output pipe to close, not only for the shell to
// exit, so a background process that inherited the pipe and keeps writing
// keeps the tool waiting. When the timeout then hits, the tool reports the
// shell's real exit code and that it had already finished. Unix only, as in
// pi: on Windows the deadline cannot reach background processes once the
// shell is gone.
func TestBashTimeoutAfterShellExitReportsTheExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the deadline cannot kill background processes once the shell pid is gone")
	}
	start := time.Now()
	_, err := run(t, bashTool(t.TempDir(), nil), map[string]any{
		"command": "(while true; do echo tick; sleep 0.02; done) & sleep 0.2; echo boom >&2; exit 3",
		"timeout": 1.0,
	})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Errorf("the background writer outlived the deadline: %v", elapsed)
	}
	msg := err.Error()
	if !regexp.MustCompile(`boom`).MatchString(msg) {
		t.Errorf("output lost: %q", msg)
	}
	want := regexp.MustCompile(`Command timed out after 1 seconds\. The shell exited with code 3 after 0\.\d seconds, but background processes it started kept writing output until the timeout\.$`)
	if !want.MatchString(msg) {
		t.Errorf("message = %q", msg[max(0, len(msg)-200):])
	}
}

func TestBashTimeoutWhileShellRunsKeepsThePlainMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	_, err := run(t, bashTool(t.TempDir(), nil), map[string]any{"command": "echo started; sleep 30", "timeout": 0.3})
	if err == nil || !regexp.MustCompile(`started\s+Command timed out after 0\.3 seconds$`).MatchString(err.Error()) {
		t.Errorf("err = %v", err)
	}
}

// The classification itself: the exit is carried only when the shell exited
// before the deadline, with its code and Math.round(ms / 100) / 10 seconds.
func TestClassifyShellRunCarriesTheShellExit(t *testing.T) {
	var te *ShellTimeoutError
	after := 250 * time.Millisecond
	_, err := classifyShellRun(nil, errDeadline, exitStatus(t, 3), 1, &after)
	if !errors.As(err, &te) || te.ShellExit == nil || *te.ShellExit != (ShellExit{ExitCode: 3, AfterSeconds: 0.3}) {
		t.Fatalf("got %v (%+v)", err, te)
	}
	_, err = classifyShellRun(nil, errDeadline, nil, 1, nil)
	if !errors.As(err, &te) || te.ShellExit != nil {
		t.Fatalf("a shell still running at the deadline carries no exit: %+v", te)
	}
	// An exit reported without a deadline is an ordinary result.
	if code, err := classifyShellRun(nil, nil, exitStatus(t, 3), 1, &after); err != nil || code == nil || *code != 3 {
		t.Fatalf("got (%v, %v)", code, err)
	}
}

var errDeadline = context.DeadlineExceeded

// exitStatus is the *exec.ExitError a real process exiting with code gives.
func exitStatus(t *testing.T, code int) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh exit %d: %v", code, err)
	}
	return err
}
