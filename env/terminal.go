package env

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// terminalWriteChunk: input larger than this goes to the daemon in several
// writes; frames are at most 16 MiB.
const terminalWriteChunk = 1024 * 1024

// TerminalOptions configure OpenTerminal.
type TerminalOptions struct {
	// Cwd is the working directory of the program; it must exist.
	Cwd string
	// Argv is the program and its arguments; nil is the user's shell (SHELL
	// in Env or the daemon's environment) as a login shell.
	Argv []string
	// Env is added to the daemon's environment, or is the whole environment
	// with InheritEnv false. Set TERM here.
	Env map[string]string
	// InheritEnv nil is the daemon's default (true).
	InheritEnv *bool
	Cols       int
	Rows       int
	// OnData receives the terminal's raw output, in order, from one
	// goroutine.
	OnData func(data []byte)
}

// TerminalForeground is the foreground job of a terminal: its process group
// id, and its leader's name if the system tells.
type TerminalForeground struct {
	PID  int
	Name string
}

// RemoteTerminal is a program running in a pseudo-terminal of a pi-env
// daemon. It ends when the program exits, Close kills it, or the connection
// is lost (the daemon then kills it).
type RemoteTerminal struct {
	// PID is the program's process id.
	PID        int
	connection *Connection
	id         uint32
	session    int
	abort      context.CancelFunc

	exited   chan struct{}
	exitCode int
	exitErr  error
}

// Wait waits for the program to end and returns its exit code; it fails with
// the daemon's error, with code aborted after Close.
func (t *RemoteTerminal) Wait() (int, error) {
	<-t.exited
	return t.exitCode, t.exitErr
}

// Done is closed once the terminal has ended.
func (t *RemoteTerminal) Done() <-chan struct{} { return t.exited }

// Write sends input; once the terminal has ended, input is dropped. Input
// keeps its order across calls.
func (t *RemoteTerminal) Write(data []byte) {
	for offset := 0; offset < len(data); offset += terminalWriteChunk {
		t.request("ptyWrite", object{}, data[offset:min(offset+terminalWriteChunk, len(data))])
	}
}

// Resize changes the size; the program receives SIGWINCH.
func (t *RemoteTerminal) Resize(cols, rows int) {
	t.request("ptyResize", object{}.with("cols", cols).with("rows", rows), nil)
}

// Close kills the program and everything in its session.
func (t *RemoteTerminal) Close() { t.abort() }

// Foreground is the job in the terminal's foreground when it is not the
// program itself: what a shell runs, such as an editor. It is nil while the
// program is in front (a shell at its prompt), and fails once the terminal
// has ended.
func (t *RemoteTerminal) Foreground(ctx context.Context) (*TerminalForeground, error) {
	reply, err := t.connection.Request(ctx, "ptyForeground", object{}.with("target", t.id), &RequestOptions{Session: t.session})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Foreground *struct {
			PID  int     `json:"pid"`
			Name *string `json:"name"`
		} `json:"foreground"`
	}
	if err := json.Unmarshal(reply.JSON, &answer); err != nil {
		return nil, err
	}
	if answer.Foreground == nil {
		return nil, nil
	}
	foreground := &TerminalForeground{PID: answer.Foreground.PID}
	if answer.Foreground.Name != nil {
		foreground.Name = *answer.Foreground.Name
	}
	return foreground, nil
}

// request sends a request about the terminal without waiting for it: a
// terminal that ended or a lost connection is what Wait reports.
func (t *RemoteTerminal) request(op string, fields object, payload []byte) {
	_, _ = t.connection.begin(context.Background(), op, fields.with("target", t.id), &RequestOptions{Session: t.session, Payload: payload})
}

// OpenTerminal starts a program in a new pseudo-terminal of the daemon
// behind connection. It returns once the program started; ending ctx before
// then kills it.
func OpenTerminal(ctx context.Context, connection *Connection, options TerminalOptions) (*RemoteTerminal, error) {
	fields := object{}.with("cwd", options.Cwd)
	if options.Argv != nil {
		fields = fields.with("argv", options.Argv)
	}
	if options.Env != nil {
		fields = fields.with("env", options.Env)
	}
	if options.InheritEnv != nil {
		fields = fields.with("inheritEnv", *options.InheritEnv)
	}
	fields = fields.with("cols", options.Cols).with("rows", options.Rows)

	abortCtx, abort := context.WithCancel(context.Background())
	terminal := &RemoteTerminal{connection: connection, abort: abort, exited: make(chan struct{})}
	started := make(chan struct{})
	var startOnce sync.Once
	go func() {
		defer abort()
		reply, err := connection.Request(abortCtx, "pty", fields, &RequestOptions{
			OnStart: func(id uint32, session int) {
				terminal.id, terminal.session = id, session
			},
			OnEvent: func(event json.RawMessage, payload []byte) {
				var e struct {
					Kind string  `json:"kind"`
					PID  float64 `json:"pid"`
				}
				if json.Unmarshal(event, &e) != nil {
					return
				}
				switch e.Kind {
				case "output":
					if options.OnData != nil {
						options.OnData(payload)
					}
				case "started":
					startOnce.Do(func() {
						terminal.PID = int(e.PID)
						close(started)
					})
				}
			},
		})
		if err == nil {
			var result struct {
				ExitCode float64 `json:"exitCode"`
			}
			if decodeErr := json.Unmarshal(reply.JSON, &result); decodeErr != nil {
				err = decodeErr
			}
			terminal.exitCode = int(result.ExitCode)
		}
		terminal.exitErr = err
		close(terminal.exited)
	}()
	select {
	case <-started:
		return terminal, nil
	case <-terminal.exited:
		// Failing before it started fails the open; afterwards, Wait
		// carries it.
		select {
		case <-started:
			return terminal, nil
		default:
		}
		if terminal.exitErr == nil {
			return nil, errors.New("pi-env ended the terminal before it started")
		}
		return nil, terminal.exitErr
	case <-ctx.Done():
		abort()
		<-terminal.exited
		return nil, ctx.Err()
	}
}
