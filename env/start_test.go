package env

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// Upstream's test/connection.test.ts (479d6e5e2): how a start fails, with the
// daemon's stderr, and how a ready daemon's exit fails its requests. Its
// daemons are `node -e` scripts; here they are the test binary
// (scriptedDaemonArg), which needs no node.

// scriptedDaemonArg makes the test binary a daemon that runs one scenario:
// `<test binary> pi-env-scripted-daemon <scenario> <args...> serve --token <hex>`.
const scriptedDaemonArg = "pi-env-scripted-daemon"

// runScriptedDaemon runs a scenario:
//   - stderr <base64> <code>: writes the bytes to stderr and exits with code
//     (upstream's failing()).
//   - split <acknowledged> <base64> <at>: writes the bytes before at, waits
//     for the file acknowledged (3 s at most, then exits 99), writes the rest
//     and exits 7.
//   - fake <result|error|exit>: speaks the sync line and framing; the hello gets
//     a result, or the error {code: EINVAL, message: "hello refused", path:
//     "/daemon", extra: 1}; the next request makes it exit while a process it
//     started keeps stdout and stderr open for 3 s (upstream's fakeDaemon()).
//   - linger <first> <second>: writes first to stderr and exits 5, while a
//     process it started writes second to the same stderr half a second
//     later.
//   - sleep <ms> [text]: writes text to stderr after ms (a process the
//     daemon starts).
func runScriptedDaemon(args []string) int {
	switch args[0] {
	case "stderr":
		data, _ := base64.StdEncoding.DecodeString(args[1])
		os.Stderr.Write(data)
		code, _ := strconv.Atoi(args[2])
		return code
	case "split":
		data, _ := base64.StdEncoding.DecodeString(args[2])
		at, _ := strconv.Atoi(args[3])
		os.Stderr.Write(data[:at])
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(args[1]); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return 99
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Stderr.Write(data[at:])
		return 7
	case "fake":
		return runFakeDaemon(args[1], args[2:])
	case "linger":
		os.Stderr.Write([]byte(args[1]))
		child := exec.Command(os.Args[0], scriptedDaemonArg, "sleep", "500", args[2])
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			return 98
		}
		return 5
	case "sleep":
		ms, _ := strconv.Atoi(args[1])
		time.Sleep(time.Duration(ms) * time.Millisecond)
		if len(args) > 2 {
			os.Stderr.Write([]byte(args[2]))
		}
		return 0
	}
	return 97
}

func runFakeDaemon(helloReply string, args []string) int {
	token := ""
	for i, arg := range args {
		if arg == "--token" && i+1 < len(args) {
			token = args[i+1]
		}
	}
	os.Stdout.Write([]byte("PI-ENV " + token + "\n"))
	requests := 0
	for {
		f, err := frame.Read(os.Stdin)
		if err != nil {
			return 0
		}
		if f.Kind != frame.Request {
			continue
		}
		if requests++; requests == 1 {
			reply := frame.Frame{Kind: frame.Result, ID: f.ID}
			reply.JSON, _ = json.Marshal(map[string]any{
				"protocol": 1, "version": "test", "os": "linux", "arch": "x86_64", "home": "/", "tmpdir": "/tmp",
				"separator": "/", "cwd": "/", "driveCwds": map[string]string{}, "pid": os.Getpid(),
			})
			if helloReply == "error" {
				reply.Kind = frame.Error
				reply.JSON = []byte(`{"code":"EINVAL","message":"hello refused","path":"/daemon","extra":1}`)
			}
			_ = frame.Write(os.Stdout, reply)
			if helloReply == "exit" {
				// Exit right after the hello, leaving a process that holds
				// stdout and stderr open.
				child := exec.Command(os.Args[0], scriptedDaemonArg, "sleep", "3000")
				child.Stdout, child.Stderr = os.Stdout, os.Stderr
				_ = child.Start()
				return 0
			}
			continue
		}
		child := exec.Command(os.Args[0], scriptedDaemonArg, "sleep", "3000")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		_ = child.Start()
		return 0
	}
}

// scripted is a connection whose daemon runs a scenario of
// runScriptedDaemon.
func scripted(t *testing.T, onLog func(string), scenario ...string) *Connection {
	t.Helper()
	connection := NewConnection(ConnectionOptions{
		Command: append([]string{os.Args[0], scriptedDaemonArg}, scenario...),
		OnLog:   onLog,
	})
	t.Cleanup(connection.Close)
	return connection
}

// failing is the scenario that writes stderr and exits with code.
func failing(stderr string, code int) []string {
	return []string{"stderr", base64.StdEncoding.EncodeToString([]byte(stderr)), strconv.Itoa(code)}
}

func startError(t *testing.T, connection *Connection) *RemoteError {
	t.Helper()
	_, err := connection.Info(background())
	if err == nil {
		t.Fatal("the start did not fail")
	}
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("not a RemoteError: %#v", err)
	}
	return remote
}

// logs collects OnLog's texts.
type logs struct {
	mu    sync.Mutex
	texts []string
}

func (l *logs) add(text string) {
	l.mu.Lock()
	l.texts = append(l.texts, text)
	l.mu.Unlock()
}

func (l *logs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.texts...)
}

// waitJoined waits for OnLog, which runs on a goroutine of its own, to have
// received want.
func (l *logs) waitJoined(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Join(l.all(), "") != want {
		if time.Now().After(deadline) {
			t.Fatalf("logged %q, want %q", strings.Join(l.all(), ""), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartErrorIncludesTheEndOfStderr(t *testing.T) {
	err := startError(t, scripted(t, nil, failing("ssh: Permission denied (publickey).\n", 255)...))
	if !IsConnectionLost(err) {
		t.Error("not lost")
	}
	if want := "pi-env exited with code 255 before it was ready\nremote stderr:\nssh: Permission denied (publickey)."; err.Message != want {
		t.Fatalf("message %q, want %q", err.Message, want)
	}
}

func TestStartErrorKeepsTheMessageWithoutPrintableStderr(t *testing.T) {
	if got := startError(t, scripted(t, nil, failing("", 3)...)).Message; got != "pi-env exited with code 3 before it was ready" {
		t.Errorf("no stderr: %q", got)
	}
	if got := startError(t, scripted(t, nil, failing(" \n\x07\x1b[0m\n", 3)...)).Message; got != "pi-env exited with code 3 before it was ready" {
		t.Errorf("blank stderr: %q", got)
	}
}

func TestStartErrorDropsEscapesAndControlCharacters(t *testing.T) {
	stderr := "\x1b[31mred\x1b[0m\r\nbell\x07 del\x7f\tc1:\xc2\x85\xc2\x9d end\n\xc3\x9cberpr\xc3\xbcfung fehlgeschlagen \xe2\x9c\x97\n\n"
	want := "pi-env exited with code 1 before it was ready\nremote stderr:\nred\nbell del\tc1: end\n\xc3\x9cberpr\xc3\xbcfung fehlgeschlagen \xe2\x9c\x97"
	if got := startError(t, scripted(t, nil, failing(stderr, 1)...)).Message; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
}

func TestStartErrorKeepsTheLast20Lines(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, "line "+strconv.Itoa(i))
	}
	want := "pi-env exited with code 1 before it was ready\nremote stderr:\n" + strings.Join(lines[10:], "\n")
	if got := startError(t, scripted(t, nil, failing(strings.Join(lines, "\n")+"\n", 1)...)).Message; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
}

func TestStartErrorKeepsTheLast2000Characters(t *testing.T) {
	message := startError(t, scripted(t, nil, failing("first\n"+strings.Repeat("x", 5000)+"\n", 1)...)).Message
	_, tail, _ := strings.Cut(message, "\nremote stderr:\n")
	if tail != strings.Repeat("x", 1999) {
		t.Fatalf("tail of %d characters: %.40q", len(tail), tail)
	}
}

func TestStartPassesStderrToOnLogUnchanged(t *testing.T) {
	var logged logs
	startError(t, scripted(t, logged.add, failing("\x1b[31mred\x1b[0m\n", 1)...))
	logged.waitJoined(t, "\x1b[31mred\x1b[0m\n")
}

// TestStartDecodesACharacterSplitAcrossStderrReads (#10716): the daemon writes
// the rest of the character once OnLog had the first read.
func TestStartDecodesACharacterSplitAcrossStderrReads(t *testing.T) {
	for _, character := range []string{"\xc3\xa9", "\xe9\x94\x99", "\xf0\x9f\x98\x80"} {
		t.Run(character, func(t *testing.T) {
			acknowledged := filepath.Join(tempDir(t, "pi-env-stderr-"), "logged")
			stderr := "prefix " + character + " suffix\n"
			var logged logs
			connection := scripted(t, func(text string) {
				logged.add(text)
				if _, err := os.Stat(acknowledged); err != nil {
					_ = os.WriteFile(acknowledged, nil, 0o644)
				}
			}, "split", acknowledged, base64.StdEncoding.EncodeToString([]byte(stderr)), strconv.Itoa(len("prefix ")+1))
			err := startError(t, connection)
			logged.waitJoined(t, stderr)
			if n := len(logged.all()); n < 2 {
				t.Errorf("%d OnLog calls, want two or more", n)
			}
			if want := "pi-env exited with code 7 before it was ready\nremote stderr:\n" + strings.TrimRight(stderr, "\n"); err.Message != want {
				t.Fatalf("message %q, want %q", err.Message, want)
			}
		})
	}
}

func TestStartFlushesAnIncompleteCharacterWhenStderrEnds(t *testing.T) {
	var logged logs
	err := startError(t, scripted(t, logged.add, failing("x\xf0\x9f", 7)...))
	logged.waitJoined(t, "x\xef\xbf\xbd")
	if want := "pi-env exited with code 7 before it was ready\nremote stderr:\nx\xef\xbf\xbd"; err.Message != want {
		t.Fatalf("message %q, want %q", err.Message, want)
	}
}

func TestStartErrorHasOnlyTheStderrOfItsAttempt(t *testing.T) {
	attempt := 0
	connection := NewConnection(ConnectionOptions{CommandFunc: func(context.Context) ([]string, error) {
		attempt++
		return append([]string{os.Args[0], scriptedDaemonArg}, failing("failure "+strconv.Itoa(attempt)+"\n", 1)...), nil
	}})
	t.Cleanup(connection.Close)
	if first := startError(t, connection).Message; !strings.Contains(first, "failure 1") {
		t.Fatalf("first %q", first)
	}
	second := startError(t, connection).Message
	if !strings.Contains(second, "failure 2") || strings.Contains(second, "failure 1") {
		t.Fatalf("second %q", second)
	}
}

func TestStartErrorKeepsTheFieldsOfARefusedHello(t *testing.T) {
	connection := scripted(t, nil, "fake", "error")
	for range 2 {
		err := startError(t, connection)
		if err.Code != "EINVAL" || err.Path != "/daemon" || err.Fields["extra"] != float64(1) {
			t.Errorf("code %q, path %q, fields %v", err.Code, err.Path, err.Fields)
		}
		if !strings.HasPrefix(err.Message, "hello refused") || IsConnectionLost(err) {
			t.Errorf("message %q, lost %v", err.Message, IsConnectionLost(err))
		}
	}
}

func TestStartOfAMissingDaemonIsASpawnError(t *testing.T) {
	connection := NewConnection(ConnectionOptions{Command: []string{"/nonexistent/pi-env"}})
	t.Cleanup(connection.Close)
	err := startError(t, connection)
	if err.Code != "spawn_error" || !IsConnectionLost(err) {
		t.Fatalf("code %q, lost %v", err.Code, IsConnectionLost(err))
	}
}

// TestStartWaitsForStderrToCloseBeforeFailing: a daemon that exits before it
// is ready fails the start at node's close event, once its stdout and stderr
// are closed, not at its exit: what a process it started still writes there
// is in the error.
func TestStartWaitsForStderrToCloseBeforeFailing(t *testing.T) {
	err := startError(t, scripted(t, nil, "linger", "first\n", "second\n"))
	if want := "pi-env exited with code 5 before it was ready\nremote stderr:\nfirst\nsecond"; err.Message != want {
		t.Fatalf("message %q, want %q", err.Message, want)
	}
}

func TestReadyDaemonFailsRequestsAsSoonAsItExits(t *testing.T) {
	connection := scripted(t, nil, "fake", "result")
	_, err := connection.Info(background())
	mustDo(t, err)
	started := time.Now()
	_, err = connection.Request(background(), "lstat", map[string]any{"path": "/"}, nil)
	if err == nil {
		t.Fatal("the request did not fail")
	}
	if !IsConnectionLost(err) || err.Error() != "pi-env connection lost" {
		t.Fatalf("%v (lost %v)", err, IsConnectionLost(err))
	}
	// The process the daemon started holds its pipes for 3 s; the request
	// must not wait for them.
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("failed after %v", elapsed)
	}
}

// TestDaemonExitingRightAfterItsHelloFailsRequestsAtOnce pins a window pi
// does not have: the daemon exits after sending its hello reply but before the
// start marks the session ready. In Node the continuation that sets ready runs
// before the exit event; here the exit goroutine can read ready first and skip
// its teardown, leaving a ready session with a dead daemon whose requests
// would wait for the silence limit (a process it started holds the pipes, so
// close comes 3 s later).
func TestDaemonExitingRightAfterItsHelloFailsRequestsAtOnce(t *testing.T) {
	beforeReady = func(s *session) { <-s.exited }
	t.Cleanup(func() { beforeReady = nil })
	connection := scripted(t, nil, "fake", "exit")
	_, err := connection.Info(background())
	mustDo(t, err)
	started := time.Now()
	_, err = connection.Request(background(), "lstat", map[string]any{"path": "/"}, nil)
	if err == nil || err.Error() != "pi-env connection lost" {
		t.Fatalf("request after the daemon exited: %v, want pi-env connection lost", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("the request failed after %v, not at once", elapsed)
	}
}
