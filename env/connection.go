package env

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
	"github.com/sky-valley/pi/internal/jstext"
)

const (
	pingInterval = 5 * time.Second
	// silenceLimit: the daemon pings every five seconds; this much silence
	// means the connection is gone.
	silenceLimit = 30 * time.Second
	// startTimeout is how long starting the daemon and its hello may take.
	startTimeout = 60 * time.Second
)

// RemoteError is a failure reported by the daemon, with its Node-style code,
// or a failure of the connection itself (IsConnectionLost).
type RemoteError struct {
	Code    string
	Message string
	// Path is the path the daemon named, if any.
	Path string
	// Fields are all the fields of the daemon's error.
	Fields map[string]any
}

func (e *RemoteError) Error() string { return e.Message }

// newRemoteError reads the daemon's error fields as the RemoteError
// constructor does.
func newRemoteError(fields map[string]any) *RemoteError {
	e := &RemoteError{Code: "unknown", Message: "Remote operation failed", Fields: fields}
	if message, ok := fields["message"].(string); ok {
		e.Message = message
	}
	if code, ok := fields["code"].(string); ok {
		e.Code = code
	}
	if path, ok := fields["path"].(string); ok {
		e.Path = path
	}
	return e
}

// lost is a failed or ended connection: a request may succeed once it is
// started again.
func lost(message string) *RemoteError {
	return newRemoteError(map[string]any{"code": "unknown", "message": message, "lost": true})
}

func closedError() *RemoteError {
	return newRemoteError(map[string]any{"code": "unknown", "message": "Connection closed"})
}

// IsConnectionLost reports whether err means the connection was lost, not
// that the daemon refused the request.
func IsConnectionLost(err error) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && remote.Fields["lost"] == true
}

// RemoteInfo is what hello reports about the remote machine.
type RemoteInfo struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Home     string `json:"home"`
	Tmpdir   string `json:"tmpdir"`
	// Separator is "/", or "\" on Windows.
	Separator string `json:"separator"`
	// Cwd is the daemon's working directory, where Node's path.resolve would
	// fall back to process.cwd().
	Cwd string `json:"cwd"`
	// DriveCwds are Windows' per-drive working directories (C: → C:\work)
	// of the =C: variables.
	DriveCwds map[string]string `json:"driveCwds"`
	PID       int               `json:"pid"`
	// Features are operations added to protocol 1 that this daemon supports,
	// such as listFiles; nil from older daemons.
	Features []string `json:"features,omitempty"`
}

// hasFeature reports whether the daemon supports an operation added to
// protocol 1.
func (info RemoteInfo) hasFeature(name string) bool {
	for _, feature := range info.Features {
		if feature == name {
			return true
		}
	}
	return false
}

// NodePlatform gives the daemon's system and architecture in Node's names,
// which hello reports in Rust's spelling: windows is win32, macos is darwin,
// x86_64 is x64, aarch64 is arm64, so they compare with process.platform and
// process.arch. Other names are kept.
func NodePlatform(info RemoteInfo) (platform, arch string) {
	platform, arch = info.OS, info.Arch
	switch info.OS {
	case "windows":
		platform = "win32"
	case "macos":
		platform = "darwin"
	}
	switch info.Arch {
	case "x86_64":
		arch = "x64"
	case "aarch64":
		arch = "arm64"
	case "x86":
		arch = "ia32"
	}
	return platform, arch
}

// ConnectionOptions configure a Connection.
type ConnectionOptions struct {
	// Command starts the daemon, before its `serve --token <hex>`
	// arguments, e.g. ["ssh", "-T", "--", "host", "~/.pi/mobile/tools/pi-env"].
	Command []string
	// CommandFunc, when set, computes the command at each start instead
	// (detecting and deploying first, for example); its failure fails that
	// start, and the next request tries again. Its context ends when the
	// connection is closed.
	CommandFunc func(ctx context.Context) ([]string, error)
	// OnLog receives the daemon's and the transport's diagnostic output.
	OnLog func(text string)
}

// Reply is the daemon's result of a request.
type Reply struct {
	JSON    json.RawMessage
	Payload []byte
	// Session is the daemon session that answered; handles from it are only
	// valid while it lives.
	Session int
}

// RequestOptions are the optional parts of a request.
type RequestOptions struct {
	Payload []byte
	// OnEvent receives the request's progress events, such as exec output,
	// in order, on the goroutine that waits for the request.
	OnEvent func(json json.RawMessage, payload []byte)
	// OnStart receives the request's id and session, for Kill.
	OnStart func(id uint32, session int)
	// Session, when not zero, runs the request only in this daemon session,
	// for requests on handles it opened. After the connection was lost and
	// started again, such requests fail instead of reaching a daemon that
	// never opened the handle.
	Session int
}

// Connection is one connection to a pi-env daemon: started lazily on the
// first request and started again after it is lost. Requests in flight when
// it is lost fail with code unknown; for mutations their outcome is then
// unknown. A Connection is safe for concurrent use.
type Connection struct {
	options ConnectionOptions
	// life ends with Close, and with it any start in progress.
	life     context.Context
	stopLife context.CancelFunc

	mu       sync.Mutex
	session  *session
	ready    *starting
	sessions int
	nextID   uint32
	closed   bool
}

// NewConnection returns a connection that starts nothing until its first
// request.
func NewConnection(options ConnectionOptions) *Connection {
	life, stop := context.WithCancel(context.Background())
	return &Connection{options: options, life: life, stopLife: stop, nextID: 1}
}

// starting is a start in progress, shared by the requests that wait for it.
type starting struct {
	done    chan struct{}
	info    RemoteInfo
	session *session
	err     error
}

// session is one running daemon.
type session struct {
	id      int
	process *os.Process
	stdin   io.WriteCloser
	token   string
	writeMu sync.Mutex
	// pending and live are guarded by Connection.mu.
	pending  map[uint32]*call
	live     bool
	lastSeen atomic.Int64
	stop     chan struct{}
	// exited is closed once the process has exited, after exitCode is set.
	exited   chan struct{}
	exitCode string
}

// Info connects if needed and returns what the daemon reported about its
// machine.
func (c *Connection) Info(ctx context.Context) (RemoteInfo, error) {
	st, err := c.connect(ctx)
	if err != nil {
		return RemoteInfo{}, err
	}
	return st.info, nil
}

// Session connects if needed and returns the live session's id.
func (c *Connection) Session(ctx context.Context) (int, error) {
	st, err := c.connect(ctx)
	if err != nil {
		return 0, err
	}
	return st.session.id, nil
}

// Request sends a request for op with fields (a JSON object; nil is {}) and
// waits for its result, starting the daemon first if needed. Ending ctx
// sends cancel; the request still settles with the daemon's result or error,
// as upstream's abort signal does. Starting the daemon is not cut short by
// ctx (it has its own time limits); Info bounds that wait by its context.
func (c *Connection) Request(ctx context.Context, op string, fields any, options *RequestOptions) (Reply, error) {
	call, err := c.begin(ctx, op, fields, options)
	if err != nil {
		return Reply{}, err
	}
	return call.wait(ctx)
}

// begin connects if needed and sends the request; the returned call waits for
// its result. Requests are written in the order begin is called.
func (c *Connection) begin(ctx context.Context, op string, fields any, options *RequestOptions) (*call, error) {
	if options == nil {
		options = &RequestOptions{}
	}
	body, err := requestJSON(op, fields)
	if err != nil {
		return nil, err
	}
	var s *session
	if options.Session != 0 {
		c.mu.Lock()
		current := c.session
		live := current != nil && current.live
		c.mu.Unlock()
		if current == nil || current.id != options.Session || !live {
			return nil, lost("pi-env connection lost")
		}
		s = current
	} else {
		// As upstream, waiting for the start is not cut short; ctx then
		// cancels the request itself, which settles with the daemon's answer.
		st, err := c.connect(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		s = st.session
	}
	return c.send(s, body, options)
}

// requestJSON is {...fields, op}: the fields as JSON with op last.
func requestJSON(op string, fields any) ([]byte, error) {
	var object []byte
	if fields != nil {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(fields); err != nil {
			return nil, fmt.Errorf("encoding the %s request: %w", op, err)
		}
		object = bytes.TrimSpace(buf.Bytes())
		if len(object) < 2 || object[0] != '{' {
			return nil, fmt.Errorf("the fields of a %s request must be a JSON object, not %s", op, object)
		}
	}
	opJSON, _ := json.Marshal(op)
	if len(object) <= 2 || string(bytes.TrimSpace(object[1:len(object)-1])) == "" {
		return append(append([]byte(`{"op":`), opJSON...), '}'), nil
	}
	out := append([]byte(nil), object[:len(object)-1]...)
	out = append(out, `,"op":`...)
	out = append(append(out, opJSON...), '}')
	return out, nil
}

// Close stops the daemon; it kills everything it started.
func (c *Connection) Close() {
	c.mu.Lock()
	c.closed = true
	s := c.session
	c.mu.Unlock()
	c.stopLife()
	if s != nil {
		c.teardown(s, closedError())
	}
}

// Kill kills one exec without aborting it.
func (c *Connection) Kill(id uint32, session int) {
	c.mu.Lock()
	current := c.session
	live := current != nil && current.id == session && current.live
	c.mu.Unlock()
	if live {
		c.write(current, frame.Frame{Kind: frame.Cancel, ID: id, JSON: []byte(`{"mode":"kill"}`)})
	}
}

func (c *Connection) connect(ctx context.Context) (*starting, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, closedError()
	}
	st := c.ready
	if st == nil {
		st = &starting{done: make(chan struct{})}
		c.ready = st
		go c.start(st)
	}
	c.mu.Unlock()
	select {
	case <-st.done:
		if st.err != nil {
			return nil, st.err
		}
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// start starts a daemon; a failed start is not remembered, so the next
// request tries again.
func (c *Connection) start(st *starting) {
	info, s, err := c.startSession()
	c.mu.Lock()
	if err != nil && c.ready == st {
		c.ready = nil
	}
	st.info, st.session, st.err = info, s, err
	c.mu.Unlock()
	close(st.done)
}

func (c *Connection) startSession() (RemoteInfo, *session, error) {
	command := c.options.Command
	if c.options.CommandFunc != nil {
		computed, err := c.options.CommandFunc(c.life)
		if err != nil {
			if c.isClosed() {
				return RemoteInfo{}, nil, closedError()
			}
			return RemoteInfo{}, nil, newRemoteError(map[string]any{"code": "spawn_error", "message": err.Error(), "lost": true})
		}
		command = computed
	}
	if c.isClosed() {
		return RemoteInfo{}, nil, closedError()
	}
	if len(command) == 0 {
		return RemoteInfo{}, nil, newRemoteError(map[string]any{"code": "spawn_error", "message": "No daemon command"})
	}
	tokenBytes := make([]byte, 16)
	_, _ = rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)
	cmd := exec.Command(command[0], append(append([]string(nil), command[1:]...), "serve", "--token", token)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return RemoteInfo{}, nil, spawnError(err, command[0])
	}
	// The parent's ends of stdout and stderr are its own: Wait returns when
	// the process exits, even if a child of it still holds them, as Node's
	// exit event does.
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return RemoteInfo{}, nil, spawnError(err, command[0])
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		return RemoteInfo{}, nil, spawnError(err, command[0])
	}
	cmd.Stdout, cmd.Stderr = stdoutWrite, stderrWrite
	err = cmd.Start()
	stdoutWrite.Close()
	stderrWrite.Close()
	if err != nil {
		stdoutRead.Close()
		stderrRead.Close()
		return RemoteInfo{}, nil, spawnError(err, command[0])
	}

	c.mu.Lock()
	c.sessions++
	s := &session{
		id:      c.sessions,
		process: cmd.Process,
		stdin:   stdin,
		token:   token,
		pending: map[uint32]*call{},
		live:    true,
		stop:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	s.lastSeen.Store(time.Now().UnixNano())
	closed := c.closed
	if !closed {
		c.session = s
	}
	c.mu.Unlock()

	go c.readFrames(s, stdoutRead)
	go c.readLog(stderrRead)
	go func() {
		state, _ := cmd.Process.Wait()
		s.exitCode = "null"
		if state != nil && state.ExitCode() >= 0 {
			s.exitCode = strconv.Itoa(state.ExitCode())
		}
		close(s.exited)
		c.teardown(s, lost("pi-env connection lost"))
	}()
	go c.ping(s)
	if closed {
		c.teardown(s, closedError())
		return RemoteInfo{}, nil, closedError()
	}

	hello, err := c.send(s, []byte(`{"protocol":1,"op":"hello"}`), &RequestOptions{})
	var reply Reply
	if err == nil {
		reply, err = c.awaitHello(s, hello)
	}
	if err == nil {
		var info RemoteInfo
		var protocol struct {
			Protocol json.RawMessage `json:"protocol"`
		}
		_ = json.Unmarshal(reply.JSON, &protocol)
		if n, ok := jsNumber(protocol.Protocol); !ok || n != 1 {
			err = lost("Unsupported protocol " + jsString(protocol.Protocol))
		} else if decodeErr := json.Unmarshal(reply.JSON, &info); decodeErr != nil {
			err = lost("Unexpected hello from pi-env: " + decodeErr.Error())
		} else {
			return info, s, nil
		}
	}
	c.teardown(s, err)
	return RemoteInfo{}, nil, err
}

// awaitHello waits for the hello's answer or the start timeout. A daemon
// that exits first fails the hello through the teardown that follows its
// exit; the exit is then the reason given.
func (c *Connection) awaitHello(s *session, hello *call) (Reply, error) {
	type answer struct {
		reply Reply
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		reply, err := hello.wait(context.Background())
		answered <- answer{reply, err}
	}()
	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case a := <-answered:
		if a.err != nil {
			select {
			case <-s.exited:
				// exited is closed before the teardown that failed the hello.
				return Reply{}, lost("pi-env exited with code " + s.exitCode + " before it was ready")
			default:
			}
		}
		return a.reply, a.err
	case <-timer.C:
		return Reply{}, lost(fmt.Sprintf("pi-env did not answer within %d s", startTimeout/time.Second))
	}
}

// spawnError is Node's message for a program that could not be started.
func spawnError(err error, program string) *RemoteError {
	message := err.Error()
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		message = "spawn " + program + " ENOENT"
	case errors.Is(err, fs.ErrPermission):
		message = "spawn " + program + " EACCES"
	}
	return newRemoteError(map[string]any{"code": "spawn_error", "message": message, "lost": true})
}

func (c *Connection) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// send registers a request in s and writes it.
func (c *Connection) send(s *session, body []byte, options *RequestOptions) (*call, error) {
	c.mu.Lock()
	if !s.live {
		c.mu.Unlock()
		return nil, lost("pi-env connection lost")
	}
	id := c.nextID
	c.nextID++
	call := &call{connection: c, session: s, id: id, signal: make(chan struct{}, 1), onEvent: options.OnEvent}
	s.pending[id] = call
	c.mu.Unlock()
	if options.OnStart != nil {
		options.OnStart(id, s.id)
	}
	c.write(s, frame.Frame{Kind: frame.Request, ID: id, JSON: body, Payload: options.Payload})
	return call, nil
}

func (c *Connection) write(s *session, f frame.Frame) {
	c.mu.Lock()
	live := s.live
	c.mu.Unlock()
	if !live {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// A write to a daemon that exited fails; its requests fail with the exit.
	w := bufio.NewWriterSize(s.stdin, 13+len(f.JSON)+min(len(f.Payload), 64*1024))
	if frame.Write(w, f) == nil {
		_ = w.Flush()
	}
}

func (c *Connection) ping(s *session) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			c.write(s, frame.Frame{Kind: frame.Ping, JSON: []byte("{}")})
			if time.Since(time.Unix(0, s.lastSeen.Load())) > silenceLimit {
				c.teardown(s, lost("pi-env connection timed out"))
				return
			}
		}
	}
}

func (c *Connection) teardown(s *session, err error) {
	c.mu.Lock()
	if !s.live {
		c.mu.Unlock()
		return
	}
	s.live = false
	close(s.stop)
	if c.session == s {
		c.session = nil
		c.ready = nil
	}
	pending := s.pending
	s.pending = map[uint32]*call{}
	c.mu.Unlock()
	_ = s.stdin.Close()
	killProcess(s.process)
	for _, call := range pending {
		call.finish(Reply{}, err)
	}
}

// killProcess is Node's child.kill(): SIGTERM, or termination on Windows.
func killProcess(p *os.Process) {
	if runtime.GOOS == "windows" {
		_ = p.Kill()
		return
	}
	_ = p.Signal(syscall.SIGTERM)
}

func (c *Connection) readLog(stderr io.ReadCloser) {
	defer stderr.Close()
	buf := make([]byte, 64*1024)
	for {
		n, err := stderr.Read(buf)
		if n > 0 && c.options.OnLog != nil {
			c.options.OnLog(jstext.DecodeUTF8(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// seenReader notes when data last arrived.
type seenReader struct {
	r io.Reader
	s *session
}

func (r seenReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.s.lastSeen.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *Connection) readFrames(s *session, stdout io.ReadCloser) {
	defer stdout.Close()
	input := seenReader{stdout, s}
	// Shell startup files may print before the daemon runs; skip everything
	// before its sync line.
	marker := []byte("PI-ENV " + s.token + "\n")
	var buffered []byte
	chunk := make([]byte, 64*1024)
	for {
		n, err := input.Read(chunk)
		buffered = append(buffered, chunk[:n]...)
		if index := bytes.Index(buffered, marker); index != -1 {
			if noise := jstext.Trim(jstext.DecodeUTF8(buffered[:index])); noise != "" && c.options.OnLog != nil {
				c.options.OnLog(noise)
			}
			buffered = buffered[index+len(marker):]
			break
		}
		if len(buffered) > 1024*1024 {
			buffered = append([]byte(nil), buffered[len(buffered)-len(marker):]...)
		}
		if err != nil {
			return
		}
	}
	frames := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(buffered), input), 64*1024)
	for {
		f, err := frame.Read(frames)
		if errors.Is(err, frame.ErrCorrupt) {
			c.teardown(s, lost("Corrupt frame from pi-env"))
			return
		}
		if err != nil {
			return
		}
		object := f.JSON
		if object == nil {
			object = []byte("{}")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(object, &fields) != nil || fields == nil {
			c.teardown(s, lost("Corrupt frame from pi-env"))
			return
		}
		c.dispatch(s, f.Kind, f.ID, object, f.Payload)
	}
}

func (c *Connection) dispatch(s *session, kind byte, id uint32, object json.RawMessage, payload []byte) {
	c.mu.Lock()
	if !s.live {
		c.mu.Unlock()
		return
	}
	call := s.pending[id]
	switch kind {
	case frame.Event:
		c.mu.Unlock()
		if call != nil && call.onEvent != nil {
			call.push(item{event: true, json: object, payload: payload})
		}
		return
	case frame.Result, frame.Error:
	default:
		c.mu.Unlock()
		return
	}
	if call == nil {
		c.mu.Unlock()
		return
	}
	delete(s.pending, id)
	c.mu.Unlock()
	if kind == frame.Result {
		call.finish(Reply{JSON: object, Payload: payload, Session: s.id}, nil)
		return
	}
	var fields map[string]any
	_ = json.Unmarshal(object, &fields)
	call.finish(Reply{}, newRemoteError(fields))
}

// call is a request in flight: its events and result queue up until the
// goroutine that waits for it takes them, so callbacks never run on the
// connection's reader.
type call struct {
	connection *Connection
	session    *session
	id         uint32
	onEvent    func(json.RawMessage, []byte)

	mu     sync.Mutex
	items  []item
	signal chan struct{}
}

type item struct {
	event   bool
	json    json.RawMessage
	payload []byte
	reply   Reply
	err     error
}

func (c *call) push(it item) {
	c.mu.Lock()
	c.items = append(c.items, it)
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

func (c *call) finish(reply Reply, err error) { c.push(item{reply: reply, err: err}) }

// wait delivers the call's events to onEvent and returns its result. Ending
// ctx sends cancel once and keeps waiting for the daemon's answer.
func (c *call) wait(ctx context.Context) (Reply, error) {
	done := ctx.Done()
	if ctx.Err() != nil {
		c.cancel()
		done = nil
	}
	for {
		c.mu.Lock()
		items := c.items
		c.items = nil
		c.mu.Unlock()
		for _, it := range items {
			if !it.event {
				return it.reply, it.err
			}
			if c.onEvent != nil {
				c.onEvent(it.json, it.payload)
			}
		}
		select {
		case <-c.signal:
		case <-done:
			c.cancel()
			done = nil
		}
	}
}

func (c *call) cancel() {
	c.connection.write(c.session, frame.Frame{Kind: frame.Cancel, ID: c.id, JSON: []byte("{}")})
}

// jsNumber reads a JSON number.
func jsNumber(raw json.RawMessage) (float64, bool) {
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// jsString is String(value) of a JSON value in a template literal.
func jsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "undefined"
	}
	value, err := jstext.Parse(raw)
	if err != nil {
		return string(raw)
	}
	if s, ok := jstext.ToString(value); ok {
		return s
	}
	return string(raw)
}
