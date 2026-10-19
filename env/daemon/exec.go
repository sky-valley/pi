package daemon

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// exec: Durable's NodeExecutionEnv.exec semantics (docs/semantics.md), output
// streamed as output events.

const (
	exitStdioGrace = 100 * time.Millisecond
	// unsentPoll is how often a delivery waiting for the link to drain looks
	// again.
	unsentPoll = 20 * time.Millisecond
	// readChunk is how much one read of a command's output takes.
	readChunk = 64 * 1024
)

// groups are the commands this daemon started and has not reaped, by process
// id (which is also the process group's); killed when the connection ends.
type groups struct {
	mu   sync.Mutex
	pids map[int]struct{}
}

func newGroups() *groups { return &groups{pids: map[int]struct{}{}} }

func (g *groups) add(pid int) {
	g.mu.Lock()
	g.pids[pid] = struct{}{}
	g.mu.Unlock()
}

func (g *groups) remove(pid int) {
	g.mu.Lock()
	delete(g.pids, pid)
	g.mu.Unlock()
}

func (g *groups) killAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for pid := range g.pids {
		killTree(pid)
	}
}

// control is the cancellation of one request, registered before the request
// runs so a cancel that arrives first is not lost.
type control struct {
	// aborted: the command settles as aborted. killed: it is killed without
	// aborting, as cleanup() does, and settles with the killed process's status.
	aborted atomic.Bool
	killed  atomic.Bool
	// wake tells a running command that a flag was set.
	wake chan struct{}
}

func newControl() *control { return &control{wake: make(chan struct{}, 1)} }

func (c *control) cancel(kill bool) {
	if kill {
		c.killed.Store(true)
	} else {
		c.aborted.Store(true)
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

type spillLimits struct{ afterBytes, afterLines uint64 }

type execRequest struct {
	command      string
	hasCommand   bool
	argv         []string
	hasArgv      bool
	cwd          string
	env          object
	inheritEnv   bool
	shellPath    string
	hasShellPath bool
	timeout      time.Duration
	hasTimeout   bool
	spill        *spillLimits
	window       *window
}

func execRequestFromJSON(json any) (execRequest, *failure) {
	var r execRequest
	r.command, r.hasCommand = asString(get(json, "command"))
	r.argv, r.hasArgv = asStrings(get(json, "argv"))
	cwd, ok := asString(get(json, "cwd"))
	if !ok {
		return r, newFailure("EINVAL", "exec needs cwd")
	}
	r.cwd = cwd
	r.env, _ = asObject(get(json, "env"))
	r.inheritEnv = true
	if inherit, ok := asBool(get(json, "inheritEnv")); ok {
		r.inheritEnv = inherit
	}
	r.shellPath, r.hasShellPath = asString(get(json, "shellPath"))
	if ms, ok := asF64(get(json, "timeoutMs")); ok {
		seconds := ms / 1000
		// Rust's Duration::from_secs_f64 panics (and the request never
		// settles) on what it cannot represent; refuse it instead.
		if seconds < 0 || math.IsNaN(seconds) || seconds >= float64(math.MaxUint64) {
			return r, newFailure("EINVAL", "invalid field timeoutMs")
		}
		r.timeout, r.hasTimeout = secondsDuration(seconds), true
	}
	if spill, ok := asObject(get(json, "spill")); ok {
		afterBytes, ok1 := asU64(spill["afterBytes"])
		afterLines, ok2 := asU64(spill["afterLines"])
		if ok1 && ok2 {
			r.spill = &spillLimits{afterBytes, afterLines}
		}
	}
	if w, ok := windowFromJSON(get(json, "window")); ok {
		r.window = &w
	}
	return r, nil
}

func randomUUID() string {
	b := randomBytes()
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ioErrorText is how Rust displays an I/O error: the system's text and its
// code, or the error's own text.
func ioErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := systemText(errno)
		if text != "" {
			text = strings.ToUpper(text[:1]) + text[1:]
		}
		return text + " (os error " + strconv.Itoa(int(errno)) + ")"
	}
	return err.Error()
}

// spill is the complete raw output, written to a file once it crosses either
// threshold, as Node's is.
type spill struct {
	spillLimits
	seenBytes    uint64
	seenNewlines uint64
	prefix       [][]byte
	file         *os.File
	path         string
	failed       string
}

func (s *spill) push(chunk []byte, tmpdir string) {
	if s.failed != "" || len(chunk) == 0 {
		return
	}
	if s.file == nil {
		s.seenBytes += uint64(len(chunk))
		s.seenNewlines += uint64(bytes.Count(chunk, []byte("\n")))
		lines := s.seenNewlines
		if chunk[len(chunk)-1] != '\n' {
			lines++
		}
		if s.seenBytes <= s.afterBytes && lines <= s.afterLines {
			s.prefix = append(s.prefix, append([]byte(nil), chunk...))
			return
		}
		if err := s.start(tmpdir); err != nil {
			s.failed = ioErrorText(err)
			return
		}
	}
	if err := writeAll(s.file, chunk); err != nil {
		s.failed = ioErrorText(err)
	}
}

// start is createTempFile({ prefix: "pi-output-", suffix: ".log" }): a fresh
// tmp- directory holding the file.
func (s *spill) start(tmpdir string) error {
	directory, err := mkdtemp(joinPath(tmpdir, "tmp-"))
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return err
	}
	path := joinPath(directory, "pi-output-"+randomUUID()+".log")
	file, err := openFile(path, modeAppend)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return err
	}
	for _, chunk := range s.prefix {
		if err := writeAll(file, chunk); err != nil {
			file.Close()
			return err
		}
	}
	s.prefix = nil
	s.file, s.path = file, decodeUTF8([]byte(path))
	return nil
}

func (s *spill) close() {
	if s != nil && s.file != nil {
		s.file.Close()
	}
}

// commandMessage is what a command's readers and waiter report.
type commandMessage struct {
	stream int // for data and end of output
	data   []byte
	ended  bool
	exited bool
	code   int64
	err    error
}

// readPipe reads pipe until it ends or fails, sending each chunk; without an
// output window, reading waits while too much output is unsent. It stops when
// done closes.
func readPipe(stream int, pipe *os.File, messages chan<- commandMessage, done <-chan struct{}, stop *atomic.Bool, gate *output) {
	buf := make([]byte, readChunk)
	for !stop.Load() {
		n, err := pipe.Read(buf)
		if n > 0 {
			select {
			case messages <- commandMessage{stream: stream, data: append([]byte(nil), buf[:n]...)}:
			case <-done:
				return
			}
			if gate != nil {
				gate.waitForRoom(stop)
			}
		}
		if err != nil {
			break
		}
	}
	select {
	case messages <- commandMessage{stream: stream, ended: true}:
	case <-done:
	}
}

func outputFrame(id uint32, stream, text string, skipped object) frame.Frame {
	json := object{"kind": "output", "stream": stream}
	if skipped != nil {
		json["skipped"] = skipped
	}
	return frame.Frame{Kind: frame.Event, ID: id, JSON: marshal(json), Payload: []byte(text)}
}

// runExec runs one command, sending output events for request id, until it
// settles.
func runExec(id uint32, request execRequest, tmpdir string, out *output, ctl *control, running *groups) (object, *failure) {
	if ctl.aborted.Load() {
		return nil, newFailure("aborted", "aborted")
	}
	// A string runs through the shell (as its last argument, or on stdin for
	// legacy WSL bash); argv runs directly.
	var (
		program      string
		args         []string
		stdinCommand string
		pipeCommand  bool
	)
	switch {
	case request.hasCommand:
		shell, fail := resolveShell(request.shellPath, request.hasShellPath)
		if fail != nil {
			return nil, fail
		}
		program, args = shell.program, shell.args
		if shell.commandOnStdin {
			stdinCommand, pipeCommand = request.command, true
		} else {
			args = append(args, request.command)
		}
	case request.hasArgv:
		if len(request.argv) == 0 {
			return nil, newFailure("spawn_error", "Empty argv: no program to run")
		}
		resolved, fail := resolveArgvProgram(request.argv[0], request.cwd, request.env)
		if fail != nil {
			return nil, fail
		}
		program, args = resolved, request.argv[1:]
	default:
		return nil, newFailure("EINVAL", "exec needs command or argv")
	}
	if !pathExists(request.cwd) {
		return nil, newFailure("spawn_error", "Working directory does not exist: "+request.cwd+"\nCannot execute bash commands.")
	}
	command, fail := startCommand(program, args, request, pipeCommand)
	if fail != nil {
		return nil, fail
	}
	if pipeCommand {
		// As Rust's `let _ = stdin.write_all(...)`: a shell that exits without
		// reading its script reports through its exit code.
		_ = writeAll(command.stdin, []byte(stdinCommand))
		command.stdin.Close()
	}
	pid := command.process.pid
	running.add(pid)
	var deadline time.Time
	if request.hasTimeout {
		deadline = time.Now().Add(request.timeout)
	}
	stop := new(atomic.Bool)
	done := make(chan struct{})
	defer close(done)
	messages := make(chan commandMessage, 64)
	// Without a window, output is not coalesced; reading then waits for the
	// link.
	var gate *output
	if request.window == nil {
		gate = out
	}
	for stream, pipe := range command.output {
		go readPipe(stream, pipe, messages, done, stop, gate)
	}
	go func() {
		code, err := command.process.wait()
		select {
		case messages <- commandMessage{exited: true, code: code, err: err}:
		case <-done:
		}
	}()

	decoders := [2]*decoder{newStreamDecoder(), newStreamDecoder()}
	var spilled *spill
	if request.spill != nil {
		spilled = &spill{spillLimits: *request.spill}
	}
	defer spilled.close()
	var held *pending
	if request.window != nil {
		held = newPending(*request.window)
	}
	unsent := new(atomic.Int64)
	names := [2]string{"stdout", "stderr"}
	emit := func(stream int, text string) {
		if text == "" {
			return
		}
		if held != nil {
			held.push(names[stream], text)
		} else {
			out.sendBulk(outputFrame(id, names[stream], text, nil), nil)
		}
	}
	deliver := func() {
		for _, event := range held.take() {
			out.sendBulk(outputFrame(id, event.stream, event.text, event.skipped), unsent)
		}
	}
	var (
		ended     [2]bool
		exited    bool
		reported  bool
		code      int64
		waitErr   error
		idleUntil time.Time
		timedOut  bool
		aborted   bool
		killed    bool
	)
	kill := func() {
		if !killed {
			killed = true
			killTree(pid)
		}
	}
	if ctl.killed.Load() {
		kill()
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		now := time.Now()
		// Node's timer stays armed until the command settles, including the
		// grace period after exit.
		if !timedOut && request.hasTimeout && !now.Before(deadline) {
			timedOut = true
			kill()
		}
		if exited && ended[0] && ended[1] {
			break
		}
		if !idleUntil.IsZero() && !now.Before(idleUntil) {
			// A descendant may hold the pipes open after the process exited;
			// stop waiting for it.
			break
		}
		if held != nil && !held.isEmpty() && !now.Before(held.nextSend) && unsent.Load() == 0 {
			deliver()
		}
		wait := time.Hour
		if request.hasTimeout && !timedOut {
			wait = min(wait, max(deadline.Sub(now), 0))
		}
		if !idleUntil.IsZero() {
			wait = min(wait, max(idleUntil.Sub(now), 0))
		}
		if held != nil && !held.isEmpty() {
			if unsent.Load() > 0 {
				wait = min(wait, unsentPoll)
			} else {
				wait = min(wait, max(held.nextSend.Sub(now), 0))
			}
		}
		timer.Reset(wait)
		select {
		case message := <-messages:
			switch {
			case message.data != nil:
				emit(message.stream, decoders[message.stream].decode(message.data, false))
				if spilled != nil {
					spilled.push(message.data, tmpdir)
					if spilled.failed != "" {
						kill()
					}
				}
				if exited {
					idleUntil = time.Now().Add(exitStdioGrace)
				}
			case message.ended:
				ended[message.stream] = true
			case message.exited:
				exited = true
				if message.err != nil {
					waitErr = message.err
					idleUntil = time.Now()
				} else {
					reported, code = true, message.code
					idleUntil = time.Now().Add(exitStdioGrace)
				}
			}
		case <-ctl.wake:
			if ctl.aborted.Load() {
				aborted = true
				kill()
			}
			if ctl.killed.Load() {
				kill()
			}
		case <-timer.C:
		}
	}
	// Like Node destroying its streams: stop reading, so the pipes close even
	// if a descendant still holds them.
	stop.Store(true)
	for _, pipe := range command.output {
		pipe.Close()
	}
	running.remove(pid)
	emit(0, decoders[0].decode(nil, true))
	emit(1, decoders[1].decode(nil, true))
	if held != nil && !held.isEmpty() {
		deliver()
	}
	withSpill := func(f *failure) *failure {
		if spilled != nil && spilled.path != "" {
			return f.withExtra(object{"spillPath": spilled.path})
		}
		return f
	}
	if timedOut {
		return nil, withSpill(newFailure("timeout", "timeout"))
	}
	if aborted {
		return nil, withSpill(newFailure("aborted", "aborted"))
	}
	if spilled != nil && spilled.failed != "" {
		return nil, newFailure("unknown", "Failed to preserve complete shell output: "+spilled.failed)
	}
	if waitErr != nil {
		return nil, newFailure("spawn_error", waitErr.Error())
	}
	if !reported {
		return nil, newFailure("spawn_error", "process did not report its exit")
	}
	result := object{"exitCode": code}
	if spilled != nil && spilled.path != "" {
		result["spillPath"] = spilled.path
	}
	return result, nil
}

// command is a started command: its process, its output pipes (stdout,
// stderr), and its stdin when the command goes there.
type command struct {
	process child
	output  [2]*os.File
	stdin   *os.File
}

// startCommand starts program with args as the request asks: in its working
// directory and environment, stdin empty (or a pipe), and stdout and stderr
// piped.
func startCommand(program string, args []string, request execRequest, pipeStdin bool) (*command, *failure) {
	spawnError := func(err error) (*command, *failure) {
		return nil, newFailure("spawn_error", ioErrorText(unwrapPathError(err)))
	}
	var parentEnds, childEnds []*os.File
	closeAll := func() {
		for _, f := range append(parentEnds, childEnds...) {
			f.Close()
		}
	}
	var stdin *os.File
	if pipeStdin {
		r, w, err := os.Pipe()
		if err != nil {
			return spawnError(err)
		}
		childEnds, parentEnds = append(childEnds, r), append(parentEnds, w)
		stdin = w
	} else {
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return spawnError(err)
		}
		childEnds = append(childEnds, null)
	}
	var outputs [2]*os.File
	for i := range outputs {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
			return spawnError(err)
		}
		childEnds, parentEnds = append(childEnds, w), append(parentEnds, r)
		outputs[i] = r
	}
	process, err := spawnProcess(program, args, request.cwd, request.env, request.inheritEnv, childEnds)
	for _, f := range childEnds {
		f.Close()
	}
	childEnds = nil
	if err != nil {
		closeAll()
		return spawnError(err)
	}
	return &command{process: process, output: outputs, stdin: stdin}, nil
}

func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
