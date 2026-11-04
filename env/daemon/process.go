package daemon

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// process: a long-lived program with plain pipes, for programs that speak a
// protocol on stdin and stdout (an MCP server). Unlike exec, the program reads
// input (processWrite, processCloseStdin, addressed by the process request's
// id) and output is raw bytes per stream; unlike pty, nothing is echoed,
// translated or merged.

// pipedProcess is what processWrite and processCloseStdin reach: the stdin of
// one running process request.
type pipedProcess struct{ input *inputQueue }

// processes are the running programs by the id of their process request.
type processes = targets[pipedProcess]

func newProcesses() *processes { return newTargets[pipedProcess]("process") }

type processRequest struct {
	launch
	argv []string
}

func processRequestFromJSON(json any) (processRequest, *failure) {
	var r processRequest
	argv, ok, fail := argvField(json)
	if fail != nil {
		return r, fail
	}
	if !ok {
		return r, newFailure("EINVAL", "process needs argv")
	}
	r.argv = argv
	if r.launch, fail = launchFromJSON(json, "process"); fail != nil {
		return r, fail
	}
	return r, nil
}

// queueInput queues input for the program's stdin, to be written in the order
// it arrives without holding up other requests.
func queueInput(procs *processes, json any, in input) (object, *failure) {
	p, fail := procs.find(json)
	if fail != nil {
		return nil, fail
	}
	if !p.input.push(in) {
		return nil, newFailure("EBADF", "the program's stdin is closed")
	}
	return object{}, nil
}

// processWrite queues the payload for the program's stdin.
func processWrite(procs *processes, json any, payload []byte) (object, *failure) {
	return queueInput(procs, json, input{data: payload})
}

// processCloseStdin closes the program's stdin once the input queued before
// it has been written.
func processCloseStdin(procs *processes, json any) (object, *failure) {
	return queueInput(procs, json, input{endStdin: true})
}

// runProcess runs the program, sending a started event and then its output,
// until it exits.
func runProcess(id uint32, request processRequest, out *output, ctl *control, running *groups, procs *processes) (object, *failure) {
	if ctl.aborted.Load() {
		return nil, newFailure("aborted", "aborted")
	}
	program, fail := resolveArgvProgram(request.argv[0], request.cwd, request.env)
	if fail != nil {
		return nil, fail
	}
	if !pathExists(request.cwd) {
		return nil, newFailure("spawn_error", "Working directory does not exist: "+request.cwd)
	}
	command, fail := startCommand(program, request.argv[1:], request.launch, true)
	if fail != nil {
		return nil, fail
	}
	pid := command.process.pid
	running.add(pid)
	// What the program starts in its group dies when it exits: a wrapper such
	// as npx that exits does not leave the server it started running, out of
	// reach of close() and of a lost connection.
	group, err := newProgramGroup(command.process)
	if err != nil {
		// The started program is killed and reaped. On Windows the group kill
		// starts taskkill, which can fail too; the program is killed directly.
		killTree(pid)
		command.process.kill()
		// Only reaped: the request fails with the group's error either way.
		_, _ = command.process.wait()
		running.remove(pid)
		command.close()
		return nil, newFailure("spawn_error", ioErrorText(err))
	}
	stop := new(atomic.Bool)
	done := make(chan struct{})
	messages := make(chan commandMessage, 64)
	for stream, pipe := range command.output {
		// Like a full pipe: a program that writes faster than the link waits.
		go readPipe(stream, pipe, messages, done, stop, out)
	}
	// Closing stdin interrupts a write the program does not read, also when a
	// descendant holds the pipe.
	closeStdin := sync.OnceFunc(func() { command.stdin.Close() })
	p := &pipedProcess{input: newInputQueue()}
	go func() {
		// Later input fails with EBADF.
		defer closeStdin()
		defer p.input.close()
		for {
			in, ok := p.input.pop()
			if !ok || in.endStdin {
				return
			}
			if _, err := command.stdin.Write(in.data); err != nil {
				return
			}
		}
	}()
	waitInBackground(command.process, &ending{group: group, running: running}, messages, done)
	// Registered before started goes out, so input sent after it always finds
	// the program.
	procs.add(id, p)
	out.sendControl(frame.Frame{Kind: frame.Event, ID: id, JSON: marshal(object{"kind": "started", "pid": int64(pid)})})

	names := [2]string{"stdout", "stderr"}
	var (
		ended     [2]bool
		exited    bool
		reported  bool
		code      int64
		waitErr   error
		idleUntil time.Time
		aborted   bool
		killed    bool
	)
	kill := func() {
		if !killed {
			killed = true
			// Only while the program is in running: the waiter removes it
			// under its lock before reaping it (see groups.kill for Windows).
			running.kill(pid)
		}
	}
	if ctl.killed.Load() {
		kill()
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for !exited || !ended[0] || !ended[1] {
		wait := time.Hour
		if !idleUntil.IsZero() {
			now := time.Now()
			if !now.Before(idleUntil) {
				// A descendant may hold the pipes open after the program
				// exited; stop waiting for it.
				break
			}
			wait = idleUntil.Sub(now)
		}
		timer.Reset(wait)
		select {
		case message := <-messages:
			switch {
			case message.data != nil:
				out.sendBulk(frame.Frame{Kind: frame.Event, ID: id, JSON: marshal(object{"kind": "output", "stream": names[message.stream]}), Payload: message.data}, nil)
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
					// The waiter already ended the group; nothing is left to
					// kill.
					killed = true
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
	// Readers and the writer stop, also when a descendant still holds a pipe;
	// removing the program ends a writer waiting for input.
	stop.Store(true)
	close(done)
	procs.remove(id)
	p.input.close()
	closeStdin()
	for _, pipe := range command.output {
		pipe.Close()
	}
	running.remove(pid)
	if aborted {
		return nil, newFailure("aborted", "aborted")
	}
	if waitErr != nil {
		return nil, newFailure("spawn_error", waitErr.Error())
	}
	if !reported {
		return nil, newFailure("spawn_error", "process did not report its exit")
	}
	return object{"exitCode": code}, nil
}
