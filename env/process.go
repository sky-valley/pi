package env

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// processWriteChunk: input larger than this goes to the daemon in several
// writes; frames are at most 16 MiB.
const processWriteChunk = 1024 * 1024

// ProcessOptions configure OpenProcess.
type ProcessOptions struct {
	// Cwd is the working directory of the program; it must exist.
	Cwd string
	// Argv is the program and its arguments, run directly without a shell.
	Argv []string
	// Env is added to the daemon's environment, or is the whole environment
	// with InheritEnv false.
	Env map[string]string
	// InheritEnv nil is the daemon's default (true).
	InheritEnv *bool
	// OnStdout receives the program's stdout, raw bytes in order, from one
	// goroutine.
	OnStdout func(data []byte)
	// OnStderr receives the program's stderr, raw bytes in order, from the
	// same goroutine; nil drops it.
	OnStderr func(data []byte)
}

// RemoteProcess is a program running with plain pipes in a pi-env daemon,
// for programs that speak a protocol on stdin and stdout, such as an MCP
// server. It ends when the program exits, Close kills it, or the connection
// is lost (the daemon then kills it).
type RemoteProcess struct {
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
func (p *RemoteProcess) Wait() (int, error) {
	<-p.exited
	return p.exitCode, p.exitErr
}

// Done is closed once the program has ended.
func (p *RemoteProcess) Done() <-chan struct{} { return p.exited }

// Write writes to the program's stdin, in order across calls; once stdin is
// closed or the program has ended, input is dropped.
func (p *RemoteProcess) Write(data []byte) {
	for offset := 0; offset < len(data); offset += processWriteChunk {
		p.request("processWrite", data[offset:min(offset+processWriteChunk, len(data))])
	}
}

// CloseStdin closes the program's stdin after the input written before it.
func (p *RemoteProcess) CloseStdin() { p.request("processCloseStdin", nil) }

// Close kills the program and its process group.
func (p *RemoteProcess) Close() { p.abort() }

// request sends a request about the program without waiting for it: stdin
// already closed, the program ended, or a lost connection is what Wait
// reports.
func (p *RemoteProcess) request(op string, payload []byte) {
	_, _ = p.connection.begin(context.Background(), op, object{}.with("target", p.id), &RequestOptions{Session: p.session, Payload: payload})
}

// OpenProcess starts a program with plain pipes in the daemon behind
// connection. It returns once the program started; ending ctx before then
// kills it. It fails with not_supported when the daemon was released before
// the process operation.
func OpenProcess(ctx context.Context, connection *Connection, options ProcessOptions) (*RemoteProcess, error) {
	info, err := connection.Info(ctx)
	if err != nil {
		return nil, err
	}
	if !info.hasFeature("process") {
		return nil, newRemoteError(map[string]any{"code": "not_supported", "message": "The pi-env daemon cannot run processes; update it"})
	}
	fields := object{}.with("cwd", options.Cwd).with("argv", options.Argv)
	if options.Env != nil {
		fields = fields.with("env", options.Env)
	}
	if options.InheritEnv != nil {
		fields = fields.with("inheritEnv", *options.InheritEnv)
	}

	abortCtx, abort := context.WithCancel(context.Background())
	process := &RemoteProcess{connection: connection, abort: abort, exited: make(chan struct{})}
	started := make(chan struct{})
	var startOnce sync.Once
	go func() {
		defer abort()
		reply, err := connection.Request(abortCtx, "process", fields, &RequestOptions{
			OnStart: func(id uint32, session int) {
				process.id, process.session = id, session
			},
			OnEvent: func(event json.RawMessage, payload []byte) {
				var e struct {
					Kind   string  `json:"kind"`
					Stream string  `json:"stream"`
					PID    float64 `json:"pid"`
				}
				if json.Unmarshal(event, &e) != nil {
					return
				}
				switch e.Kind {
				case "output":
					if e.Stream != "stderr" {
						options.OnStdout(payload)
					} else if options.OnStderr != nil {
						options.OnStderr(payload)
					}
				case "started":
					startOnce.Do(func() {
						process.PID = int(e.PID)
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
			process.exitCode = int(result.ExitCode)
		}
		process.exitErr = err
		close(process.exited)
	}()
	select {
	case <-started:
		return process, nil
	case <-process.exited:
		// Failing before it started fails the open; afterwards, Wait
		// carries it.
		select {
		case <-started:
			return process, nil
		default:
		}
		if process.exitErr == nil {
			return nil, errors.New("pi-env ended the process before it started")
		}
		return nil, process.exitErr
	case <-ctx.Done():
		abort()
		<-process.exited
		return nil, ctx.Err()
	}
}
