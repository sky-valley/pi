//go:build !windows

package env

import (
	"os/exec"
	"syscall"
)

// processAlive reports whether a process with this pid exists, as Node's
// process.kill(pid, 0) does.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// detach starts the command in a session of its own, as Node's detached
// does, so killing its parent's process group leaves it running.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
