package daemon

import (
	"bufio"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// bulkLimit is the unsent bulk bytes above which unwindowed command output
// waits.
const bulkLimit = 4 * 1024 * 1024

// output is the single writer of stdout. Control frames (results, errors,
// pings, watch events) always go before bulk command output, so a slow link
// delays output but never a cancel's result or a ping. Bulk output is
// bounded: readers of commands without an output window wait while too much
// of it is unsent, which slows the command down like a full pipe would.
type output struct {
	mu        sync.Mutex
	control   []frame.Frame
	bulk      []bulkFrame
	bulkBytes int
	closed    bool
	// queued is signalled when a frame is queued or the writer stops.
	queued *sync.Cond
	// drained is closed and replaced whenever bulk bytes were written or the
	// writer stops, waking every waiter.
	drained chan struct{}
}

type bulkFrame struct {
	frame frame.Frame
	// unsent counts its sender's frames until they are written.
	unsent *atomic.Int64
}

func newOutput() *output {
	o := &output{drained: make(chan struct{})}
	o.queued = sync.NewCond(&o.mu)
	return o
}

// sendControl queues a control frame.
func (o *output) sendControl(f frame.Frame) {
	o.mu.Lock()
	o.control = append(o.control, f)
	o.mu.Unlock()
	o.queued.Signal()
}

// sendBulk queues bulk output; unsent, when set, counts the sender's frames
// until they are written.
func (o *output) sendBulk(f frame.Frame, unsent *atomic.Int64) {
	o.mu.Lock()
	o.bulkBytes += len(f.Payload)
	if unsent != nil {
		unsent.Add(1)
	}
	o.bulk = append(o.bulk, bulkFrame{f, unsent})
	o.mu.Unlock()
	o.queued.Signal()
}

// waitForRoom waits while more than the bulk limit is unsent, unless stop is
// set (checked every 100 ms) or the writer has stopped.
func (o *output) waitForRoom(stop *atomic.Bool) {
	for {
		o.mu.Lock()
		if o.bulkBytes <= bulkLimit || o.closed || stop.Load() {
			o.mu.Unlock()
			return
		}
		drained := o.drained
		o.mu.Unlock()
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-drained:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// close stops the writer once the queues are empty and wakes every waiter.
func (o *output) close() {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.drained)
		o.drained = make(chan struct{})
	}
	o.mu.Unlock()
	o.queued.Broadcast()
}

// run writes frames to w, flushing each, until closed and drained or a write
// fails.
func (o *output) run(w *bufio.Writer) {
	for {
		o.mu.Lock()
		var next frame.Frame
		var unsent *atomic.Int64
		for {
			if len(o.control) > 0 {
				next = o.control[0]
				o.control[0] = frame.Frame{}
				o.control = o.control[1:]
				break
			}
			if len(o.bulk) > 0 {
				queued := o.bulk[0]
				o.bulk[0] = bulkFrame{}
				o.bulk = o.bulk[1:]
				o.bulkBytes -= len(queued.frame.Payload)
				next, unsent = queued.frame, queued.unsent
				break
			}
			if o.closed {
				o.mu.Unlock()
				return
			}
			o.queued.Wait()
		}
		o.mu.Unlock()
		err := frame.Write(w, next)
		if err == nil {
			err = w.Flush()
		}
		if unsent != nil {
			unsent.Add(-1)
		}
		o.mu.Lock()
		close(o.drained)
		o.drained = make(chan struct{})
		o.mu.Unlock()
		if err != nil {
			o.close()
			return
		}
	}
}
