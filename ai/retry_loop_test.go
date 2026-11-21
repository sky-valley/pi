package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func errResp(msg string) *AssistantMessage {
	return &AssistantMessage{StopReason: StopError, ErrorMessage: msg}
}

// textResp is pi's fauxAssistantMessage(text): a completed message carrying
// one text block.
func textResp(text string) *AssistantMessage {
	return &AssistantMessage{Content: ContentList{TextContent{Text: text}}, StopReason: StopStop}
}

func abortedResp() *AssistantMessage {
	return &AssistantMessage{StopReason: StopAborted}
}

type retryFinishedCall struct {
	success    bool
	attempt    int
	finalError string
}

// retryRecorder stands in for pi's vi.fn() callbacks: it records every call so
// a case can assert counts and arguments the way the vitest suite does.
type retryRecorder struct {
	scheduled     []int // the attempt of each OnRetryScheduled
	attemptStarts int
	finished      []retryFinishedCall
}

func (r *retryRecorder) callbacks() *RetryCallbacks {
	return &RetryCallbacks{
		OnRetryScheduled:    func(attempt, _, _ int, _ string) { r.scheduled = append(r.scheduled, attempt) },
		OnRetryAttemptStart: func() { r.attemptStarts++ },
		OnRetryFinished: func(success bool, attempt int, finalError string) {
			r.finished = append(r.finished, retryFinishedCall{success, attempt, finalError})
		},
	}
}

// finishedWith is vitest's toHaveBeenCalledWith on OnRetryFinished. pi calls it
// with two arguments on success and on an abort; Go's empty finalError is that
// missing third argument.
func (r *retryRecorder) finishedWith(t *testing.T, want retryFinishedCall) {
	t.Helper()
	for _, call := range r.finished {
		if call == want {
			return
		}
	}
	t.Fatalf("OnRetryFinished calls = %+v, want one with %+v", r.finished, want)
}

// TestRetryAssistantCall transliterates pi's retryAssistantCall suite
// (packages/ai/test/retry.test.ts at e5d18382a) with its fixtures: "terminated"
// is the transient error and "insufficient_quota" the non-retryable one.
func TestRetryAssistantCall(t *testing.T) {
	disabled := &RetryPolicy{Enabled: false, MaxRetries: 3, BaseDelayMs: 0}
	enabled := &RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelayMs: 0}

	t.Run("returns a successful response immediately without retrying", func(t *testing.T) {
		calls := 0
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage { calls++; return textResp("ok") }, enabled, nil)
		if want := (ContentList{TextContent{Text: "ok"}}); !reflect.DeepEqual(res.Content, want) {
			t.Fatalf("content = %+v, want %+v", res.Content, want)
		}
		if calls != 1 {
			t.Fatalf("produce calls = %d, want 1", calls)
		}
	})

	t.Run("does not retry an aborted message", func(t *testing.T) {
		calls := 0
		var rec retryRecorder
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage { calls++; return abortedResp() }, enabled, rec.callbacks())
		if res.StopReason != StopAborted {
			t.Fatalf("stop = %v, want aborted", res.StopReason)
		}
		if calls != 1 {
			t.Fatalf("produce calls = %d, want 1", calls)
		}
		if len(rec.scheduled) != 0 {
			t.Fatalf("OnRetryScheduled called for attempts %v, want never", rec.scheduled)
		}
	})

	t.Run("does not retry a non-retryable error (quota/billing)", func(t *testing.T) {
		calls := 0
		var rec retryRecorder
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage { calls++; return errResp("insufficient_quota") }, enabled, rec.callbacks())
		if res.StopReason != StopError {
			t.Fatalf("stop = %v, want error", res.StopReason)
		}
		if calls != 1 {
			t.Fatalf("produce calls = %d, want 1", calls)
		}
		if len(rec.scheduled) != 0 || len(rec.finished) != 0 {
			t.Fatalf("callbacks fired: scheduled=%v finished=%+v, want none", rec.scheduled, rec.finished)
		}
	})

	t.Run("retries a transient error up to maxRetries then returns the final error", func(t *testing.T) {
		calls := 0
		var rec retryRecorder
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage { calls++; return errResp("terminated") }, enabled, rec.callbacks())
		if res.StopReason != StopError {
			t.Fatalf("stop = %v, want error", res.StopReason)
		}
		if calls != 4 { // 1 initial + 3 retries
			t.Fatalf("produce calls = %d, want 4", calls)
		}
		if len(rec.scheduled) != 3 {
			t.Fatalf("OnRetryScheduled calls = %d, want 3", len(rec.scheduled))
		}
		rec.finishedWith(t, retryFinishedCall{false, 3, "terminated"})
		// The positive counterpart to the absence pin in the abort case below:
		// an exhausted budget keeps the last error under pi's own key. Without
		// this, that pin would survive the field losing its key entirely.
		if raw, present := marshalField(t, res, "errorMessage"); !present || string(raw) != `"terminated"` {
			t.Fatalf("errorMessage = %s (present=%v), want \"terminated\"", raw, present)
		}
	})

	// Regression for #10506 (upstream 51ba3ab5b).
	t.Run("stops retrying once a call succeeds and removes backoff abort listeners", func(t *testing.T) {
		ctx := newListenerCountingContext()
		n := 0
		var rec retryRecorder
		res := RetryAssistantCall(ctx, func() *AssistantMessage {
			n++
			if n < 4 {
				return errResp("terminated")
			}
			return textResp("recovered")
		}, enabled, rec.callbacks())
		if want := (ContentList{TextContent{Text: "recovered"}}); !reflect.DeepEqual(res.Content, want) {
			t.Fatalf("content = %+v, want %+v", res.Content, want)
		}
		if n != 4 {
			t.Fatalf("produce calls = %d, want 4", n)
		}
		rec.finishedWith(t, retryFinishedCall{true, 3, ""})
		if live := ctx.listeners(); live != 0 {
			t.Fatalf("%d abort listeners still registered on the context, want 0", live)
		}
	})

	t.Run("reports an aborted retried call as unsuccessful", func(t *testing.T) {
		n := 0
		var rec retryRecorder
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage {
			n++
			if n == 1 {
				return errResp("terminated")
			}
			return abortedResp()
		}, enabled, rec.callbacks())
		if res.StopReason != StopAborted {
			t.Fatalf("stop = %v, want aborted", res.StopReason)
		}
		if n != 2 {
			t.Fatalf("produce calls = %d, want 2", n)
		}
		rec.finishedWith(t, retryFinishedCall{false, 1, ""})
	})

	t.Run("does not retry when policy is disabled", func(t *testing.T) {
		calls := 0
		var rec retryRecorder
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage { calls++; return errResp("terminated") }, disabled, rec.callbacks())
		if res.StopReason != StopError {
			t.Fatalf("stop = %v, want error", res.StopReason)
		}
		if calls != 1 {
			t.Fatalf("produce calls = %d, want 1", calls)
		}
		if len(rec.scheduled) != 0 || len(rec.finished) != 0 {
			t.Fatalf("callbacks fired: scheduled=%v finished=%+v, want none", rec.scheduled, rec.finished)
		}
	})

	// Go-only: pi's policy parameter is `RetryPolicy | undefined`, and nil is
	// the Go spelling of undefined.
	t.Run("nil policy does not retry", func(t *testing.T) {
		calls := 0
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage { calls++; return errResp("terminated") }, nil, nil)
		if calls != 1 || res.StopReason != StopError {
			t.Fatalf("produce calls = %d, stop = %v; want 1, error", calls, res.StopReason)
		}
	})

	t.Run("emits onRetryAttemptStart after backoff before each retried call", func(t *testing.T) {
		var events []string
		scheduled, attemptStarts, n := 0, 0, 0
		callbacks := &RetryCallbacks{
			OnRetryScheduled: func(attempt, _, _ int, _ string) {
				scheduled++
				events = append(events, fmt.Sprintf("retry:%d", attempt))
			},
			OnRetryAttemptStart: func() {
				attemptStarts++
				events = append(events, "attempt-start")
			},
		}
		res := RetryAssistantCall(context.Background(), func() *AssistantMessage {
			events = append(events, fmt.Sprintf("produce:%d", n))
			n++
			if n < 3 {
				return errResp("terminated")
			}
			return textResp("recovered")
		}, enabled, callbacks)
		if want := (ContentList{TextContent{Text: "recovered"}}); !reflect.DeepEqual(res.Content, want) {
			t.Fatalf("content = %+v, want %+v", res.Content, want)
		}
		if scheduled != 2 || attemptStarts != 2 {
			t.Fatalf("OnRetryScheduled calls = %d, OnRetryAttemptStart calls = %d; want 2, 2", scheduled, attemptStarts)
		}
		want := []string{"produce:0", "retry:1", "attempt-start", "produce:1", "retry:2", "attempt-start", "produce:2"}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("events = %q, want %q", events, want)
		}
	})

	t.Run("aborts backoff sleep via signal, returns an aborted message, and emits onRetryFinished(false)", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		policy := &RetryPolicy{Enabled: true, MaxRetries: 5, BaseDelayMs: 10_000}
		var rec retryRecorder
		// pi awaits the first produce() and then aborts, so the abort lands in
		// the first backoff sleep.
		produced := make(chan struct{})
		calls := 0
		go func() {
			<-produced
			cancel()
		}()
		res := RetryAssistantCall(ctx, func() *AssistantMessage {
			calls++
			if calls == 1 {
				close(produced)
			}
			return errResp("terminated")
		}, policy, rec.callbacks())
		if res.StopReason != StopAborted {
			t.Fatalf("stop = %v, want aborted", res.StopReason)
		}
		// pi 86bac52f9 destructures errorMessage away rather than setting it to
		// undefined, so the aborted message carries no errorMessage key at all —
		// the field is observable in serialized session JSON. Go's omitempty on
		// the cleared string is the same wire shape; this pins it.
		if raw, present := marshalField(t, res, "errorMessage"); present {
			t.Fatalf("errorMessage must be absent on an aborted retry, got %s", raw)
		}
		if calls != 1 {
			t.Fatalf("produce calls = %d, want 1", calls)
		}
		rec.finishedWith(t, retryFinishedCall{false, 1, "terminated"})
	})
}

// listenerCountingContext stands in for pi's getEventListeners(signal,
// "abort"). context.AfterFunc, and every context derived with WithCancel,
// WithTimeout or WithDeadline, registers on a parent that is not one of the
// context package's own through the parent's AfterFunc method, so the
// registrations not yet stopped are the abort handling still attached to it.
// It is never cancelled: the test only counts what is registered.
type listenerCountingContext struct {
	context.Context
	done chan struct{}
	mu   sync.Mutex
	live int
}

func newListenerCountingContext() *listenerCountingContext {
	return &listenerCountingContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *listenerCountingContext) Done() <-chan struct{} { return c.done }

func (c *listenerCountingContext) AfterFunc(func()) (stop func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live++
	stopped := false
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if stopped {
			return false
		}
		stopped = true
		c.live--
		return true
	}
}

func (c *listenerCountingContext) listeners() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live
}

// marshalField serializes msg and returns one top-level field of the result,
// reporting whether the key is there at all.
func marshalField(t *testing.T, msg *AssistantMessage, key string) (json.RawMessage, bool) {
	t.Helper()
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, present := fields[key]
	return raw, present
}

// TestRetryAssistantCallCapsBackoff pins pi's 60s default cap on the
// agent-level backoff (upstream c37b0e03b, fixes #8826). The context is
// cancelled before the call, so the loop reports the delay it WOULD sleep and
// then aborts on it — the scheduled value is observable without the sleep.
func TestRetryAssistantCallCapsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := &RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelayMs: 1_000_000}
	var got []int
	callbacks := &RetryCallbacks{
		OnRetryScheduled: func(_, _, delayMs int, _ string) { got = append(got, delayMs) },
	}
	RetryAssistantCall(ctx, func() *AssistantMessage { return errResp("terminated") }, policy, callbacks)
	if len(got) != 1 || got[0] != 60_000 {
		t.Fatalf("scheduled delays = %v, want [60000] (pi caps the agent backoff at DEFAULT_MAX_AGENT_RETRY_DELAY_MS)", got)
	}
}

// TestRetryDelayMs is pi's own retryDelayMs oracle (upstream c37b0e03b,
// packages/ai/test/retry.test.ts "caps agent retry delay") plus the cases that
// exist only in Go: pi computes with floats and clamps a non-safe integer,
// while an int shift here wraps — negative, then to zero — and a non-positive
// time.Duration makes time.NewTimer fire immediately, so an uncapped overflow
// would be a hot retry loop rather than a long sleep.
func TestRetryDelayMs(t *testing.T) {
	ptr := func(v int) *int { return &v }
	cases := []struct {
		name    string
		policy  RetryPolicy
		attempt int
		want    int
	}{
		// pi's three assertions, verbatim.
		{"default cap applies", RetryPolicy{BaseDelayMs: 2000}, 6, 60_000},
		{"explicit cap applies", RetryPolicy{BaseDelayMs: 2000, MaxAgentDelayMs: ptr(5000)}, 5, 5000},
		{"zero cap is a real zero", RetryPolicy{BaseDelayMs: 2000, MaxAgentDelayMs: ptr(0)}, 5, 0},
		// Below the cap the raw exponential passes through untouched.
		{"under the cap", RetryPolicy{BaseDelayMs: 2000}, 5, 32_000},
		{"cap above the delay is a no-op", RetryPolicy{BaseDelayMs: 1000, MaxAgentDelayMs: ptr(999_999)}, 3, 4000},
		// pi's Math.max(0, attempt-1): attempt 0 and below yield the base delay,
		// where an unguarded Go shift would panic on a negative count.
		{"attempt 1 is the base delay", RetryPolicy{BaseDelayMs: 1000}, 1, 1000},
		{"attempt 0 floors the exponent", RetryPolicy{BaseDelayMs: 1000}, 0, 1000},
		{"negative attempt floors the exponent", RetryPolicy{BaseDelayMs: 1000}, -5, 1000},
		// Overflow saturates upward into the cap. Unsaturated, 1000<<54 wraps
		// negative and 1000<<61 wraps to exactly zero.
		{"overflow saturates into the cap", RetryPolicy{BaseDelayMs: 1000}, 55, 60_000},
		{"shift past the word size saturates", RetryPolicy{BaseDelayMs: 1000}, 100, 60_000},
		{"overflow saturates above a raised cap", RetryPolicy{BaseDelayMs: 1000, MaxAgentDelayMs: ptr(1 << 30)}, 100, 1 << 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryDelayMs(tc.policy, tc.attempt); got != tc.want {
				t.Fatalf("RetryDelayMs(%+v, %d) = %d, want %d", tc.policy, tc.attempt, got, tc.want)
			}
		})
	}
}

// TestRetryAssistantCallReportsCappedDelays transliterates pi's "reports capped
// retry delays" (upstream c37b0e03b, packages/ai/test/retry.test.ts): the whole
// loop, asserted through the delay each OnRetryScheduled reports. The delays are
// real sleeps, which is why pi's tiny 10ms/15ms policy is kept verbatim.
func TestRetryAssistantCallReportsCappedDelays(t *testing.T) {
	maxAgentDelayMs := 15
	policy := &RetryPolicy{Enabled: true, MaxRetries: 4, BaseDelayMs: 10, MaxAgentDelayMs: &maxAgentDelayMs}
	calls := 0
	var got []int
	callbacks := &RetryCallbacks{
		OnRetryScheduled: func(_, _, delayMs int, _ string) { got = append(got, delayMs) },
	}
	produce := func() *AssistantMessage {
		calls++
		if calls == 5 {
			return textResp("recovered")
		}
		return errResp("terminated")
	}
	res := RetryAssistantCall(context.Background(), produce, policy, callbacks)
	if res.StopReason != StopStop {
		t.Fatalf("stop = %v, want StopStop", res.StopReason)
	}
	want := []int{10, 15, 15, 15}
	if len(got) != len(want) {
		t.Fatalf("scheduled delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scheduled delays = %v, want %v", got, want)
		}
	}
}
