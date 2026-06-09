package ai

import (
	"sync"
	"testing"
)

func TestEventStreamOrderingAndResult(t *testing.T) {
	s := NewAssistantMessageEventStream()
	final := &AssistantMessage{Model: "m", StopReason: StopStop}

	go func() {
		s.Push(AssistantMessageEvent{Type: EventStart, Partial: &AssistantMessage{}})
		s.Push(AssistantMessageEvent{Type: EventTextDelta, Delta: "hello"})
		s.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: final})
		s.End()
	}()

	var types []EventType
	for e := range s.Events() {
		types = append(types, e.Type)
	}
	if len(types) != 3 || types[0] != EventStart || types[2] != EventDone {
		t.Fatalf("unexpected event sequence: %v", types)
	}
	if got := s.Result(); got != final {
		t.Fatalf("Result() = %v, want final message", got)
	}
}

func TestEventStreamResultBeforeDrain(t *testing.T) {
	s := NewAssistantMessageEventStream()
	final := &AssistantMessage{StopReason: StopError, ErrorMessage: "boom"}
	go func() {
		s.Push(AssistantMessageEvent{Type: EventError, Reason: StopError, Error: final})
	}()
	// Result resolves from the terminal event even without consuming events.
	if got := s.Result(); got != final {
		t.Fatalf("Result() = %v, want error message", got)
	}
}

func TestEventStreamPushAfterDoneIgnored(t *testing.T) {
	s := NewAssistantMessageEventStream()
	final := &AssistantMessage{StopReason: StopStop}
	s.Push(AssistantMessageEvent{Type: EventDone, Message: final})
	s.Push(AssistantMessageEvent{Type: EventTextDelta, Delta: "late"})
	s.End()
	count := 0
	for range s.Events() {
		count++
	}
	if count != 1 {
		t.Fatalf("expected 1 event (push after done ignored), got %d", count)
	}
}

// TestEventStreamEndSuppliesResult asserts End(result) sets the deferred result
// when no terminal event captured one (pi event-stream.ts:38-41 resolves the
// final result promise from end's argument). Go keeps finite behavior: Result()
// returns the supplied value rather than hanging.
func TestEventStreamEndSuppliesResult(t *testing.T) {
	s := NewAssistantMessageEventStream()
	final := &AssistantMessage{Model: "m", StopReason: StopStop}

	s.Push(AssistantMessageEvent{Type: EventTextDelta, ContentIndex: 0, Delta: "hi"})
	s.End(final)

	if got := s.Result(); got != final {
		t.Fatalf("End(result) did not supply result: got %v, want final", got)
	}
}

// TestEventStreamEndResultIdempotent asserts a terminal event's result wins and
// is not overwritten by a later End(other), matching pi's resolve-once promise.
func TestEventStreamEndResultIdempotent(t *testing.T) {
	s := NewAssistantMessageEventStream()
	captured := &AssistantMessage{Model: "captured", StopReason: StopStop}
	other := &AssistantMessage{Model: "other", StopReason: StopStop}

	s.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: captured})
	s.End(other) // should be ignored; terminal event already captured result

	if got := s.Result(); got != captured {
		t.Fatalf("End(result) overwrote terminal result: got %v, want captured", got)
	}
}

// TestEventStreamEndNoResultIsZero asserts End() with no result leaves a zero
// result (Go's finite behavior; pi would hang awaiting the promise).
func TestEventStreamEndNoResultIsZero(t *testing.T) {
	s := NewAssistantMessageEventStream()
	s.Push(AssistantMessageEvent{Type: EventTextDelta, ContentIndex: 0, Delta: "x"})
	s.End()
	if got := s.Result(); got != nil {
		t.Fatalf("Result() = %v, want nil zero value", got)
	}
}

func TestEventStreamConcurrentConsumers(t *testing.T) {
	s := NewEventStream(
		func(i int) bool { return i < 0 },
		func(i int) int { return i },
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := range s.Events() {
				mu.Lock()
				total += v
				mu.Unlock()
			}
		}()
	}
	for i := 1; i <= 100; i++ {
		s.Push(i)
	}
	s.End()
	wg.Wait()
	if total != 5050 {
		t.Fatalf("sum across consumers = %d, want 5050", total)
	}
}
