//go:build !windows

package daemon

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// runProcessProgram is the test binary as a program the process tests run
// (`<test binary> pi-env-process-program <mode> [args]`), by mode:
//
//   - spawn-descendant <file>: starts `descendant <file>` in a session of its
//     own, so the program's group kill does not reach it, inheriting stdin;
//     exits after 300 ms.
//   - descendant <file>: 1 s after its parent, the program, exited, reads
//     stdin until its end and writes how many bytes it read to file; exits
//     with 1 after 15 s.
func runProcessProgram(args []string) int {
	switch args[0] {
	case "spawn-descendant":
		self, err := os.Executable()
		if err != nil {
			return 1
		}
		descendant := exec.Command(self, processProgramArg, "descendant", args[1])
		descendant.Stdin = os.Stdin
		descendant.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := descendant.Start(); err != nil {
			return 1
		}
		time.Sleep(300 * time.Millisecond)
	case "descendant":
		time.AfterFunc(15*time.Second, func() { os.Exit(1) })
		// Only after the program has exited (a race-enabled program sleeps
		// a second before it exits), and the daemon had time to stop its
		// writer.
		for parent := os.Getppid(); os.Getppid() == parent; {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(time.Second)
		length, _ := io.Copy(io.Discard, os.Stdin)
		if os.WriteFile(args[1]+".tmp", []byte(strconv.FormatInt(length, 10)), 0o644) != nil ||
			os.Rename(args[1]+".tmp", args[1]) != nil {
			return 1
		}
	default:
		return 2
	}
	return 0
}
