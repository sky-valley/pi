package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// The oracle tests run scripted request sequences (scenarios_test.go) against
// a daemon binary and compare every reply, byte for byte after normalizing
// what differs between runs (temporary paths, times, inodes, pids), with
// replies the Rust daemon gave to the same requests. Those replies live in
// testdata/oracle/<GOOS>.json and are captured, never edited:
//
//	go test ./daemon -run TestOracle -oracle=/path/to/rust/pi-env
//
// (the Rust daemon of @earendil-works/pi-env 1.4.0, from its npm bin/ or a
// cargo build of upstream packages/env/daemon).

var (
	oracleBinary = flag.String("oracle", "", "capture testdata/oracle from this daemon binary (the Rust pi-env) instead of testing the Go one")
	daemonBinary = flag.String("daemon", "", "compare this daemon binary with the oracle instead of the Go daemon built from source")
)

// goDaemon is the Go daemon built for the tests, with buildFlags.
var (
	goDaemon   string
	buildFlags []string
)

func TestMain(m *testing.M) {
	if daemon := os.Getenv(seccompExecVariable); daemon != "" {
		// Started by TestAndroidBuildRunsCommandsWithoutPidfd to run a daemon.
		execUnderSeccomp(daemon)
	}
	flag.Parse()
	dir, err := os.MkdirTemp("", "pi-env-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	goDaemon = filepath.Join(dir, "pi-env")
	if runtime.GOOS == "windows" {
		goDaemon += ".exe"
	}
	build := exec.Command("go", append(append([]string{"build"}, buildFlags...), "-o", goDaemon, "github.com/sky-valley/pi/env/cmd/pi-env")...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the daemon:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// daemonSession is one running daemon and its frames.
type daemonSession struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	frames  map[uint32][]frame.Frame
	arrived chan struct{}
	ended   chan struct{}
	nextID  uint32
}

func startDaemon(t *testing.T, binary string, env ...string) *daemonSession {
	return startDaemonWith(t, exec.Command(binary, "serve", "--token", "0123abcd"), env...)
}

// startDaemonPrepared starts the daemon from python3 after setup, Python
// statements that ignore or block signals as nohup or a parent may.
func startDaemonPrepared(t *testing.T, binary string, setup string, env ...string) *daemonSession {
	script := "import os, signal, sys\n" + setup + "\nos.execv(sys.argv[1], [sys.argv[1], 'serve', '--token', '0123abcd'])"
	return startDaemonWith(t, exec.Command("/usr/bin/python3", "-I", "-c", script, binary), env...)
}

func startDaemonWith(t *testing.T, cmd *exec.Cmd, env ...string) *daemonSession {
	t.Helper()
	if len(env) > 0 {
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &daemonSession{t: t, cmd: cmd, stdin: stdin, frames: map[uint32][]frame.Frame{}, arrived: make(chan struct{}, 1), ended: make(chan struct{})}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || line != "PI-ENV 0123abcd\n" {
		t.Fatalf("sync line %q: %v", line, err)
	}
	go func() {
		defer close(s.ended)
		for {
			f, err := frame.Read(reader)
			if err != nil {
				return
			}
			if f.Kind == frame.Ping {
				continue
			}
			s.mu.Lock()
			s.frames[f.ID] = append(s.frames[f.ID], f)
			s.mu.Unlock()
			select {
			case s.arrived <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		stdin.Close()
		select {
		case <-s.ended:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
		}
		cmd.Wait()
	})
	return s
}

func (s *daemonSession) send(kind byte, id uint32, json []byte, payload []byte) {
	s.t.Helper()
	if err := frame.Write(s.stdin, frame.Frame{Kind: kind, ID: id, JSON: json, Payload: payload}); err != nil {
		s.t.Fatal(err)
	}
}

// start sends a request and returns its id without waiting.
func (s *daemonSession) start(json string, payload []byte) uint32 {
	s.nextID++
	s.send(frame.Request, s.nextID, []byte(json), payload)
	return s.nextID
}

// wait collects the frames of request id until one satisfies done.
func (s *daemonSession) wait(id uint32, done func([]frame.Frame) bool) []frame.Frame {
	s.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		s.mu.Lock()
		frames := append([]frame.Frame(nil), s.frames[id]...)
		s.mu.Unlock()
		if done(frames) {
			return frames
		}
		select {
		case <-s.arrived:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			s.t.Fatalf("request %d: no reply in time (have %d frames)", id, len(frames))
		}
	}
}

func finished(frames []frame.Frame) bool {
	return len(frames) > 0 && (frames[len(frames)-1].Kind == frame.Result || frames[len(frames)-1].Kind == frame.Error)
}

// request sends a request and waits for its result or error.
func (s *daemonSession) request(json string, payload []byte) []frame.Frame {
	return s.wait(s.start(json, payload), finished)
}

// record is one reply as the golden file holds it.
type record struct {
	Label   string `json:"label"`
	Kind    byte   `json:"kind"`
	JSON    string `json:"json"`
	Payload string `json:"payload,omitempty"`
}

// recorder runs a scenario's requests and keeps normalized replies.
type recorder struct {
	*daemonSession
	root      string
	normalize []func(string) string
	records   []record
}

var numericFields = regexp.MustCompile(`"(mtimeSec|mtimeNsec|dev|ino|pid)":-?[0-9]+`)

func (r *recorder) clean(text string) string {
	for _, n := range r.normalize {
		text = n(text)
	}
	return numericFields.ReplaceAllString(text, `"$1":0`)
}

// keep records frames under label, payloads as text when they are UTF-8.
// Consecutive output events without a skip are joined: how reads split a
// command's output is not deterministic.
func (r *recorder) keep(label string, frames []frame.Frame) {
	var joined []frame.Frame
	for _, f := range frames {
		if n := len(joined); n > 0 && f.Kind == frame.Event && joined[n-1].Kind == frame.Event &&
			string(f.JSON) == string(joined[n-1].JSON) && !strings.Contains(string(f.JSON), "skipped") &&
			strings.Contains(string(f.JSON), `"output"`) {
			joined[n-1].Payload = append(append([]byte(nil), joined[n-1].Payload...), f.Payload...)
			continue
		}
		joined = append(joined, f)
	}
	for _, f := range joined {
		payload := string(f.Payload)
		if !isText(f.Payload) {
			payload = fmt.Sprintf("%x", f.Payload)
		}
		r.records = append(r.records, record{Label: label, Kind: f.Kind, JSON: r.clean(string(f.JSON)), Payload: r.clean(payload)})
	}
}

// isText reports whether a payload reads as text in the golden file: UTF-8
// with no control characters but newlines, tabs and the NULs that end
// listFiles records.
func isText(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\t' && c != '\r' && c != 0 || c == 0x7f {
			return false
		}
	}
	return decodeUTF8(b) == string(b)
}

// do sends a request (with %R replaced by the scenario root) and records the
// replies.
func (r *recorder) do(label, json string, payload ...byte) []frame.Frame {
	json = strings.ReplaceAll(json, "%R", jsonEscape(r.root))
	frames := r.request(json, payload)
	r.keep(label, frames)
	return frames
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

type scenario struct {
	name string
	// skip is a reason to skip the scenario on this system, or "".
	skip func() string
	// launch, when set, is Python run before the daemon starts in its process
	// (startDaemonPrepared).
	launch string
	run    func(r *recorder)
	clean  []func(string) string
}

func goldenPath() string {
	return filepath.Join("testdata", "oracle", runtime.GOOS+".json")
}

func TestOracle(t *testing.T) {
	binary := goDaemon
	if *daemonBinary != "" {
		binary = *daemonBinary
	}
	capturing := *oracleBinary != ""
	if capturing {
		binary = *oracleBinary
	}
	golden := map[string][]record{}
	if data, err := os.ReadFile(goldenPath()); err == nil {
		if err := json.Unmarshal(data, &golden); err != nil {
			t.Fatal(err)
		}
	} else if !capturing {
		t.Skipf("no oracle for %s: capture one with go test ./daemon -run TestOracle -oracle=<Rust pi-env>", runtime.GOOS)
	}
	ran := 0
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			if sc.skip != nil {
				if reason := sc.skip(); reason != "" {
					t.Skip(reason)
				}
			}
			root, err := os.MkdirTemp("", "pi-env-oracle-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			canonical, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			// The daemon's home and git configuration live in the scenario's
			// directory, so the user's own global excludes cannot leak in.
			var env []string
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				switch key {
				case "HOME", "XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM":
					continue
				}
				env = append(env, entry)
			}
			env = append(env, "HOME="+filepath.Join(root, "home"), "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_CONFIG_SYSTEM="+os.DevNull)
			session := startDaemon
			if sc.launch != "" {
				session = func(t *testing.T, binary string, env ...string) *daemonSession {
					return startDaemonPrepared(t, binary, sc.launch, env...)
				}
			}
			r := &recorder{daemonSession: session(t, binary, env...), root: root}
			r.normalize = append(r.normalize,
				func(s string) string { return strings.ReplaceAll(s, jsonEscape(canonical), "<root>") },
				func(s string) string { return strings.ReplaceAll(s, jsonEscape(root), "<root>") },
			)
			r.normalize = append(r.normalize, sc.clean...)
			sc.run(r)
			ran++
			if capturing {
				golden[sc.name] = r.records
				return
			}
			want, ok := golden[sc.name]
			if !ok || len(want) == 0 {
				t.Fatalf("no captured replies for %s: capture with -oracle=<Rust pi-env>", sc.name)
			}
			compareRecords(t, want, r.records)
		})
	}
	if ran == 0 {
		t.Fatal("no scenario ran")
	}
	if capturing {
		keys := make([]string, 0, len(golden))
		for key := range golden {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var out bytes.Buffer
		out.WriteString("{\n")
		for i, key := range keys {
			body, _ := json.MarshalIndent(golden[key], "  ", "  ")
			name, _ := json.Marshal(key)
			fmt.Fprintf(&out, "  %s: %s", name, body)
			if i < len(keys)-1 {
				out.WriteString(",")
			}
			out.WriteString("\n")
		}
		out.WriteString("}\n")
		if err := os.MkdirAll(filepath.Dir(goldenPath()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath(), out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("captured %d scenarios from %s into %s", len(keys), binary, goldenPath())
	}
}

func compareRecords(t *testing.T, want, got []record) {
	t.Helper()
	for i := 0; i < max(len(want), len(got)); i++ {
		switch {
		case i >= len(want):
			t.Errorf("extra reply %d: %+v", i, got[i])
		case i >= len(got):
			t.Errorf("missing reply %d: %+v", i, want[i])
		case want[i] != got[i]:
			t.Errorf("reply %d (%s) differs:\n  rust: kind %d %s %q\n  go:   kind %d %s %q", i, want[i].Label,
				want[i].Kind, want[i].JSON, want[i].Payload, got[i].Kind, got[i].JSON, got[i].Payload)
		}
	}
}
