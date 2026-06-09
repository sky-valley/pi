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
type EventStream[T any, R any] struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []T
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

// Push enqueues an event. If the event is the terminal event, the final result
// is captured. Pushes after the stream is done are ignored, matching the TS
// implementation.
func (s *EventStream[T, R]) Push(event T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	if s.isComplete(event) {
		s.done = true
		if !s.hasResult {
			s.result = s.extract(event)
			s.hasResult = true
		}
		s.queue = append(s.queue, event)
		s.cond.Broadcast()
		return
	}
	s.queue = append(s.queue, event)
	s.cond.Broadcast()
}

// End terminates the stream. If a non-nil result is supplied it becomes the
// final result (when one was not already captured). Waiting consumers are woken.
func (s *EventStream[T, R]) End(result ...R) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	if len(result) > 0 && !s.hasResult {
		s.result = result[0]
		s.hasResult = true
	}
	s.cond.Broadcast()
}

// Next pulls the next event, blocking until one is available or the stream is
// drained. ok is false once no more events will arrive.
func (s *EventStream[T, R]) Next() (event T, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if len(s.queue) > 0 {
			event = s.queue[0]
			s.queue = s.queue[1:]
			return event, true
		}
		if s.done {
			var zero T
			return zero, false
		}
		s.cond.Wait()
	}
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
