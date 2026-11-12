//go:build !windows

package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// The output of a program that exits while the link holds its reader up
// (upstream b48c675a9): the daemon collects output until 100 ms of silence
// after the exit, but a reader waiting for the link is silent too, so
// without the gate's held count the rest of the pipe was lost. The link is
// backed up here by queueing more than the bulk limit before the command
// starts and draining it only well after the grace; the program writes one
// byte, which the reader hands over before waiting, then the rest, which
// sits in the pipe while the program exits. Upstream's test backs the link
// up from a stalled Node client; the Go client queues events without
// bound, so a slow OnStdout never backs it up, and the stall is made here
// instead.
func TestCommandsReadOnWhileTheLinkHoldsTheirReaderUp(t *testing.T) {
	// Small enough to fit in a pipe and in a pty's line buffer, so the
	// program exits with it unread.
	const rest = 2048
	script := "printf a; sleep 0.3; head -c " + strconv.Itoa(rest) + " /dev/zero"
	argv, _ := json.Marshal([]string{"sh", "-c", script})
	for _, op := range []string{"process", "exec", "pty"} {
		t.Run(op, func(t *testing.T) {
			out := newOutput()
			out.sendBulk(frame.Frame{Kind: frame.Event, ID: 99, Payload: make([]byte, bulkLimit+1)}, nil)
			var written bytes.Buffer
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				time.Sleep(time.Second)
				out.run(bufio.NewWriter(&written))
			}()

			request := parseJSON([]byte(`{"argv":` + string(argv) + `,"cwd":"` + t.TempDir() + `","cols":80,"rows":24}`))
			var fail *failure
			switch op {
			case "process":
				r, f := processRequestFromJSON(request)
				if f == nil {
					_, fail = runProcess(1, r, out, newControl(), newGroups(), newProcesses())
				} else {
					fail = f
				}
			case "exec":
				r, f := execRequestFromJSON(request)
				if f == nil {
					_, fail = runExec(1, r, t.TempDir(), out, newControl(), newGroups())
				} else {
					fail = f
				}
			case "pty":
				r, f := ptyRequestFromJSON(request)
				if f == nil {
					_, fail = runPty(1, r, out, newControl(), newGroups(), newTerminals())
				} else {
					fail = f
				}
			}
			if fail != nil {
				t.Fatalf("%s failed: %v", op, fail)
			}
			out.close()
			<-drained

			got := 0
			frames := bufio.NewReader(&written)
			for {
				f, err := frame.Read(frames)
				if err != nil {
					break
				}
				if f.ID == 1 && f.Kind == frame.Event && strings.Contains(string(f.JSON), `"output"`) {
					got += len(f.Payload)
				}
			}
			// A pty may translate nothing here: NUL bytes and "a" pass as is.
			if got != rest+1 {
				t.Fatalf("%d bytes of output, want %d", got, rest+1)
			}
		})
	}
}
