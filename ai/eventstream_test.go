package ai

import (
	"errors"
	"sync"
	"testing"
	"time"
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

// TestEventStreamEndThenEndResultSurfaces asserts End() followed by
// End(result) still surfaces the result (pi event-stream.ts:38-42 has no
// done-guard on result resolution; only the resolve-once promise semantics
// apply, which the hasResult guard preserves).
func TestEventStreamEndThenEndResultSurfaces(t *testing.T) {
	s := NewAssistantMessageEventStream()
	final := &AssistantMessage{Model: "m", StopReason: StopStop}
	s.End()
	s.End(final)
	if got := s.Result(); got != final {
		t.Fatalf("End() then End(result) dropped result: got %v, want final", got)
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

// TestEventStreamDeliversToWaitersInRegistrationOrder mirrors pi's "delivers
// events to waiting consumers in registration order" (upstream b2602be77,
// packages/ai/test/event-stream.test.ts). pi's push hands each event DIRECTLY to
// the longest-waiting consumer, dequeued from its `waiting` FIFO and bypassing
// the event queue entirely, so the consumer that registered first receives the
// first event.
func TestEventStreamDeliversToWaitersInRegistrationOrder(t *testing.T) {
	s := NewEventStream(
		func(int) bool { return false },
		func(i int) int { return i },
	)

	got := make([]int, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	register := func(idx int) {
		go func() {
			defer wg.Done()
			v, _ := s.Next()
			got[idx] = v
		}()
	}

	// Stagger registration so "first" and "second" are unambiguous.
	register(0)
	time.Sleep(50 * time.Millisecond)
	register(1)
	time.Sleep(50 * time.Millisecond)

	s.Push(1)
	s.Push(2)
	wg.Wait()

	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("delivery to waiters = first:%d second:%d, want first:1 second:2", got[0], got[1])
	}
}

// The AssistantMessageEventStream timing tests mirror pi event-stream.test.ts
// "AssistantMessageEventStream timing" (upstream 36a686ee8, f284a2460).

func timedTestMessage(timestamp int64) *AssistantMessage {
	return &AssistantMessage{Api: "openai-responses", Provider: "openai", Model: "m", StopReason: StopStop, Timestamp: timestamp}
}

func TestAssistantMessageEventStreamTimesFinalMessage(t *testing.T) {
	done := NewAssistantMessageEventStream()
	answer := timedTestMessage(nowMillis())
	time.Sleep(20 * time.Millisecond)
	done.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: answer})
	if answer.DurationMs == nil || *answer.DurationMs < 15 {
		t.Fatalf("done message durationMs = %v, want >= 15", answer.DurationMs)
	}
	if got := done.Result(); got.DurationMs != answer.DurationMs {
		t.Fatalf("result durationMs = %v, want the done message's", got.DurationMs)
	}

	failed := NewAssistantMessageEventStream()
	errMsg := timedTestMessage(nowMillis())
	errMsg.StopReason = StopError
	failed.Push(AssistantMessageEvent{Type: EventError, Reason: StopError, Error: errMsg})
	if errMsg.DurationMs == nil || *errMsg.DurationMs < 0 {
		t.Fatalf("error message durationMs = %v, want >= 0", errMsg.DurationMs)
	}

	ended := NewAssistantMessageEventStream()
	result := timedTestMessage(nowMillis())
	ended.End(result)
	if result.DurationMs == nil || *result.DurationMs < 0 {
		t.Fatalf("end() result durationMs = %v, want >= 0", result.DurationMs)
	}
}

// A forwarding stream keeps the inner measurement.
func TestAssistantMessageEventStreamKeepsExistingDuration(t *testing.T) {
	outer := NewAssistantMessageEventStream()
	time.Sleep(20 * time.Millisecond)
	inner := NewAssistantMessageEventStream()
	answer := timedTestMessage(nowMillis())
	inner.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: answer})
	if answer.DurationMs == nil {
		t.Fatal("the inner stream did not time the response")
	}
	measured := *answer.DurationMs
	outer.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: answer})
	if *answer.DurationMs != measured || measured >= 20 {
		t.Fatalf("durationMs = %d after forwarding, want the inner %d (< 20)", *answer.DurationMs, measured)
	}

	preset := timedTestMessage(nowMillis())
	preset.DurationMs = new(int64)
	*preset.DurationMs = 1234
	NewAssistantMessageEventStream().Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: preset})
	if *preset.DurationMs != 1234 {
		t.Fatalf("preset durationMs = %d, want 1234", *preset.DurationMs)
	}
}

// A message that started before the stream, such as a fetched deferred
// result, stays untimed.
func TestAssistantMessageEventStreamLeavesEarlierMessageUntimed(t *testing.T) {
	fetched := timedTestMessage(nowMillis() - 60_000)
	NewAssistantMessageEventStream().Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: fetched})
	if fetched.DurationMs != nil {
		t.Fatalf("durationMs = %d, want none", *fetched.DurationMs)
	}
}

func TestAssistantMessageEventStreamDoesNotTimeAfterCompletion(t *testing.T) {
	stream := NewAssistantMessageEventStream()
	stream.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: timedTestMessage(nowMillis())})
	late := timedTestMessage(nowMillis())
	stream.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: late})
	if late.DurationMs != nil {
		t.Fatalf("durationMs = %d, want none", *late.DurationMs)
	}
	stream.End(late)
	if late.DurationMs != nil {
		t.Fatalf("end() after completion timed the message: %d", *late.DurationMs)
	}
}

// pi's lazyStream stamps a setup-error message with the wall clock taken just
// before its stream (3ba22ce17), which ErrorStream mirrors. Whether the stream
// then times it depends on both readings falling in the same millisecond, as
// in pi.
func TestErrorStreamStampsSetupErrorAtStreamStart(t *testing.T) {
	before := nowMillis()
	msg := ErrorStream(&Model{Api: "a", Provider: "p", ID: "m"}, errors.New("setup failed")).Result()
	if after := nowMillis(); msg.Timestamp < before || msg.Timestamp > after {
		t.Fatalf("timestamp = %d, want the stream start in [%d, %d]", msg.Timestamp, before, after)
	}
}
