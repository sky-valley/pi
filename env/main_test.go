package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// testDaemon is the daemon the tests run: PI_ENV_DAEMON (the Rust daemon of
// @earendil-works/pi-env 1.3.0, say, to check the client against it), else
// the Go daemon built from ./cmd/pi-env for the tests.
var testDaemon string

// oldDaemonArg makes the test binary a stand-in for a daemon released before
// hello.features (upstream's test/old-daemon.ts).
const oldDaemonArg = "pi-env-old-daemon"

// listingDaemonArg makes the test binary a daemon that lists fixed,
// unsorted directory entries: `<test binary> pi-env-listing-daemon <os>`.
const listingDaemonArg = "pi-env-listing-daemon"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == scriptedDaemonArg {
		os.Exit(runScriptedDaemon(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == oldDaemonArg {
		os.Exit(runOldDaemon(os.Args[2:]))
	}
	if len(os.Args) > 2 && os.Args[1] == listingDaemonArg {
		os.Exit(runListingDaemon(os.Args[2], os.Args[3:]))
	}
	testDaemon = os.Getenv("PI_ENV_DAEMON")
	var dir string
	if testDaemon == "" {
		var err error
		dir, err = os.MkdirTemp("", "pi-env-client-test-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		testDaemon = filepath.Join(dir, "pi-env")
		if runtime.GOOS == "windows" {
			testDaemon += ".exe"
		}
		build := exec.Command("go", "build", "-o", testDaemon, "github.com/sky-valley/pi/env/cmd/pi-env")
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "building the daemon:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// runOldDaemon answers hello without features and refuses every other
// operation as an unknown one, as such a daemon refuses listFiles.
func runOldDaemon(args []string) int {
	token := ""
	for i, arg := range args {
		if arg == "--token" && i+1 < len(args) {
			token = args[i+1]
		}
	}
	fmt.Fprintf(os.Stdout, "PI-ENV %s\n", token)
	for {
		f, err := frame.Read(os.Stdin)
		if err != nil {
			return 0
		}
		if f.Kind != frame.Request {
			continue
		}
		var request struct {
			Op string `json:"op"`
		}
		_ = json.Unmarshal(f.JSON, &request)
		var reply frame.Frame
		if request.Op == "hello" {
			hello, _ := json.Marshal(map[string]any{
				"protocol": 1, "version": "1.1.0", "os": "linux", "arch": "x86_64", "home": "/", "separator": "/",
				"tmpdir": "/tmp", "cwd": "/", "driveCwds": map[string]string{}, "pid": os.Getpid(),
			})
			reply = frame.Frame{Kind: frame.Result, ID: f.ID, JSON: hello}
		} else {
			failure, _ := json.Marshal(map[string]string{"code": "EINVAL", "message": "unknown operation " + request.Op})
			reply = frame.Frame{Kind: frame.Error, ID: f.ID, JSON: failure}
		}
		if err := frame.Write(os.Stdout, reply); err != nil {
			return 0
		}
	}
}

// listingEntries are the readdir entries of the listing daemon's
// directories, by the last path component, in the daemon's order.
var listingEntries = map[string][]map[string]any{
	"sorted": {
		listingEntry("b"), listingEntry("a"),
		{"name": "\ufffd", "raw": []int{0xff}, "info": listingInfo("\ufffd")},
		listingEntry("B"),
	},
	"vanishing": {listingEntry("x"), {"name": "gone", "error": map[string]any{"code": "ENOENT", "message": "ENOENT: no such file or directory, lstat"}}, listingEntry("y")},
	"denied":    {listingEntry("x"), {"name": "secret", "error": map[string]any{"code": "EACCES", "message": "EACCES: permission denied, lstat"}}},
	// Closing the handle of "exiting" ends the daemon without an answer.
	"exiting": {},
}

func listingInfo(name string) map[string]any {
	return map[string]any{"name": name, "kind": "file", "size": 1, "mtimeSec": 0, "mtimeNsec": 0}
}

func listingEntry(name string) map[string]any {
	return map[string]any{"name": name, "info": listingInfo(name)}
}

// runListingDaemon answers hello for a system (linux or windows), opendir of
// the directories of listingEntries, readdir with all their entries, and
// close (exiting instead for the directory "exiting").
func runListingDaemon(system string, args []string) int {
	token := ""
	for i, arg := range args {
		if arg == "--token" && i+1 < len(args) {
			token = args[i+1]
		}
	}
	fmt.Fprintf(os.Stdout, "PI-ENV %s\n", token)
	handles := map[float64]string{}
	for {
		f, err := frame.Read(os.Stdin)
		if err != nil {
			return 0
		}
		if f.Kind != frame.Request {
			continue
		}
		var request struct {
			Op     string  `json:"op"`
			Path   string  `json:"path"`
			Handle float64 `json:"handle"`
		}
		_ = json.Unmarshal(f.JSON, &request)
		var answer any
		switch request.Op {
		case "hello":
			home, separator := "/", "/"
			if system == "windows" {
				home, separator = "C:\\", "\\"
			}
			answer = map[string]any{"protocol": 1, "version": "0.0.0", "os": system, "arch": "x86_64", "home": home,
				"separator": separator, "tmpdir": home, "cwd": home, "driveCwds": map[string]string{}, "pid": os.Getpid()}
		case "opendir":
			handle := float64(len(handles) + 1)
			name := request.Path[strings.LastIndexAny(request.Path, "/\\")+1:]
			handles[handle] = name
			answer = map[string]any{"handle": handle}
		case "readdir":
			answer = map[string]any{"entries": listingEntries[handles[request.Handle]], "done": true}
		case "close":
			if handles[request.Handle] == "exiting" {
				return 0
			}
			answer = map[string]any{}
		default:
			answer = map[string]any{}
		}
		reply, _ := json.Marshal(answer)
		if err := frame.Write(os.Stdout, frame.Frame{Kind: frame.Result, ID: f.ID, JSON: reply}); err != nil {
			return 0
		}
	}
}

// newConnection starts nothing; the test closes it.
func newConnection(t *testing.T) *Connection {
	t.Helper()
	connection := NewConnection(ConnectionOptions{Command: []string{testDaemon}})
	t.Cleanup(connection.Close)
	return connection
}

// tempDir is a fresh directory, with symlinks resolved so paths compare.
func tempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// environment is a RemoteExecutionEnv on a fresh connection in a fresh
// directory.
func environment(t *testing.T) (*RemoteExecutionEnv, *Connection) {
	t.Helper()
	connection := newConnection(t)
	return NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: tempDir(t, "pi-env-remote-")}), connection
}

func background() context.Context { return context.Background() }

func abortedContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// errorCode is the code of a FileError or ExecutionError, "" for nil.
func errorCode(err error) string {
	switch e := err.(type) {
	case nil:
		return ""
	case *FileError:
		return string(e.Code)
	case *ExecutionError:
		return string(e.Code)
	case *RemoteError:
		return e.Code
	}
	return fmt.Sprintf("%T: %v", err, err)
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killPID kills a process as SIGKILL does.
func killPID(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, process.Kill())
}

// compareJSON compares got, as JSON, with a captured JSON value.
func compareJSON(t *testing.T, what string, got any, want json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(got)
	mustDo(t, err)
	var gotValue, wantValue any
	_ = json.Unmarshal(data, &gotValue)
	_ = json.Unmarshal(want, &wantValue)
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("%s:\n got %s\nwant %s", what, data, want)
	}
}
