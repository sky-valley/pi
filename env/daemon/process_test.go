package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// The process operation (upstream 9c9ce8547): a long-lived program with plain
// pipes. These tests run the Go daemon, or with -daemon=<binary> another one,
// such as the Rust pi-env built from upstream at 9c9ce8547 or later; the
// replies they expect are the Rust daemon's. TestProcessAgainstRust compares
// the two daemons on processScenarios.

func testedDaemon() string {
	if *daemonBinary != "" {
		return *daemonBinary
	}
	return goDaemon
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if reason := unixOnly(); reason != "" {
		t.Skip(reason)
	}
}

// started reports whether a process request's started event arrived.
func started(frames []frame.Frame) bool {
	return len(frames) > 0 && frames[0].Kind == frame.Event && strings.Contains(string(frames[0].JSON), `"started"`) || finished(frames)
}

// describe is frames as text: kind, JSON with pids zeroed, and payload.
func describe(frames []frame.Frame) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(strconv.Itoa(int(f.Kind)) + " " + numericFields.ReplaceAllString(string(f.JSON), `"$1":0`))
		if len(f.Payload) > 0 {
			b.WriteString(" " + strconv.Quote(string(f.Payload)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// joinOutput joins consecutive output events of one stream: how reads split
// a program's output is not deterministic.
func joinOutput(frames []frame.Frame) []frame.Frame {
	var joined []frame.Frame
	for _, f := range frames {
		if n := len(joined); n > 0 && f.Kind == frame.Event && joined[n-1].Kind == frame.Event &&
			string(f.JSON) == string(joined[n-1].JSON) && strings.Contains(string(f.JSON), `"output"`) {
			joined[n-1].Payload = append(append([]byte(nil), joined[n-1].Payload...), f.Payload...)
			continue
		}
		joined = append(joined, f)
	}
	return joined
}

func expectFrames(t *testing.T, what string, frames []frame.Frame, want string) {
	t.Helper()
	if got := describe(joinOutput(frames)); got != want {
		t.Errorf("%s:\n got %s\nwant %s", what, got, want)
	}
}

func TestHelloReportsTheProcessFeature(t *testing.T) {
	d := startDaemon(t, testedDaemon())
	reply := d.request(`{"op":"hello","protocol":1}`, nil)
	if !strings.Contains(string(reply[0].JSON), `"features":["listFiles","process"]`) {
		t.Fatalf("hello: %s", reply[0].JSON)
	}
}

func TestProcessRefusesRequestsItCannotRun(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"data.txt": "x"})
	for _, c := range []struct{ request, want string }{
		{`{"op":"process","cwd":"%R"}`, `{"code":"EINVAL","message":"process needs argv"}`},
		{`{"op":"process","argv":null,"cwd":"%R"}`, `{"code":"EINVAL","message":"process needs argv"}`},
		{`{"op":"process","argv":[],"cwd":"%R"}`, `{"code":"EINVAL","message":"argv must be a non-empty array of strings"}`},
		{`{"op":"process","argv":["true",1],"cwd":"%R"}`, `{"code":"EINVAL","message":"argv must be a non-empty array of strings"}`},
		{`{"op":"process","argv":"true","cwd":"%R"}`, `{"code":"EINVAL","message":"argv must be a non-empty array of strings"}`},
		{`{"op":"process","argv":[]}`, `{"code":"EINVAL","message":"argv must be a non-empty array of strings"}`},
		{`{"op":"process","argv":["true"]}`, `{"code":"EINVAL","message":"process needs cwd"}`},
		{`{"op":"process","argv":["true"],"cwd":"%R/missing"}`, `{"code":"spawn_error","message":"Working directory does not exist: %R/missing"}`},
		{`{"op":"process","argv":["pi-env-no-such-program"],"cwd":"%R"}`, `{"code":"spawn_error","message":"No such file or directory (os error 2)"}`},
		{`{"op":"process","argv":["./data.txt"],"cwd":"%R"}`, `{"code":"spawn_error","message":"Permission denied (os error 13)"}`},
		{`{"op":"processWrite"}`, `{"code":"EINVAL","message":"missing field target"}`},
		{`{"op":"processWrite","target":"1"}`, `{"code":"EINVAL","message":"missing field target"}`},
		{`{"op":"processWrite","target":999}`, `{"code":"EBADF","message":"no running process with this id"}`},
		{`{"op":"processCloseStdin"}`, `{"code":"EINVAL","message":"missing field target"}`},
		{`{"op":"processCloseStdin","target":999}`, `{"code":"EBADF","message":"no running process with this id"}`},
	} {
		request := strings.ReplaceAll(c.request, "%R", dir)
		want := "3 " + strings.ReplaceAll(c.want, "%R", dir) + "\n"
		expectFrames(t, request, d.request(request, nil), want)
	}
}

func TestProcessStartsThenStreamsOutputAndReportsTheExitCode(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	dir := t.TempDir()
	expectFrames(t, "exit code", d.request(`{"op":"process","argv":["sh","-c","exit 3"],"cwd":"`+dir+`"}`, nil),
		"4 {\"kind\":\"started\",\"pid\":0}\n2 {\"exitCode\":3}\n")
	expectFrames(t, "killed by a signal", d.request(`{"op":"process","argv":["sh","-c","kill -9 $$"],"cwd":"`+dir+`"}`, nil),
		"4 {\"kind\":\"started\",\"pid\":0}\n2 {\"exitCode\":137}\n")
	// Raw bytes, not decoded or translated, stdout and stderr apart.
	expectFrames(t, "output", d.request(`{"op":"process","argv":["sh","-c","printf 'a\\n\\377\\r\\n'; sleep 0.2; printf e >&2"],"cwd":"`+dir+`"}`, nil),
		"4 {\"kind\":\"started\",\"pid\":0}\n4 {\"kind\":\"output\",\"stream\":\"stdout\"} \"a\\n\\xff\\r\\n\"\n4 {\"kind\":\"output\",\"stream\":\"stderr\"} \"e\"\n2 {\"exitCode\":0}\n")
	expectFrames(t, "environment and working directory", d.request(`{"op":"process","argv":["sh","-c","printf %s \"$A\"; pwd"],"cwd":"`+dir+`","env":{"A":"a"}}`, nil),
		"4 {\"kind\":\"started\",\"pid\":0}\n4 {\"kind\":\"output\",\"stream\":\"stdout\"} \"a"+dir+"\\n\"\n2 {\"exitCode\":0}\n")
	expectFrames(t, "without the daemon's environment", d.request(`{"op":"process","argv":["/usr/bin/env"],"cwd":"`+dir+`","env":{"A":"a"},"inheritEnv":false}`, nil),
		"4 {\"kind\":\"started\",\"pid\":0}\n4 {\"kind\":\"output\",\"stream\":\"stdout\"} \"A=a\\n\"\n2 {\"exitCode\":0}\n")
}

func TestProcessWritesInputInOrderAndClosesStdinAfterIt(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	id := d.start(`{"op":"process","argv":["cat"],"cwd":"`+t.TempDir()+`"}`, nil)
	d.wait(id, started)
	target := `{"op":"processWrite","target":` + strconv.Itoa(int(id)) + `}`
	for _, chunk := range []string{"one ", "two ", "three"} {
		expectFrames(t, "write", d.request(target, []byte(chunk)), "2 {}\n")
	}
	expectFrames(t, "close stdin", d.request(`{"op":"processCloseStdin","target":`+strconv.Itoa(int(id))+`}`, nil), "2 {}\n")
	expectFrames(t, "cat", d.wait(id, finished),
		"4 {\"kind\":\"started\",\"pid\":0}\n4 {\"kind\":\"output\",\"stream\":\"stdout\"} \"one two three\"\n2 {\"exitCode\":0}\n")
	expectFrames(t, "write after the end", d.request(target, []byte("x")), "3 {\"code\":\"EBADF\",\"message\":\"no running process with this id\"}\n")
}

func TestProcessRefusesInputOnceStdinIsClosed(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	id := d.start(`{"op":"process","argv":["sleep","30"],"cwd":"`+t.TempDir()+`"}`, nil)
	d.wait(id, started)
	target := strconv.Itoa(int(id))
	d.request(`{"op":"processCloseStdin","target":`+target+`}`, nil)
	var refused []frame.Frame
	for tries := 0; tries < 100; tries++ {
		if refused = d.request(`{"op":"processWrite","target":`+target+`}`, []byte("x")); refused[0].Kind == frame.Error {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	expectFrames(t, "write after processCloseStdin", refused, "3 {\"code\":\"EBADF\",\"message\":\"the program's stdin is closed\"}\n")
	d.send(frame.Cancel, id, []byte(`{}`), nil)
	expectFrames(t, "cancelled", d.wait(id, finished), "4 {\"kind\":\"started\",\"pid\":0}\n3 {\"code\":\"aborted\",\"message\":\"aborted\"}\n")
}

func TestProcessCancelKillsTheProgramsGroup(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	dir := t.TempDir()
	// The program's child reports its pid and waits; killing the group ends
	// both, so stdout closes and the request settles.
	id := d.start(`{"op":"process","argv":["sh","-c","sleep 30 & echo $!; wait"],"cwd":"`+dir+`"}`, nil)
	frames := d.wait(id, func(frames []frame.Frame) bool { return len(frames) > 1 || finished(frames) })
	child, err := strconv.Atoi(strings.TrimSpace(string(frames[1].Payload)))
	if err != nil {
		t.Fatalf("child pid: %s", describe(frames))
	}
	d.send(frame.Cancel, id, []byte(`{"mode":"kill"}`), nil)
	expectFrames(t, "killed", d.wait(id, finished)[2:], "2 {\"exitCode\":137}\n")
	for deadline := time.Now().Add(5 * time.Second); processExists(child) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(child) {
		t.Fatalf("the program's child %d survived", child)
	}
}

// TestProcessKillsWhatTheProgramLeftRunningWhenItExits (upstream 032656b6c):
// a wrapper that starts a server in its group and exits, as npx can, leaves
// nothing running.
func TestProcessKillsWhatTheProgramLeftRunningWhenItExits(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	id := d.start(`{"op":"process","argv":["sh","-c","sleep 30 </dev/null >/dev/null 2>&1 & echo $!"],"cwd":"`+t.TempDir()+`"}`, nil)
	frames := joinOutput(d.wait(id, finished))
	if len(frames) != 3 {
		t.Fatalf("frames: %s", describe(frames))
	}
	expectFrames(t, "exit", frames[2:], "2 {\"exitCode\":0}\n")
	child, err := strconv.Atoi(strings.TrimSpace(string(frames[1].Payload)))
	if err != nil {
		t.Fatalf("child pid: %s", describe(frames))
	}
	// The killed child is an orphan; it is gone once init has reaped it.
	for deadline := time.Now().Add(5 * time.Second); processExists(child) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(child) {
		t.Fatalf("the program's child %d survived the program's exit", child)
	}
}

// TestProcessStopsWritingOnceTheProgramHasExited: a writer blocked on a pipe
// that a descendant holds open without reading stops when the program exits,
// so the request settles and the descendant sees stdin end.
func TestProcessStopsWritingOnceTheProgramHasExited(t *testing.T) {
	skipOnWindows(t)
	d := startDaemon(t, testedDaemon())
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	// fd 3: a non-interactive shell gives background jobs /dev/null as stdin.
	// The descendant leaves the program's process group, which is killed when
	// the program exits (perl's setsid: macOS has no setsid command).
	descendant := `sleep 1; wc -c <&3 > ` + count + `.tmp; mv ` + count + `.tmp ` + count
	script := `exec 3<&0; perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' sh -c '` + descendant + `' >/dev/null 2>&1 & sleep 0.3`
	id := d.start(`{"op":"process","argv":["sh","-c",`+strconv.Quote(script)+`],"cwd":"`+dir+`"}`, nil)
	d.wait(id, started)
	expectFrames(t, "write", d.request(`{"op":"processWrite","target":`+strconv.Itoa(int(id))+`}`, bytes.Repeat([]byte("x"), 4<<20)), "2 {}\n")
	expectFrames(t, "exit", d.wait(id, finished), "4 {\"kind\":\"started\",\"pid\":0}\n2 {\"exitCode\":0}\n")
	var data []byte
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var err error
		if data, err = os.ReadFile(count); err == nil {
			break
		}
	}
	if read, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || read >= 4<<20 {
		t.Fatalf("the descendant read %q bytes (%v); the writer should have stopped", data, err)
	}
}

func processExists(pid int) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// TestProcessAgainstRust runs process sessions on both daemons and compares
// every reply. It needs the Rust daemon at upstream 9c9ce8547 or later:
//
//	PI_ENV_RUST_DAEMON=/path/to/rust/pi-env go test ./daemon -run TestProcessAgainstRust
func TestProcessAgainstRust(t *testing.T) {
	rust := os.Getenv("PI_ENV_RUST_DAEMON")
	if rust == "" {
		t.Skip("set PI_ENV_RUST_DAEMON to the Rust pi-env to compare processes with it")
	}
	skipOnWindows(t)
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"data.txt": "x", "bin/script": "echo from a script\n"})
	if err := os.Chmod(filepath.Join(dir, "bin/script"), 0o755); err != nil {
		t.Fatal(err)
	}
	q := strconv.Quote
	sessions := map[string]func(d *daemonSession) []frame.Frame{}
	for i, request := range []string{
		`{"op":"process","cwd":"%R"}`, `{"op":"process","argv":[],"cwd":"%R"}`, `{"op":"process","argv":[""],"cwd":"%R"}`,
		`{"op":"process","argv":{"0":"true"},"cwd":"%R"}`, `{"op":"process","argv":["true"],"cwd":1}`,
		`{"op":"process","argv":["true"],"cwd":"%R/data.txt"}`, `{"op":"process","argv":["true"],"cwd":"%R/missing"}`,
		`{"op":"process","argv":["true"],"cwd":"%R","env":{"A":1}}`, `{"op":"process","argv":["true"],"cwd":"%R","env":[]}`,
		`{"op":"process","argv":["true"],"cwd":"%R","inheritEnv":"no"}`, `{"op":"process","argv":["a\u0000b"],"cwd":"%R"}`,
		`{"op":"process","argv":["echo","a\u0000b"],"cwd":"%R"}`, `{"op":"process","argv":["true"],"cwd":"%R","env":{"A":"a\u0000"}}`,
		`{"op":"process","argv":["data.txt"],"cwd":"%R"}`, `{"op":"process","argv":["./data.txt"],"cwd":"%R"}`,
		`{"op":"process","argv":["bin/script"],"cwd":"%R"}`, `{"op":"process","argv":["script"],"cwd":"%R","env":{"PATH":"%R/bin"}}`,
		`{"op":"process","argv":["/bin/sh","-c","printf %s \"$PATH\""],"cwd":"%R","env":{},"inheritEnv":false}`,
		`{"op":"process","argv":["sh","-c","exit 255"],"cwd":"%R"}`, `{"op":"process","argv":["sh","-c","kill -TERM $$"],"cwd":"%R"}`,
		`{"op":"process","argv":["sh","-c","printf 'x%.0s' $(seq 1 100000)"],"cwd":"%R"}`,
		`{"op":"process","argv":["sh","-c","sleep 5 & exit 0"],"cwd":"%R"}`,
		`{"op":"processWrite","target":-1}`, `{"op":"processWrite","target":1.5}`, `{"op":"processWrite","target":4294967296}`,
		`{"op":"processCloseStdin","target":null}`,
	} {
		request := strings.ReplaceAll(request, "%R", dir)
		sessions[strconv.Itoa(i)+" "+request] = func(d *daemonSession) []frame.Frame { return joinOutput(d.request(request, nil)) }
	}
	sessions["input"] = func(d *daemonSession) []frame.Frame {
		id := d.start(`{"op":"process","argv":["sh","-c","cat; echo done >&2"],"cwd":`+q(dir)+`}`, nil)
		d.wait(id, started)
		target := `{"op":"processWrite","target":` + strconv.Itoa(int(id)) + `}`
		var frames []frame.Frame
		frames = append(frames, d.request(target, []byte("a\r\n\xff"))...)
		frames = append(frames, d.request(target, nil)...)
		frames = append(frames, d.request(`{"op":"processCloseStdin","target":`+strconv.Itoa(int(id))+`}`, nil)...)
		frames = append(frames, joinOutput(d.wait(id, finished))...)
		return append(frames, d.request(target, []byte("late"))...)
	}
	for _, mode := range []string{`{}`, `{"mode":"kill"}`} {
		sessions["cancel "+mode] = func(d *daemonSession) []frame.Frame {
			id := d.start(`{"op":"process","argv":["sleep","30"],"cwd":`+q(dir)+`}`, nil)
			d.wait(id, started)
			d.send(frame.Cancel, id, []byte(mode), nil)
			return d.wait(id, finished)
		}
	}
	sessions["cancelled before it runs"] = func(d *daemonSession) []frame.Frame {
		d.nextID++
		id := d.nextID
		d.send(frame.Request, id, []byte(`{"op":"process","argv":["sleep","30"],"cwd":`+q(dir)+`}`), nil)
		d.send(frame.Cancel, id, []byte(`{}`), nil)
		frames := d.wait(id, finished)
		// The cancel may reach the program before or after it starts.
		if len(frames) == 2 {
			frames = frames[1:]
		}
		return frames
	}
	sessions["program closes stdin"] = func(d *daemonSession) []frame.Frame {
		id := d.start(`{"op":"process","argv":["sh","-c","exec 0<&-; sleep 0.5"],"cwd":`+q(dir)+`}`, nil)
		d.wait(id, started)
		time.Sleep(100 * time.Millisecond)
		target := `{"op":"processWrite","target":` + strconv.Itoa(int(id)) + `}`
		var frames []frame.Frame
		// The first write fails in the writer (a broken pipe), which stops.
		frames = append(frames, d.request(target, []byte("x"))...)
		for tries := 0; tries < 100; tries++ {
			if reply := d.request(target, []byte("x")); reply[0].Kind == frame.Error {
				frames = append(frames, reply...)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return append(frames, d.wait(id, finished)...)
	}
	daemons := [2]*daemonSession{startDaemon(t, rust), startDaemon(t, goDaemon)}
	for name, session := range sessions {
		var replies [2]string
		for i, d := range daemons {
			replies[i] = strings.ReplaceAll(describe(session(d)), dir, "<dir>")
		}
		if replies[0] != replies[1] {
			t.Errorf("%s\nrust: %s\ngo:   %s", name, replies[0], replies[1])
		}
	}
	t.Logf("%d sessions", len(sessions))
}
