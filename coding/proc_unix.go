//go:build !windows

package coding

import (
	"os/exec"
	"runtime"
	"syscall"
)

// setProcessGroup puts the command in its own process group so the whole tree
// (including backgrounded grandchildren) can be signalled together.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree kills the command's entire process group (port of pi's
// killProcessTree). The child is the group leader, so signalling -pid reaches
// every descendant, not just the direct child.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return killProcessGroup(runtime.GOOS, cmd.Process.Pid, syscall.Kill, cmd.Process.Kill)
}

// killProcessGroup is killProcessTree's signalling, with the platform, kill(2)
// and the single-process fallback passed in so each path is testable.
//
// On macOS the group is stopped first (upstream 27cc6bfc0, #11252): macOS
// signals group members one by one after releasing the group lock, so a
// killed child can wake its waiting shell before the shell is signalled,
// letting `a; b` start b during cancellation. Stopping the group narrows that
// race; it is best-effort, not an atomic freeze, and a failed stop does not
// prevent the kill. SIGKILL also kills a stopped process. Linux holds the
// tasklist lock across group signalling, so the shell is signalled before it
// can observe the child's exit, and is left as it was.
func killProcessGroup(goos string, pid int, kill func(pid int, sig syscall.Signal) error, killChild func() error) error {
	if goos == "darwin" {
		_ = kill(-pid, syscall.SIGSTOP)
	}
	// Negative pid → signal the process group.
	if err := kill(-pid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the child if the group is gone.
		return killChild()
	}
	return nil
}
