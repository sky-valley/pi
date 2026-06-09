//go:build !windows

package coding

import (
	"os/exec"
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
	// Negative pid → signal the process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the child if the group is gone.
		return cmd.Process.Kill()
	}
	return nil
}
