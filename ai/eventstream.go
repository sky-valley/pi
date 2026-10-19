package ai

import (
	"iter"
	"sync"
)

// EventStream is a generic, thread-safe async event queue with a deferred final
// result. It is the Go port of pi's EventStream<T, R> (event-stream.ts).
//
// Producers call Push for each event and (optionally) End to terminate. A single
// "complete" event — identified by isComplete — captures the final result via
// extractResult and unblocks Result. Consumers range over Events (an iterator)
// or pull events with Next, and may await the final result with Result.
//
// Like pi, the stream holds TWO collections: buffered events with no consumer
// yet (queue), and consumers waiting with no event yet (waiting). Push hands an
// event DIRECTLY to the longest-waiting consumer, bypassing the queue, so
// events reach blocked consumers in registration order — pi's `waiting` FIFO
// (upstream b2602be77), pinned here by
// TestEventStreamDeliversToWaitersInRegistrationOrder. pi's two-stack FifoQueue
// itself has no Go counterpart: it exists to make JS's O(n) Array.shift() an
// O(1) dequeue, and a Go slice reslice is already O(1).
type EventStream[T any, R any] struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []T
	waiting    []chan T
	done       bool
	result     R
	hasResult  bool
	isComplete func(T) bool
	extract    func(T) R
}

// NewEventStream creates an EventStream. isComplete reports whether an event is
// the terminal event; extract derives the final result from that event.
func NewEventStream[T any, R any](isComplete func(T) bool, extract func(T) R) *EventStream[T, R] {
	s := &EventStream[T, R]{isComplete: isComplete, extract: extract}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// releaseWaitersLocked signals every registered consumer that no further event
// is coming. Closing the channel makes the consumer's receive yield ok=false.
func (s *EventStream[T, R]) releaseWaitersLocked() {
	for _, ch := range s.waiting {
		close(ch)
	}
	s.waiting = nil
}

// Push enqueues an event, or hands it straight to the longest-waiting consumer.
// If the event is the terminal event, the final result is captured. Pushes after
// the stream is done are ignored, matching the TS implementation.
func (s *EventStream[T, R]) Push(event T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}

	terminal := s.isComplete(event)
	if terminal {
		s.done = true
		if !s.hasResult {
			s.result = s.extract(event)
			s.hasResult = true
		}
	}

	// Deliver to a waiting consumer, else buffer it (pi push(), event-stream.ts).
	if len(s.waiting) > 0 {
		ch := s.waiting[0]
		s.waiting = s.waiting[1:]
		ch <- event // cap-1 buffer, delivered once: never blocks
	} else {
		s.queue = append(s.queue, event)
	}

	if terminal {
		// pi leaves consumers that did not receive the terminal event pending
		// until end() resolves them; a pending JS promise is free, a parked
		// goroutine is not. Releasing them here keeps Next from blocking
		// forever when a producer ends the stream with a terminal event and
		// never calls End, which is what this port has always done.
		s.releaseWaitersLocked()
		s.cond.Broadcast()
	}
}

// isDone reports whether the stream has completed: a terminal event was pushed
// or End was called (pi's protected `done`).
func (s *EventStream[T, R]) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// End terminates the stream. If a result is supplied it becomes the final
// result unless one was already captured (a terminal event's result wins,
// mirroring pi's resolve-once promise). Like pi's end (event-stream.ts:60-70),
// there is no done-guard: End() followed by End(result) still surfaces the
// result. Waiting consumers are woken.
func (s *EventStream[T, R]) End(result ...R) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(result) > 0 && !s.hasResult {
		s.result = result[0]
		s.hasResult = true
	}
	s.done = true
	s.releaseWaitersLocked()
	s.cond.Broadcast()
}

// Next pulls the next event, blocking until one is available or the stream is
// drained. ok is false once no more events will arrive. Buffered events are
// drained before the done flag is honoured, so events pushed before End are
// never lost.
func (s *EventStream[T, R]) Next() (event T, ok bool) {
	s.mu.Lock()
	if len(s.queue) > 0 {
		event = s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		return event, true
	}
	if s.done {
		s.mu.Unlock()
		var zero T
		return zero, false
	}
	ch := make(chan T, 1)
	s.waiting = append(s.waiting, ch)
	s.mu.Unlock()

	event, ok = <-ch
	return event, ok
}

// Events returns a single-use iterator over the stream's events. It is safe to
// use with range-over-func.
func (s *EventStream[T, R]) Events() iter.Seq[T] {
	return func(yield func(T) bool) {
		for {
			event, ok := s.Next()
			if !ok {
				return
			}
			if !yield(event) {
				return
			}
		}
	}
}

// Result blocks until the final result is available (or the stream ends without
// one, returning the zero value).
func (s *EventStream[T, R]) Result() R {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.hasResult && !s.done {
		s.cond.Wait()
	}
	return s.result
}
