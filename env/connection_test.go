package env

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/coding"
)

// startsGolden is testdata/connection/starts.json: how upstream's TypeScript
// Connection fails to start against misbehaving daemons (fakedaemon.sh) and
// bad commands, how its RemoteExecutionEnv resolves paths against a POSIX and
// a Windows daemon, and how requests fail when the connection is lost
// (capture.mts).
type startsGolden struct {
	Starts []struct {
		Name    string          `json:"name"`
		Outcome json.RawMessage `json:"outcome"`
		Logs    []string        `json:"logs"`
	} `json:"starts"`
	Paths []struct {
		Mode   string          `json:"mode"`
		Cwd    string          `json:"cwd"`
		Input  string          `json:"input"`
		Result json.RawMessage `json:"result"`
	} `json:"paths"`
	AbortedPaths []struct {
		Operation string          `json:"operation"`
		Result    json.RawMessage `json:"result"`
	} `json:"abortedPaths"`
	Lost []struct {
		Name              string          `json:"name"`
		Outcome           json.RawMessage `json:"outcome"`
		Restarted         *bool           `json:"restarted"`
		AfterSilenceLimit *bool           `json:"afterSilenceLimit"`
	} `json:"lost"`
}

func loadStarts(t *testing.T) startsGolden {
	t.Helper()
	data, err := os.ReadFile("testdata/connection/starts.json")
	mustDo(t, err)
	var golden startsGolden
	mustDo(t, json.Unmarshal(data, &golden))
	if len(golden.Starts) == 0 || len(golden.Paths) == 0 || len(golden.AbortedPaths) == 0 || len(golden.Lost) == 0 {
		t.Fatal("testdata/connection/starts.json is incomplete; recapture it with capture.mts")
	}
	return golden
}

// TestConnectionStartsAndFailsLikeUpstream: the Go Connection must fail to
// start as upstream's does.
func TestConnectionStartsAndFailsLikeUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakedaemon.sh needs a POSIX shell")
	}
	golden := loadStarts(t)
	script, err := filepath.Abs("testdata/connection/fakedaemon.sh")
	mustDo(t, err)
	for _, want := range golden.Starts {
		t.Run(want.Name, func(t *testing.T) {
			options := ConnectionOptions{}
			closeFirst := false
			switch want.Name {
			case "missing program":
				options.Command = []string{"/nonexistent/pi-env-missing"}
			case "no command":
				options.Command = []string{}
			case "failing command function":
				options.CommandFunc = func(context.Context) ([]string, error) {
					return nil, errors.New("ssh gpu-box failed: Network is unreachable")
				}
			case "closed":
				options.Command = []string{"sh", script, "noise"}
				closeFirst = true
			default:
				options.Command = []string{"sh", script, want.Name}
			}
			var mu sync.Mutex
			logs := []string{}
			options.OnLog = func(text string) {
				mu.Lock()
				logs = append(logs, text)
				mu.Unlock()
			}
			connection := NewConnection(options)
			if closeFirst {
				connection.Close()
			}
			var outcome any
			info, err := connection.Info(background())
			if err != nil {
				var remote *RemoteError
				if !errors.As(err, &remote) {
					t.Fatalf("not a RemoteError: %#v", err)
				}
				outcome = map[string]any{"code": remote.Code, "message": remote.Message, "lost": IsConnectionLost(err)}
			} else {
				outcome = map[string]any{"info": info}
			}
			time.Sleep(300 * time.Millisecond)
			connection.Close()
			compareJSON(t, "outcome", outcome, want.Outcome)
			if want.Logs != nil {
				mu.Lock()
				defer mu.Unlock()
				if !reflect.DeepEqual(logs, want.Logs) {
					t.Errorf("logs %q, want %q", logs, want.Logs)
				}
			}
		})
	}
}

// TestCallsOnLogOneAtATime: the daemon's shell noise (stdout) and its
// diagnostics (stderr) are read on two goroutines, but OnLog, like upstream's
// onLog on its one thread, never runs twice at once.
func TestCallsOnLogOneAtATime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakedaemon.sh needs a POSIX shell")
	}
	script, err := filepath.Abs("testdata/connection/fakedaemon.sh")
	mustDo(t, err)
	var active, most, calls atomic.Int32
	connection := NewConnection(ConnectionOptions{Command: []string{"sh", script, "chatty"}, OnLog: func(string) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(300 * time.Millisecond)
	}})
	defer connection.Close()
	_, err = connection.Info(background())
	mustDo(t, err)
	waitFor(t, 5*time.Second, "both logs", func() bool { return calls.Load() == 2 })
	if n := most.Load(); n != 1 {
		t.Fatalf("OnLog ran on %d goroutines at once", n)
	}
}

// TestOnLogMayUseTheConnection: upstream's onLog may call into the
// connection (its calls return promises); Go's OnLog, called for the shell
// noise before the daemon's sync line, may wait for the daemon's answers.
func TestOnLogMayUseTheConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakedaemon.sh needs a POSIX shell")
	}
	script, err := filepath.Abs("testdata/connection/fakedaemon.sh")
	mustDo(t, err)
	var connection *Connection
	logged := make(chan error, 2)
	connection = NewConnection(ConnectionOptions{Command: []string{"sh", script, "chatty"}, OnLog: func(string) {
		_, err := connection.Info(background())
		logged <- err
	}})
	defer connection.Close()
	started := make(chan error, 1)
	go func() {
		_, err := connection.Info(background())
		started <- err
	}()
	for _, ch := range []chan error{started, logged, logged} {
		select {
		case err := <-ch:
			mustDo(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the connection hung on an OnLog that asks for Info")
		}
	}
}

func TestRequestFieldsComeBeforeTheOperation(t *testing.T) {
	for _, c := range []struct {
		fields any
		want   string
	}{
		{nil, `{"op":"x"}`},
		{object{}, `{"op":"x"}`},
		{object{}.with("b", 1).with("a", "<&>"), `{"b":1,"a":"<&>","op":"x"}`},
		{map[string]int{"z": 1}, `{"z":1,"op":"x"}`},
	} {
		got, err := requestJSON("x", c.fields)
		if err != nil || string(got) != c.want {
			t.Errorf("requestJSON(%v) = %s, %v; want %s", c.fields, got, err, c.want)
		}
	}
	if _, err := requestJSON("x", []int{1}); err == nil {
		t.Error("an array was accepted as request fields")
	}
}

func TestFileErrorsMatchTheStandardErrors(t *testing.T) {
	notFound := toFileError(&RemoteError{Code: "ENOENT", Message: "ENOENT: no such file or directory, open '/x'"}, "/x")
	if !errors.Is(notFound, os.ErrNotExist) || errors.Is(notFound, os.ErrPermission) || notFound.Code != FileNotFound || notFound.Path != "/x" {
		t.Fatalf("ENOENT: %+v", notFound)
	}
	if denied := toFileError(&RemoteError{Code: "EPERM"}, ""); !errors.Is(denied, os.ErrPermission) {
		t.Fatalf("EPERM: %+v", denied)
	}
	for code, want := range map[string]FileErrorCode{
		"aborted": FileAborted, "EACCES": FilePermissionDenied, "ENOTDIR": FileNotDirectory, "EISDIR": FileIsDirectory,
		"EINVAL": FileInvalid, "SYMLINK": FileInvalid, "NOT_REGULAR": FileInvalid, "EBUSY": FileUnknown,
	} {
		if got := toFileError(&RemoteError{Code: code}, "").Code; got != want {
			t.Errorf("%s: %s, want %s", code, got, want)
		}
	}
	// The coding tools' shell seam classifies aborts and timeouts.
	if !errors.Is(&ExecutionError{Code: ExecAborted}, coding.ErrShellAborted) {
		t.Error("an aborted command is not coding.ErrShellAborted")
	}
	var timeout *coding.ShellTimeoutError
	if !errors.As(&ExecutionError{Code: ExecTimeout, timeout: 2.5}, &timeout) || timeout.Seconds != 2.5 {
		t.Errorf("a timed-out command: %+v", timeout)
	}
}

func TestWatcherReopensAfterALostConnectionAndReportsOverflow(t *testing.T) {
	env, connection := environment(t)
	var mu sync.Mutex
	var changes []WatchChange
	watcher, err := env.Watch(background(), []WatchTarget{{Path: "AGENTS.md"}}, func(change WatchChange) {
		mu.Lock()
		changes = append(changes, change)
		mu.Unlock()
	})
	mustDo(t, err)
	defer watcher.Close()
	info, err := connection.Info(background())
	mustDo(t, err)
	killPID(t, info.PID)
	overflowed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, change := range changes {
			if change.Overflow {
				return true
			}
		}
		return false
	}
	// The first reopen waits a second, then starts the daemon again.
	waitFor(t, 10*time.Second, "an overflow after the reconnect", overflowed)
	mu.Lock()
	changes = nil
	mu.Unlock()
	mustDo(t, env.WriteFile(background(), "AGENTS.md", []byte("x")))
	target, _ := env.AbsolutePath(background(), "AGENTS.md")
	waitFor(t, 5*time.Second, "the reopened watcher to report a change", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, change := range changes {
			if covers(change, target) {
				return true
			}
		}
		return false
	})
}

// TestWatcherClosesFromItsOwnCallback: upstream's close() called inside
// onChange returns at once (its promise settles later); Go's Close must not
// wait for the callback that calls it, and no call follows.
func TestWatcherClosesFromItsOwnCallback(t *testing.T) {
	env, _ := environment(t)
	var watcher *RemoteWatcher
	opened := make(chan struct{})
	closed := make(chan error, 1)
	var calls atomic.Int32
	watcher, err := env.Watch(background(), []WatchTarget{{Path: "AGENTS.md"}}, func(change WatchChange) {
		<-opened
		if calls.Add(1) == 1 {
			closed <- watcher.Close()
		}
	})
	mustDo(t, err)
	close(opened)
	mustDo(t, env.WriteFile(background(), "AGENTS.md", []byte("x")))
	select {
	case err := <-closed:
		mustDo(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close called from onChange did not return")
	}
	mustDo(t, env.WriteFile(background(), "AGENTS.md", []byte("y")))
	time.Sleep(500 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d onChange calls, want 1: the watcher reported changes after it was closed", n)
	}
}

// TestWatcherCallsOnChangeOneAtATimeAcrossAReconnect: upstream's callbacks
// run one at a time (one JavaScript thread), also the overflow a reopened
// watcher reports while the new watch already reports changes.
func TestWatcherCallsOnChangeOneAtATimeAcrossAReconnect(t *testing.T) {
	connection := newConnection(t)
	env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{
		Connection: connection, ID: "pi-env:test", Cwd: tempDir(t, "pi-env-remote-"),
		Watch: RemoteWatchOptions{Mode: "polling", PollIntervalMS: 50},
	})
	var active, most atomic.Int32
	var overflowed, changedAfter atomic.Bool
	watcher, err := env.Watch(background(), []WatchTarget{{Path: "AGENTS.md"}}, func(change WatchChange) {
		n := active.Add(1)
		defer active.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		if change.Overflow {
			overflowed.Store(true)
			time.Sleep(time.Second)
		} else if overflowed.Load() {
			changedAfter.Store(true)
		}
	})
	mustDo(t, err)
	defer watcher.Close()
	info, err := connection.Info(background())
	mustDo(t, err)
	killPID(t, info.PID)
	// Keep changing the file while the watcher reopens (after a second), so
	// the new watch reports changes while the overflow is delivered.
	stop := make(chan struct{})
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			_ = env.WriteFile(background(), "AGENTS.md", []byte(strconv.Itoa(i)))
		}
	}()
	waitFor(t, 10*time.Second, "the overflow and a change after it", func() bool { return overflowed.Load() && changedAfter.Load() })
	close(stop)
	<-writer
	if n := most.Load(); n != 1 {
		t.Fatalf("onChange ran on %d goroutines at once", n)
	}
}

// TestResolvesPathsLikeUpstream compares AbsolutePath and JoinPath with
// upstream's against the same POSIX and Windows hellos: home directories,
// drive-relative paths and their per-drive working directories, UNC paths,
// and file: URLs.
func TestResolvesPathsLikeUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakedaemon.sh needs a POSIX shell")
	}
	golden := loadStarts(t)
	script, err := filepath.Abs("testdata/connection/fakedaemon.sh")
	mustDo(t, err)
	envs := map[string]*RemoteExecutionEnv{}
	for _, want := range golden.Paths {
		env := envs[want.Mode]
		if env == nil {
			connection := NewConnection(ConnectionOptions{Command: []string{"sh", script, want.Mode}})
			t.Cleanup(connection.Close)
			env = NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:paths", Cwd: want.Cwd})
			envs[want.Mode] = env
		}
		var got any
		var resolved string
		var err error
		if want.Input == "join a .. b c/d" {
			resolved, err = env.JoinPath(background(), []string{"a", "..", "b", "c/d"})
		} else {
			resolved, err = env.AbsolutePath(background(), want.Input)
		}
		if err != nil {
			got = map[string]any{"code": errorCode(err)}
		} else {
			got = resolved
		}
		compareJSON(t, want.Mode+" "+want.Input, got, want.Result)
	}
}

// TestResolvesPathsWithAnAbortedContextLikeUpstream: absolutePath and
// joinPath do not look at the context; aborted, they still wait for the
// daemon to start and resolve.
func TestResolvesPathsWithAnAbortedContextLikeUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakedaemon.sh needs a POSIX shell")
	}
	golden := loadStarts(t)
	script, err := filepath.Abs("testdata/connection/fakedaemon.sh")
	mustDo(t, err)
	for _, want := range golden.AbortedPaths {
		connection := NewConnection(ConnectionOptions{Command: []string{"sh", script, "posix"}})
		env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:paths", Cwd: "/work/dir"})
		var resolved string
		switch want.Operation {
		case "absolutePath ~/a":
			resolved, err = env.AbsolutePath(abortedContext(), "~/a")
		case "joinPath a b":
			resolved, err = env.JoinPath(abortedContext(), []string{"a", "b"})
		default:
			t.Fatalf("unknown operation %q", want.Operation)
		}
		connection.Close()
		var got any = resolved
		if err != nil {
			got = map[string]any{"code": errorCode(err)}
		}
		compareJSON(t, want.Operation, got, want.Result)
	}
}

// TestFailsRequestsLikeUpstreamWhenTheConnectionIsLost: a request larger than
// a frame loses the connection (the daemon refuses it and exits), and so does
// a daemon that stops reading and pinging, even while a write to it is
// blocked and the request's context has ended.
func TestFailsRequestsLikeUpstreamWhenTheConnectionIsLost(t *testing.T) {
	golden := loadStarts(t)
	script, err := filepath.Abs("testdata/connection/fakedaemon.sh")
	mustDo(t, err)
	type outcome struct {
		err     error
		elapsed time.Duration
	}
	// settle runs request and waits a bounded time for it to settle.
	settle := func(t *testing.T, request func() error) outcome {
		t.Helper()
		done := make(chan outcome, 1)
		started := time.Now()
		go func() {
			err := request()
			done <- outcome{err, time.Since(started)}
		}()
		select {
		case result := <-done:
			return result
		case <-time.After(10 * time.Second):
			t.Fatal("the request is still pending after 10 s")
			return outcome{}
		}
	}
	remoteOutcome := func(t *testing.T, err error) any {
		t.Helper()
		var remote *RemoteError
		if !errors.As(err, &remote) {
			t.Fatalf("not a RemoteError: %#v", err)
		}
		return map[string]any{"code": remote.Code, "message": remote.Message, "lost": IsConnectionLost(err)}
	}
	for _, want := range golden.Lost {
		t.Run(want.Name, func(t *testing.T) {
			switch want.Name {
			case "request above a frame", "command above a frame":
				connection := newConnection(t)
				env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:lost", Cwd: "/"})
				info, err := connection.Info(background())
				mustDo(t, err)
				var got any
				if want.Name == "request above a frame" {
					result := settle(t, func() error {
						_, err := connection.Request(background(), "lstat", object{}.with("path", "/"), &RequestOptions{Payload: make([]byte, 17*1024*1024)})
						return err
					})
					got = remoteOutcome(t, result.err)
				} else {
					result := settle(t, func() error {
						_, err := env.RunShell(background(), strings.Repeat("x", 17*1024*1024), nil)
						return err
					})
					var failure *ExecutionError
					if !errors.As(result.err, &failure) {
						t.Fatalf("not an ExecutionError: %#v", result.err)
					}
					got = map[string]any{"code": failure.Code, "message": failure.Message}
				}
				compareJSON(t, "outcome", got, want.Outcome)
				again, err := connection.Info(background())
				mustDo(t, err)
				if restarted := again.PID != info.PID; want.Restarted == nil || restarted != *want.Restarted {
					t.Errorf("restarted %v, want %v", restarted, *want.Restarted)
				}
			case "daemon stops reading":
				if runtime.GOOS == "windows" {
					t.Skip("fakedaemon.sh needs a POSIX shell")
				}
				connection := NewConnection(ConnectionOptions{Command: []string{"sh", script, "silent"}})
				t.Cleanup(connection.Close)
				connection.timing = timing{ping: 50 * time.Millisecond, silence: 500 * time.Millisecond}
				_, err := connection.Info(background())
				mustDo(t, err)
				ctx, cancel := context.WithTimeout(background(), 100*time.Millisecond)
				defer cancel()
				result := settle(t, func() error {
					fields := object{}.with("path", "/nonexistent/x").with("append", false).with("parents", false)
					_, err := connection.Request(ctx, "write", fields, &RequestOptions{Payload: make([]byte, 4*1024*1024)})
					return err
				})
				compareJSON(t, "outcome", remoteOutcome(t, result.err), want.Outcome)
				if after := result.elapsed >= connection.timing.silence; want.AfterSilenceLimit == nil || after != *want.AfterSilenceLimit {
					t.Errorf("settled after %v (silence limit %v)", result.elapsed, connection.timing.silence)
				}
			default:
				t.Fatalf("unknown case %q", want.Name)
			}
		})
	}
}
