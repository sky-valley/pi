package env

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// Ported from upstream's packages/env/test/remote-abort.test.ts (c96e368e2):
// aborts and timeouts against a remote host that does not answer settle on
// time, whatever the host does. The daemons are scenarios of
// runScriptedDaemon instead of `node -e` scripts, and the grace after a
// cancel is shortened (timing.abortGrace) where upstream waits its second.

// runNeverReadyDaemon is a "daemon" that never gets ready, like ssh to a host
// that is down: no sync line, no exit.
func runNeverReadyDaemon() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	time.Sleep(time.Minute)
	return 0
}

// runSilentDaemon answers hello and then nothing, like one behind a
// connection that died unnoticed (a rebooting host): it neither answers
// requests nor exits.
func runSilentDaemon(args []string) int {
	token := ""
	for i, arg := range args {
		if arg == "--token" && i+1 < len(args) {
			token = args[i+1]
		}
	}
	os.Stdout.Write([]byte("PI-ENV " + token + "\n"))
	answered := false
	for {
		f, err := frame.Read(os.Stdin)
		if err != nil {
			time.Sleep(time.Minute)
			return 0
		}
		if f.Kind != frame.Request || answered {
			continue
		}
		answered = true
		reply := frame.Frame{Kind: frame.Result, ID: f.ID}
		reply.JSON, _ = json.Marshal(map[string]any{
			"protocol": 1, "version": "test", "os": "linux", "arch": "x86_64", "home": "/", "tmpdir": "/tmp",
			"separator": "/", "cwd": "/", "driveCwds": map[string]string{}, "pid": os.Getpid(),
		})
		_ = frame.Write(os.Stdout, reply)
	}
}

// testGrace is the grace after a cancel in these tests.
const testGrace = 200 * time.Millisecond

// unanswering is a connection to one of the daemons above, connected already
// for the silent one.
func unanswering(t *testing.T, scenario string) *Connection {
	t.Helper()
	connection := NewConnection(ConnectionOptions{Command: []string{os.Args[0], scriptedDaemonArg, scenario}})
	t.Cleanup(connection.Close)
	connection.timing.abortGrace = testGrace
	if scenario == "silent" {
		_, err := connection.Info(background())
		mustDo(t, err)
	}
	return connection
}

// abortAfter is a context that ends after d.
func abortAfter(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithCancel(background())
	timer := time.AfterFunc(d, cancel)
	t.Cleanup(func() {
		timer.Stop()
		cancel()
	})
	return ctx
}

// timed runs f and returns how long it took and its error; it fails the test
// if f is still running after limit.
func timed(t *testing.T, limit time.Duration, f func() error) (time.Duration, error) {
	t.Helper()
	type outcome struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan outcome, 1)
	started := time.Now()
	go func() {
		err := f()
		done <- outcome{time.Since(started), err}
	}()
	select {
	case result := <-done:
		return result.elapsed, result.err
	case <-time.After(limit):
		t.Fatalf("still waiting after %v", limit)
		return 0, nil
	}
}

func remoteCode(err error) string {
	var remote *RemoteError
	if !errors.As(err, &remote) {
		return errorCode(err)
	}
	return remote.Code
}

func TestAbortGraceIsASecond(t *testing.T) {
	connection := NewConnection(ConnectionOptions{})
	defer connection.Close()
	if connection.timing.abortGrace != time.Second {
		t.Fatalf("abort grace %v, want 1s", connection.timing.abortGrace)
	}
}

func TestAbortStopsWaitingForAStartThatDoesNotFinishAtOnce(t *testing.T) {
	connection := unanswering(t, "never")
	elapsed, err := timed(t, 3*time.Second, func() error {
		_, err := connection.Request(abortAfter(t, 100*time.Millisecond), "lstat", object{}.with("path", "/"), nil)
		return err
	})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "aborted" || remote.Message != "aborted" {
		t.Fatalf("request: %#v, want RemoteError aborted", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("request took %v", elapsed)
	}
	elapsed, err = timed(t, 3*time.Second, func() error {
		_, err := connection.Info(abortAfter(t, 100*time.Millisecond))
		return err
	})
	if !errors.As(err, &remote) || remote.Code != "aborted" || remote.Message != "aborted" {
		t.Fatalf("info: %#v, want RemoteError aborted", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("info took %v", elapsed)
	}
}

func TestAbortSettlesASentRequestTheDaemonDoesNotAnswerAfterTheGrace(t *testing.T) {
	connection := unanswering(t, "silent")
	elapsed, err := timed(t, 3*time.Second, func() error {
		_, err := connection.Request(abortAfter(t, 100*time.Millisecond), "exec", object{}.with("command", "true"), nil)
		return err
	})
	if code := remoteCode(err); code != "aborted" {
		t.Fatalf("%v (%q), want aborted", err, code)
	}
	if elapsed < 100*time.Millisecond+testGrace || elapsed >= 100*time.Millisecond+testGrace+time.Second {
		t.Fatalf("settled after %v, want about %v", elapsed, 100*time.Millisecond+testGrace)
	}
}

func TestExecOnAHostThatDoesNotAnswer(t *testing.T) {
	for _, daemon := range []struct{ name, scenario string }{
		{"connection that does not start", "never"},
		{"daemon that stopped answering", "silent"},
	} {
		t.Run("ends at the timeout: "+daemon.name, func(t *testing.T) {
			env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: unanswering(t, daemon.scenario), ID: "pi-env:test", Cwd: "/"})
			_, err := timed(t, 3*time.Second, func() error {
				_, err := env.RunShell(background(), "uptime", &RunOptions{Timeout: 0.2})
				return err
			})
			var failure *ExecutionError
			if !errors.As(err, &failure) || failure.Code != ExecTimeout || failure.Message != "timeout:0.2" {
				t.Fatalf("%#v, want timeout:0.2", err)
			}
		})
		t.Run("ends when aborted: "+daemon.name, func(t *testing.T) {
			env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: unanswering(t, daemon.scenario), ID: "pi-env:test", Cwd: "/"})
			_, err := timed(t, 3*time.Second, func() error {
				_, err := env.RunShell(abortAfter(t, 100*time.Millisecond), "uptime", &RunOptions{Timeout: 30})
				return err
			})
			var failure *ExecutionError
			if !errors.As(err, &failure) || failure.Code != ExecAborted || failure.Message != "aborted" {
				t.Fatalf("%#v, want aborted", err)
			}
		})
	}
	// The timeout also covers resolving the working directory, which waits
	// for the connection.
	t.Run("ends at the timeout while resolving its cwd", func(t *testing.T) {
		env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: unanswering(t, "never"), ID: "pi-env:test", Cwd: "/"})
		_, err := timed(t, 3*time.Second, func() error {
			_, err := env.RunShell(background(), "uptime", &RunOptions{Timeout: 0.2, Cwd: "sub"})
			return err
		})
		if code := errorCode(err); code != "timeout" {
			t.Fatalf("%v (%q), want timeout", err, code)
		}
	})
}

func TestFileOperationsWaitingForTheConnectionEndWhenAborted(t *testing.T) {
	env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: unanswering(t, "never"), ID: "pi-env:test", Cwd: "/"})
	operations := map[string]func(ctx context.Context) error{
		"ReadTextFile": func(ctx context.Context) error {
			_, err := env.ReadTextFile(ctx, "/etc/hostname")
			return err
		},
		"AbsolutePath": func(ctx context.Context) error {
			_, err := env.AbsolutePath(ctx, "a")
			return err
		},
		"CreateTempDir": func(ctx context.Context) error {
			_, err := env.CreateTempDir(ctx, "")
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			elapsed, err := timed(t, 3*time.Second, func() error { return operation(abortAfter(t, 100*time.Millisecond)) })
			if code := errorCode(err); code != "aborted" {
				t.Fatalf("%v (%q), want aborted", err, code)
			}
			if elapsed >= time.Second {
				t.Fatalf("took %v", elapsed)
			}
		})
	}
}
