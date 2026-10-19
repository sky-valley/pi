// Package daemon is pi-env, a small daemon that gives a Pi Durable host an
// execution environment on this machine over stdin and stdout
// (docs/protocol.md). It is started as `pi-env serve --token <hex>`, usually
// through ssh, and is a port of upstream's Rust daemon
// (packages/env/daemon): its replies are byte-identical, and its results match
// Node's NodeExecutionEnv on this machine (docs/semantics.md).
package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

const (
	protocolVersion = 1
	// Version is the npm package version of @earendil-works/pi-env this
	// daemon matches; hello reports it.
	Version = "1.3.0"
	// pingInterval and silenceLimit: each side pings every 5 s; the daemon
	// stops everything and exits after 30 s without receiving a byte.
	pingInterval = 5 * time.Second
	silenceLimit = 30 * time.Second
	scanChunk    = 64 * 1024
	// workers is the number of goroutines for file operations; commands run
	// on their own.
	workers = 16
	// maxHandles is the open handles per connection, beyond which opening
	// fails with EMFILE.
	maxHandles = 4096
	// controlPayload: replies with larger payloads queue behind command output
	// instead of going first.
	controlPayload = 64 * 1024
)

// features are operations added after protocol 1's first release; clients
// check hello.features before using them.
var features = []string{"listFiles"}

// handle is an open file or directory of the connection, valid until close.
type handle interface{ close() }

// fileHandle is an open file.
type fileHandle struct {
	file *os.File
	path string
	// failed poisons a write handle after a failed chunk write, so later
	// chunks cannot leave a gap.
	failed atomic.Bool
	// serial runs chunk writes one at a time in arrival order, though the
	// client sends several at once.
	serial struct {
		sync.Mutex
		jobs    []func()
		running bool
	}
}

func (h *fileHandle) close() { h.file.Close() }

// dirHandle is an open directory, read a page at a time.
type dirHandle struct {
	mu   sync.Mutex
	dir  *os.File
	path string
	// buffered holds names read but not yet returned.
	buffered []string
	done     bool
}

func (h *dirHandle) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dir.Close()
}

type server struct {
	out        *output
	tmpdir     string
	mu         sync.Mutex
	handles    map[uint64]handle
	nextHandle atomic.Uint64
	controls   map[uint32]*control
	jobs       *jobQueue
	running    *groups
	terminals  *terminals
}

func missingField(key string) *failure {
	return newFailure("EINVAL", "missing field "+key)
}

func field(json any, key string) (string, *failure) {
	s, ok := asString(get(json, key))
	if !ok {
		return "", missingField(key)
	}
	return s, nil
}

func number(json any, key string) (uint64, *failure) {
	n, ok := asU64(get(json, key))
	if !ok {
		return 0, newFailure("EINVAL", "missing or invalid field "+key)
	}
	return n, nil
}

func boolField(json any, key string) bool {
	b, _ := asBool(get(json, key))
	return b
}

func (s *server) file(json any) (*fileHandle, *failure) {
	id, fail := number(json, "handle")
	if fail != nil {
		return nil, fail
	}
	s.mu.Lock()
	h := s.handles[id]
	s.mu.Unlock()
	switch h := h.(type) {
	case *fileHandle:
		return h, nil
	case *dirHandle:
		return nil, newFailure("EBADF", "not a file handle")
	default:
		return nil, newFailure("EBADF", "unknown handle")
	}
}

func (s *server) insert(h handle) (uint64, *failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.handles) >= maxHandles {
		return 0, newFailure("EMFILE", "EMFILE: too many open files")
	}
	id := s.nextHandle.Add(1) - 1
	s.handles[id] = h
	return id, nil
}

func (s *server) insertFile(f *os.File, path string) (uint64, *failure) {
	id, fail := s.insert(&fileHandle{file: f, path: path})
	if fail != nil {
		f.Close()
	}
	return id, fail
}

// dispatch runs one request and returns its result JSON and payload.
func (s *server) dispatch(id uint32, json any, payload []byte, ctl *control) (object, []byte, *failure) {
	if json == nil {
		return nil, nil, newFailure("EINVAL", "invalid JSON")
	}
	op, _ := asString(get(json, "op"))
	plain := func(result object, fail *failure) (object, []byte, *failure) {
		return result, nil, fail
	}
	path := func() (string, *failure) { return field(json, "path") }
	switch op {
	case "hello":
		return plain(object{
			"protocol":  uint64(protocolVersion),
			"version":   Version,
			"os":        osName(),
			"arch":      archName(),
			"home":      home(),
			"separator": pathSeparator,
			"tmpdir":    s.tmpdir,
			"cwd":       cwd(),
			"driveCwds": driveCwds(),
			"pid":       int64(os.Getpid()),
			"features":  features,
		}, nil)
	case "lstat":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(lstatOp(p))
	case "realpath":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(realpathOp(p))
	case "write":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		parents := true
		if value, ok := asBool(get(json, "parents")); ok {
			parents = value
		}
		f, fail := writeOp(p, boolField(json, "append"), parents, payload)
		if fail != nil {
			return plain(nil, fail)
		}
		if !boolField(json, "keep") {
			f.Close()
			return plain(object{}, nil)
		}
		handle, fail := s.insertFile(f, p)
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(object{"handle": handle}, nil)
	case "writeChunk":
		handle, fail := s.file(json)
		if fail != nil {
			return plain(nil, fail)
		}
		if handle.failed.Load() {
			return plain(nil, newFailure("EBADF", "an earlier write failed"))
		}
		if err := writeAll(handle.file, payload); err != nil {
			handle.failed.Store(true)
			return plain(nil, ioFailure(err, "write", handle.path))
		}
		return plain(object{}, nil)
	case "truncate":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		size, fail := number(json, "size")
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(truncateOp(p, size))
	case "fsync":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(fsyncOp(p))
	case "rename":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		to, fail := field(json, "to")
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(renameOp(p, to))
	case "mkdir":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(mkdirOp(p, boolField(json, "recursive")))
	case "rm":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(rmOp(p, boolField(json, "recursive"), boolField(json, "force")))
	case "mkdtemp":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(mkdtempOp(p))
	case "open":
		return plain(s.open(json))
	case "pread":
		handle, fail := s.file(json)
		if fail != nil {
			return plain(nil, fail)
		}
		length, fail := number(json, "length")
		if fail != nil {
			return plain(nil, fail)
		}
		length = min(length, frame.MaxPayload)
		var data []byte
		if offset, ok := asU64(get(json, "offset")); ok {
			data, fail = pread(handle.file, handle.path, offset, int(length))
		} else {
			data, fail = readCurrent(handle.file, handle.path, int(length))
		}
		if fail != nil {
			return nil, nil, fail
		}
		return object{}, data, nil
	case "fstat":
		handle, fail := s.file(json)
		if fail != nil {
			return plain(nil, fail)
		}
		st, err := fstatFile(handle.file)
		if err != nil {
			return plain(nil, ioFailure(err, "fstat", handle.path))
		}
		return plain(info(handle.path, st), nil)
	case "scanLines":
		handle, fail := s.file(json)
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(scanLines(handle.file, handle.path, json, ctl))
	case "opendir":
		p, fail := path()
		if fail != nil {
			return plain(nil, fail)
		}
		if hasNul(p) {
			return plain(nil, ioFailure(nulError, "opendir", p))
		}
		dir, err := openDir(p)
		if err != nil {
			return plain(nil, ioFailure(err, "opendir", p))
		}
		handle, fail := s.insert(&dirHandle{dir: dir, path: p})
		if fail != nil {
			dir.Close()
			return plain(nil, fail)
		}
		return plain(object{"handle": handle}, nil)
	case "readdir":
		return plain(s.readDir(json))
	case "listFiles":
		return listFilesOp(json, ctl.aborted.Load)
	case "close":
		id, fail := number(json, "handle")
		if fail != nil {
			return plain(nil, fail)
		}
		s.mu.Lock()
		h := s.handles[id]
		delete(s.handles, id)
		s.mu.Unlock()
		if h != nil {
			// Closed once no request uses it any more, as dropping the last
			// reference does: a read blocked on a FIFO does not hold up the
			// reply.
			go h.close()
		}
		return plain(object{}, nil)
	case "exec":
		request, fail := execRequestFromJSON(json)
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(runExec(id, request, s.tmpdir, s.out, ctl, s.running))
	case "watch":
		return plain(runWatch(id, json, s.out, ctl))
	case "pty":
		request, fail := ptyRequestFromJSON(json)
		if fail != nil {
			return plain(nil, fail)
		}
		return plain(runPty(id, request, s.out, ctl, s.running, s.terminals))
	}
	return plain(nil, newFailure("EINVAL", "unknown operation "+op))
}

// open is Node's open(path, "r") with mode "read": any kind of file,
// blocking, reporting what the file is. Otherwise it opens a reader for
// openBinaryReader: nonblocking, regular files only.
func (s *server) open(json any) (object, *failure) {
	path, fail := field(json, "path")
	if fail != nil {
		return nil, fail
	}
	if mode, _ := asString(get(json, "mode")); mode == "read" {
		if hasNul(path) {
			return nil, ioFailure(nulError, "open", path)
		}
		f, err := openFile(path, modeRead)
		if err != nil {
			return nil, ioFailure(err, "open", path)
		}
		result := object{}
		if st, err := fstatFile(f); err != nil {
			result["statError"] = ioFailure(err, "fstat", path).json()
		} else {
			result["stat"] = info(path, st)
		}
		handle, fail := s.insertFile(f, path)
		if fail != nil {
			return nil, fail
		}
		result["handle"] = handle
		return result, nil
	}
	f, information, fail := openReaderOp(path, boolField(json, "noFollow"))
	if fail != nil {
		return nil, fail
	}
	handle, fail := s.insertFile(f, path)
	if fail != nil {
		return nil, fail
	}
	return object{"handle": handle, "info": information}, nil
}

// readDir returns up to max entries of a directory handle; done once the
// directory is exhausted.
func (s *server) readDir(json any) (object, *failure) {
	limit, fail := number(json, "max")
	if fail != nil {
		return nil, fail
	}
	limit = min(max(limit, 1), uint64(1<<31))
	id, fail := number(json, "handle")
	if fail != nil {
		return nil, fail
	}
	// Lock only this directory, so other requests are not held up by a slow
	// listing.
	s.mu.Lock()
	dir, ok := s.handles[id].(*dirHandle)
	s.mu.Unlock()
	if !ok {
		return nil, newFailure("EBADF", "not a directory handle")
	}
	dir.mu.Lock()
	defer dir.mu.Unlock()
	entries := []any{}
	done := false
	for uint64(len(entries)) < limit {
		if len(dir.buffered) == 0 && !dir.done {
			names, err := dir.dir.Readdirnames(int(min(limit-uint64(len(entries)), 1024)))
			dir.buffered = names
			if err == io.EOF || (err == nil && len(names) == 0) {
				dir.done = true
			} else if err != nil {
				if len(names) == 0 {
					return nil, ioFailure(unwrapPathError(err), "readdir", dir.path)
				}
			}
		}
		if len(dir.buffered) == 0 {
			done = true
			break
		}
		name := dir.buffered[0]
		dir.buffered = dir.buffered[1:]
		entries = append(entries, dirEntry(dir.path, name))
	}
	return object{"entries": entries, "done": done}, nil
}

func scanLines(f *os.File, path string, json any, ctl *control) (object, *failure) {
	startLine, fail := number(json, "startLine")
	if fail != nil {
		return nil, fail
	}
	endLine, hasEnd := asU64(get(json, "endLine"))
	if hasEnd && endLine <= startLine {
		return nil, newFailure("EINVAL", "Invalid line range")
	}
	scanner := newLineScanner(startLine, endLine, hasEnd)
	buf := make([]byte, scanChunk)
	var position int64
	for {
		if ctl.aborted.Load() {
			return nil, newFailure("aborted", "aborted")
		}
		n, err := readAt(f, buf, position)
		if err != nil {
			return nil, ioFailure(err, "read", path)
		}
		if n == 0 {
			return scanner.finish(), nil
		}
		scanner.push(buf[:n])
		position += int64(n)
	}
}

// jobQueue is the file operations' worker pool: an unbounded queue, so the
// goroutine reading frames never waits for a worker.
type jobQueue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	queue []func()
}

func startWorkers() *jobQueue {
	q := &jobQueue{}
	q.cond = sync.NewCond(&q.mu)
	for range workers {
		go func() {
			for {
				q.mu.Lock()
				for len(q.queue) == 0 {
					q.cond.Wait()
				}
				job := q.queue[0]
				q.queue[0] = nil
				q.queue = q.queue[1:]
				q.mu.Unlock()
				job()
			}
		}()
	}
	return q
}

func (q *jobQueue) send(job func()) {
	q.mu.Lock()
	q.queue = append(q.queue, job)
	q.mu.Unlock()
	q.cond.Signal()
}

// runInOrder queues job behind the handle's earlier chunk writes; one pool
// job drains the queue.
func (s *server) runInOrder(handle *fileHandle, job func()) {
	serial := &handle.serial
	serial.Lock()
	serial.jobs = append(serial.jobs, job)
	if serial.running {
		serial.Unlock()
		return
	}
	serial.running = true
	serial.Unlock()
	s.jobs.send(func() {
		for {
			serial.Lock()
			if len(serial.jobs) == 0 {
				serial.running = false
				serial.Unlock()
				return
			}
			next := serial.jobs[0]
			serial.jobs[0] = nil
			serial.jobs = serial.jobs[1:]
			serial.Unlock()
			next()
		}
	})
}

func (s *server) respond(id uint32, op string, json any, payload []byte, ctl *control) {
	result, data, fail := s.dispatch(id, json, payload, ctl)
	var reply frame.Frame
	if fail != nil {
		reply = frame.Frame{Kind: frame.Error, ID: id, JSON: marshal(fail.json())}
	} else {
		reply = frame.Frame{Kind: frame.Result, ID: id, JSON: marshal(result), Payload: data}
	}
	s.mu.Lock()
	delete(s.controls, id)
	s.mu.Unlock()
	// A command's result follows its output; large payloads do not hold up
	// pings and small replies.
	if op == "exec" || op == "pty" || len(reply.Payload) > controlPayload {
		s.out.sendBulk(reply, nil)
	} else {
		s.out.sendControl(reply)
	}
}

func (s *server) serveFrames(input io.Reader) error {
	for {
		request, err := frame.Read(input)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch request.Kind {
		case frame.Request:
			json := parseRequestJSON(request.JSON)
			op, _ := asString(get(json, "op"))
			switch op {
			case "ptyWrite", "ptyResize", "ptyForeground":
				// Answered here, in arrival order: through the worker pool, two
				// writes could swap.
				var result object
				var fail *failure
				switch op {
				case "ptyWrite":
					result, fail = ptyWrite(s.terminals, json, request.Payload)
				case "ptyResize":
					result, fail = ptyResize(s.terminals, json)
				default:
					result, fail = ptyForeground(s.terminals, json)
				}
				if fail != nil {
					s.out.sendControl(frame.Frame{Kind: frame.Error, ID: request.ID, JSON: marshal(fail.json())})
				} else {
					s.out.sendControl(frame.Frame{Kind: frame.Result, ID: request.ID, JSON: marshal(result)})
				}
				continue
			}
			id, payload := request.ID, request.Payload
			// Registered before the request runs, so a cancel right behind it
			// is not lost.
			ctl := newControl()
			s.mu.Lock()
			s.controls[id] = ctl
			s.mu.Unlock()
			respond := func() { s.respond(id, op, json, payload, ctl) }
			switch op {
			case "exec", "watch", "pty":
				// Commands and watchers last; they get their own goroutines.
				go respond()
			case "writeChunk":
				if handle, fail := s.file(json); fail == nil {
					s.runInOrder(handle, respond)
				} else {
					s.jobs.send(respond)
				}
			default:
				s.jobs.send(respond)
			}
		case frame.Cancel:
			s.mu.Lock()
			ctl := s.controls[request.ID]
			s.mu.Unlock()
			if ctl != nil {
				mode, _ := asString(get(parseRequestJSON(request.JSON), "mode"))
				ctl.cancel(mode == "kill")
			}
		}
	}
}

// parseRequestJSON is a frame's JSON: {} when empty, nil when it does not
// parse.
func parseRequestJSON(data []byte) any {
	if len(data) == 0 {
		return object{}
	}
	return parseJSON(data)
}

// seenReader records when bytes last arrived, so a large frame arriving
// slowly counts as a live client.
type seenReader struct {
	r        io.Reader
	lastSeen *atomic.Int64
}

func (r seenReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	if n > 0 {
		r.lastSeen.Store(time.Now().UnixMilli())
	}
	return n, err
}

// serve speaks the protocol on stdin and stdout until stdin ends, then stops
// everything it started.
func serve(token string, stdin io.Reader, stdout io.Writer) error {
	if _, err := fmt.Fprintf(stdout, "PI-ENV %s\n", token); err != nil {
		return err
	}
	out := newOutput()
	writer := bufio.NewWriterSize(stdout, 1<<20)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		out.run(writer)
	}()
	s := &server{
		out:       out,
		tmpdir:    tmpdir(),
		handles:   map[uint64]handle{},
		controls:  map[uint32]*control{},
		jobs:      startWorkers(),
		running:   newGroups(),
		terminals: newTerminals(),
	}
	s.nextHandle.Store(1)
	lastSeen := new(atomic.Int64)
	lastSeen.Store(time.Now().UnixMilli())
	go func() {
		for {
			time.Sleep(pingInterval)
			out.sendControl(frame.Frame{Kind: frame.Ping, JSON: marshal(object{})})
			// A client that went silent (a phone that lost its network) leaves
			// nothing running.
			if time.Since(time.UnixMilli(lastSeen.Load())) > silenceLimit {
				s.running.killAll()
				os.Exit(0)
			}
		}
	}()
	err := s.serveFrames(bufio.NewReaderSize(seenReader{stdin, lastSeen}, 1<<20))
	// However input ended, the client is gone: stop everything it started.
	s.running.killAll()
	out.close()
	<-writerDone
	return err
}

const usage = "usage: pi-env serve --token <hex>"

// Main runs the daemon with the command line args (args[0] is the program)
// and returns the exit code.
func Main(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[1] {
	case "serve":
		token, found := "", false
		for i, arg := range args {
			if arg == "--token" && i+1 < len(args) {
				token, found = args[i+1], true
				break
			}
		}
		if !found {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		prepareSignals()
		if err := serve(token, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "pi-env: %v\n", err)
			return 1
		}
		// Request goroutines may still be blocked (a FIFO open); nothing is
		// left to answer them.
		return 0
	case "--version":
		fmt.Println("pi-env " + Version)
		return 0
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}
