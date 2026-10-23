//go:build !windows

package coding

import (
	"errors"
	"reflect"
	"syscall"
	"testing"
)

// upstream shell-kill-process-tree.test.ts (27cc6bfc0, #11252): stop-before-kill
// is macOS-only and best-effort, and the fallback still kills the shell.
func TestKillProcessGroupSignals(t *testing.T) {
	type call struct {
		pid int
		sig syscall.Signal
	}
	run := func(goos string, fail func(pid int, sig syscall.Signal) bool) []call {
		var calls []call
		kill := func(pid int, sig syscall.Signal) error {
			calls = append(calls, call{pid, sig})
			if fail != nil && fail(pid, sig) {
				return errors.New("failed")
			}
			return nil
		}
		killChild := func() error { return kill(1234, syscall.SIGKILL) }
		_ = killProcessGroup(goos, 1234, kill, killChild)
		return calls
	}
	for _, tc := range []struct {
		name string
		goos string
		fail func(pid int, sig syscall.Signal) bool
		want []call
	}{
		{"stops the macOS process group before killing it without resuming it", "darwin", nil,
			[]call{{-1234, syscall.SIGSTOP}, {-1234, syscall.SIGKILL}}},
		{"still kills the group when stopping fails", "darwin",
			func(pid int, sig syscall.Signal) bool { return sig == syscall.SIGSTOP },
			[]call{{-1234, syscall.SIGSTOP}, {-1234, syscall.SIGKILL}}},
		{"falls back to killing the shell when the group kill fails", "darwin",
			func(pid int, sig syscall.Signal) bool { return pid < 0 && sig == syscall.SIGKILL },
			[]call{{-1234, syscall.SIGSTOP}, {-1234, syscall.SIGKILL}, {1234, syscall.SIGKILL}}},
		{"tolerates failure of both group signals and the fallback", "darwin",
			func(int, syscall.Signal) bool { return true },
			[]call{{-1234, syscall.SIGSTOP}, {-1234, syscall.SIGKILL}, {1234, syscall.SIGKILL}}},
		{"keeps Linux group termination unchanged", "linux", nil,
			[]call{{-1234, syscall.SIGKILL}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.goos, tc.fail); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("signals = %v, want %v", got, tc.want)
			}
		})
	}
}
