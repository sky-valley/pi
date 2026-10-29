//go:build !windows

package env

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Ported from upstream's packages/env/test/remote.test.ts (b6d5c2ecd), which
// skips it on win32: the daemon used to die on Connection.close()'s SIGTERM
// before killing its commands.
func TestKillsRunningCommandsWhenTheConnectionCloses(t *testing.T) {
	env, connection := environment(t)
	command, running := startSleeper(t, env)
	connection.Close()
	settle(t, running)
	expectGone(t, command)
}

// Connection.Close ends the daemon's stdin before its SIGTERM, and a daemon
// that sees its input end stops everything it started anyway, so the test
// above passes on a daemon that dies of the signal whenever the end wins the
// race. Here the signal comes alone, with the connection still open: each of
// SIGTERM, SIGINT and SIGHUP kills the daemon's commands (upstream b6d5c2ecd).
func TestKillsRunningCommandsWhenTheDaemonIsSignalled(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			env, connection := environment(t)
			command, running := startSleeper(t, env)
			info, err := connection.Info(background())
			mustDo(t, err)
			mustDo(t, syscall.Kill(info.PID, sig))
			settle(t, running)
			expectGone(t, command)
		})
	}
}

// startSleeper runs a 30-second sleep on env and returns its pid once it
// printed it, and the channel its RunProgram's error arrives on.
func startSleeper(t *testing.T, env *RemoteExecutionEnv) (int, <-chan error) {
	t.Helper()
	pids := make(chan int, 1)
	running := make(chan error, 1)
	go func() {
		_, err := env.RunProgram(background(), []string{"sh", "-c", "echo $$; exec sleep 30"}, &RunOptions{
			OnOutput: func(text string, _ OutputInfo) error {
				if pid, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
					select {
					case pids <- pid:
					default:
					}
				}
				return nil
			},
		})
		running <- err
	}()
	select {
	case pid := <-pids:
		return pid, running
	case <-time.After(10 * time.Second):
		t.Fatal("the command never printed its pid")
		return 0, nil
	}
}

// settle waits for the command's RunProgram to return.
func settle(t *testing.T, running <-chan error) {
	t.Helper()
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("RunProgram never settled")
	}
}

// expectGone fails unless pid is gone within 5 seconds.
func expectGone(t *testing.T, pid int) {
	t.Helper()
	alive := func() bool { return syscall.Kill(pid, 0) == nil }
	for deadline := time.Now().Add(5 * time.Second); alive() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if alive() {
		t.Fatalf("command %d survived", pid)
	}
}
