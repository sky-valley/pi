//go:build windows

package coding

import (
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows (process groups work differently; pi
// only sets detached on non-win32 and relies on taskkill /T for the tree).
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills the command's entire process tree via taskkill, matching
// pi's killProcessTree on win32: `taskkill /F /T /PID <pid>`. /T terminates the
// process and any child processes it started; /F forces termination.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run(); err != nil {
		// Fall back to killing just the child if taskkill is unavailable.
		return cmd.Process.Kill()
	}
	return nil
}
