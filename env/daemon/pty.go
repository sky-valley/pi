package daemon

import (
	"strconv"
	"sync"
)

// pty: a program in a pseudo-terminal, for interactive terminals. Unlike
// exec, output is the terminal's raw bytes, and the program reads input:
// ptyWrite, ptyResize and ptyForeground address the running pty request by
// its id.

// maxCells is the largest terminal size accepted, in cells.
const maxCells = 4096

// terminals are the running terminals by the id of their pty request.
type terminals struct {
	mu      sync.Mutex
	running map[uint32]*terminal
}

func newTerminals() *terminals { return &terminals{running: map[uint32]*terminal{}} }

func (t *terminals) find(json any) (*terminal, *failure) {
	target, ok := asU64(get(json, "target"))
	if !ok {
		return nil, newFailure("EINVAL", "missing field target")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	terminal, ok := t.running[uint32(target)]
	if !ok {
		return nil, newFailure("EBADF", "no running terminal with this id")
	}
	return terminal, nil
}

type ptyRequest struct {
	argv       []string
	hasArgv    bool
	cwd        string
	env        object
	inheritEnv bool
	cols, rows uint16
}

func terminalSize(json any, key string) (uint16, *failure) {
	value, ok := asU64(get(json, key))
	if !ok || value < 1 || value > maxCells {
		return 0, newFailure("EINVAL", key+" must be between 1 and "+strconv.Itoa(maxCells))
	}
	return uint16(value), nil
}

func ptyRequestFromJSON(json any) (ptyRequest, *failure) {
	var r ptyRequest
	if argv := get(json, "argv"); argv != nil {
		items, ok := asStrings(argv)
		if !ok || len(items) == 0 {
			return r, newFailure("EINVAL", "argv must be a non-empty array of strings")
		}
		r.argv, r.hasArgv = items, true
	}
	cwd, ok := asString(get(json, "cwd"))
	if !ok {
		return r, newFailure("EINVAL", "pty needs cwd")
	}
	r.cwd = cwd
	r.env, _ = asObject(get(json, "env"))
	r.inheritEnv = true
	if inherit, ok := asBool(get(json, "inheritEnv")); ok {
		r.inheritEnv = inherit
	}
	var fail *failure
	if r.cols, fail = terminalSize(json, "cols"); fail != nil {
		return r, fail
	}
	if r.rows, fail = terminalSize(json, "rows"); fail != nil {
		return r, fail
	}
	return r, nil
}

// inputQueue is a terminal's unbounded input, written in arrival order by one
// writer, so queueing input never holds up the thread that reads frames.
type inputQueue struct {
	mu     sync.Mutex
	chunks [][]byte
	closed bool
	ready  chan struct{}
}

func newInputQueue() *inputQueue { return &inputQueue{ready: make(chan struct{}, 1)} }

// push queues input; false once the writer has stopped.
func (q *inputQueue) push(b []byte) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.chunks = append(q.chunks, b)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true
}

// pop waits for the next input; false once closed.
func (q *inputQueue) pop() ([]byte, bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		if len(q.chunks) > 0 {
			next := q.chunks[0]
			q.chunks[0] = nil
			q.chunks = q.chunks[1:]
			q.mu.Unlock()
			return next, true
		}
		q.mu.Unlock()
		<-q.ready
	}
}

func (q *inputQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.chunks = nil
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// ptyWrite queues input for the terminal; it is written in the order it
// arrives, without holding up other requests.
func ptyWrite(t *terminals, json any, payload []byte) (object, *failure) {
	terminal, fail := t.find(json)
	if fail != nil {
		return nil, fail
	}
	if !terminal.input.push(payload) {
		return nil, newFailure("EBADF", "the terminal has ended")
	}
	return object{}, nil
}

// ptyResize changes the terminal's size; the program receives SIGWINCH.
func ptyResize(t *terminals, json any) (object, *failure) {
	terminal, fail := t.find(json)
	if fail != nil {
		return nil, fail
	}
	cols, fail := terminalSize(json, "cols")
	if fail != nil {
		return nil, fail
	}
	rows, fail := terminalSize(json, "rows")
	if fail != nil {
		return nil, fail
	}
	if err := terminal.setSize(cols, rows); err != nil {
		return nil, ioFailure(err, "ioctl", "pty")
	}
	return object{}, nil
}

// ptyForeground reports the process group in the terminal's foreground when it
// is not the program's own: a job a shell started, such as an editor.
// { foreground: null } while the program itself is in front (a shell at its
// prompt).
func ptyForeground(t *terminals, json any) (object, *failure) {
	terminal, fail := t.find(json)
	if fail != nil {
		return nil, fail
	}
	group, supported, err := terminal.foreground()
	if err != nil {
		return nil, ioFailure(err, "tcgetpgrp", "pty")
	}
	if !supported || group <= 0 || group == terminal.pid {
		return object{"foreground": nil}, nil
	}
	var name any
	if processName, ok := processName(group); ok {
		name = processName
	}
	return object{"foreground": object{"pid": int64(group), "name": name}}, nil
}
