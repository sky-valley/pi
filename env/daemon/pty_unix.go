//go:build darwin || linux

package daemon

import (
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
	"golang.org/x/sys/unix"
)

// ptyExitGrace: after the program exits, output still arriving is collected
// until this much silence.
const ptyExitGrace = 100 * time.Millisecond

// terminal is what ptyWrite, ptyResize and ptyForeground reach: the terminal
// of one running pty request.
type terminal struct {
	input  *inputQueue
	master *os.File
	// pid is the program's process id, which is also its process group's (it
	// leads the terminal's session).
	pid int
}

// withMaster runs call on the master's descriptor while it is open.
func (t *terminal) withMaster(call func(fd int) error) error {
	conn, err := t.master.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	if err := conn.Control(func(fd uintptr) { callErr = call(int(fd)) }); err != nil {
		return unix.EBADF
	}
	return callErr
}

func (t *terminal) setSize(cols, rows uint16) error {
	return t.withMaster(func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}

// foreground is the terminal's foreground process group.
func (t *terminal) foreground() (int, bool, error) {
	var group int
	err := t.withMaster(func(fd int) (err error) {
		group, err = unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		return err
	})
	return group, true, err
}

// poll waits up to 100 ms for the master to be ready; false on a timeout or
// an interruption.
func (t *terminal) poll(events int16) (bool, error) {
	var ready bool
	err := t.withMaster(func(fd int) error {
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			return nil
		}
		ready = n > 0
		return err
	})
	return ready, err
}

// read reads until the terminal closes (Linux reports EIO once no program
// holds it), onData returns false, or stop is set.
func (t *terminal) read(stop *atomic.Bool, onData func([]byte) bool) {
	buf := make([]byte, readChunk)
	for !stop.Load() {
		ready, err := t.poll(unix.POLLIN)
		if err != nil {
			return
		}
		if !ready {
			continue
		}
		var n int
		err = t.withMaster(func(fd int) (err error) {
			n, err = unix.Read(fd, buf)
			return err
		})
		switch {
		case err == unix.EAGAIN || err == unix.EINTR:
		case err != nil || n == 0:
			return
		default:
			if !onData(append([]byte(nil), buf[:n]...)) {
				return
			}
		}
	}
}

// writeAll writes all of b, waiting while the program does not read its
// input; false once the terminal is gone.
func (t *terminal) writeAll(b []byte, stop *atomic.Bool) bool {
	for len(b) > 0 {
		if stop.Load() {
			return false
		}
		var n int
		err := t.withMaster(func(fd int) (err error) {
			n, err = unix.Write(fd, b)
			return err
		})
		switch {
		case err == unix.EAGAIN:
			if _, err := t.poll(unix.POLLOUT); err != nil {
				return false
			}
		case err == unix.EINTR:
		case err != nil || n == 0:
			return false
		default:
			b = b[n:]
		}
	}
	return true
}

// ptyCommand is the program to run and its argv: the request's argv, or the
// user's shell as a login shell ("-" before its name, as login does).
func ptyCommand(request ptyRequest) (string, []string) {
	if request.hasArgv {
		return request.argv[0], request.argv
	}
	shell, ok := asString(request.env["SHELL"])
	if !ok {
		shell, ok = os.LookupEnv("SHELL")
	}
	if !ok || shell == "" || !pathExists(shell) {
		shell = "/bin/sh"
	}
	name := shell[strings.LastIndexByte(shell, '/')+1:]
	return shell, []string{"-" + name}
}

// spawnPty starts the program in a new terminal, as the leader of a new
// session with the terminal as its controlling terminal.
func spawnPty(request ptyRequest) (*os.File, *os.Process, error) {
	master, slave, err := openpty(request.cols, request.rows)
	if err != nil {
		return nil, nil, err
	}
	program, argv := ptyCommand(request)
	files := []*os.File{slave, slave, slave}
	process, err := startProcess(program, argv, childEnvironment(request.env, request.inheritEnv), request.cwd, files, true)
	// Only the program holds the terminal now, so reads end once it and its
	// descendants close it.
	slave.Close()
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	conn, err := master.SyscallConn()
	if err == nil {
		err = conn.Control(func(fd uintptr) { err = unix.SetNonblock(int(fd), true) })
	}
	if err != nil {
		killTree(process.Pid)
		master.Close()
		return nil, nil, err
	}
	return master, process, nil
}

// runPty runs the program in a new terminal, sending a started event and then
// its output, until it exits.
func runPty(id uint32, request ptyRequest, out *output, ctl *control, running *groups, terms *terminals) (object, *failure) {
	if ctl.aborted.Load() {
		return nil, newFailure("aborted", "aborted")
	}
	if !pathExists(request.cwd) {
		return nil, newFailure("spawn_error", "Working directory does not exist: "+request.cwd)
	}
	master, process, err := spawnPty(request)
	if err != nil {
		return nil, newFailure("spawn_error", ioErrorText(unwrapPathError(err)))
	}
	pid := process.Pid
	running.add(pid)
	term := &terminal{input: newInputQueue(), master: master, pid: pid}
	terms.mu.Lock()
	terms.running[id] = term
	terms.mu.Unlock()
	out.sendControl(frame.Frame{Kind: frame.Event, ID: id, JSON: marshal(object{"kind": "started", "pid": int64(pid)})})

	stop := new(atomic.Bool)
	done := make(chan struct{})
	messages := make(chan commandMessage, 64)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		term.read(stop, func(b []byte) bool {
			select {
			case messages <- commandMessage{data: b}:
			case <-done:
				return false
			}
			// Like a full pipe: a program that writes faster than the link
			// waits.
			out.waitForRoom(stop)
			return true
		})
		select {
		case messages <- commandMessage{ended: true}:
		case <-done:
		}
	}()
	go func() {
		for {
			b, ok := term.input.pop()
			if !ok || !term.writeAll(b, stop) {
				term.input.close()
				return
			}
		}
	}()
	go func() {
		state, err := process.Wait()
		select {
		case messages <- commandMessage{exited: true, state: state, err: err}:
		case <-done:
		}
	}()

	var (
		exited    bool
		ended     bool
		state     *os.ProcessState
		waitErr   error
		idleUntil time.Time
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
loop:
	for {
		if exited && ended {
			break
		}
		wait := time.Hour
		if !idleUntil.IsZero() {
			now := time.Now()
			if !now.Before(idleUntil) {
				// A background job may keep the terminal open after the program
				// exited.
				break
			}
			wait = idleUntil.Sub(now)
		}
		timer.Reset(wait)
		select {
		case message := <-messages:
			switch {
			case message.data != nil:
				out.sendBulk(frame.Frame{Kind: frame.Event, ID: id, JSON: marshal(object{"kind": "output"}), Payload: message.data}, nil)
				if exited {
					idleUntil = time.Now().Add(ptyExitGrace)
				}
			case message.ended:
				ended = true
			case message.exited:
				exited = true
				if message.err != nil {
					waitErr = message.err
					idleUntil = time.Now()
				} else {
					state = message.state
					idleUntil = time.Now().Add(ptyExitGrace)
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
			continue loop
		}
	}
	stop.Store(true)
	close(done)
	// Ending the input ends the writer.
	terms.mu.Lock()
	delete(terms.running, id)
	terms.mu.Unlock()
	term.input.close()
	running.remove(pid)
	<-readerDone
	master.Close()
	if aborted {
		return nil, newFailure("aborted", "aborted")
	}
	if waitErr != nil {
		return nil, newFailure("spawn_error", waitErr.Error())
	}
	if state == nil {
		return nil, newFailure("spawn_error", "process did not report its exit")
	}
	return object{"exitCode": exitCode(state)}, nil
}
