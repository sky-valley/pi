package coding

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// regression 6647-compaction-retries-transient-stream-drop.test.ts (upstream
// 8e53e0e49): a summary request that fails with a transient error is retried
// under the compaction's retry policy, through ai.RetryAssistantCall, so one
// dropped stream no longer fails the whole compaction. Aborts and
// non-retryable errors are not retried.

// retryScriptSession is a session whose stream answers the n-th summary
// request with script[n], or the last entry once the script runs out.
func retryScriptSession(t *testing.T, script []*ai.AssistantMessage) (*Session, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	stream := func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		mu.Lock()
		message := *script[min(calls, len(script)-1)]
		calls++
		mu.Unlock()
		message.Api, message.Provider, message.Model = model.Api, model.Provider, model.ID
		s := ai.NewAssistantMessageEventStream()
		if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
			s.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: message.StopReason, Error: &message})
		} else {
			s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: message.StopReason, Message: &message})
		}
		s.End()
		return s
	}
	model := &ai.Model{ID: "faux-1", Api: "faux", Provider: "faux", ContextWindow: 1000, Input: []string{"text"}}
	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, StreamFn: stream})
	return sess, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func retryErrorMessage(text string) *ai.AssistantMessage {
	return &ai.AssistantMessage{Content: ai.ContentList{}, StopReason: ai.StopError, ErrorMessage: text}
}

// retryCompactable is pi's seedCompactableSession under keepRecentTokens 1:
// one turn whose prefix is summarized in a single request.
func retryCompactable() []agent.AgentMessage {
	return []agent.AgentMessage{
		ai.NewUserText("message to compact", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "assistant response to compact"}}, StopReason: ai.StopStop, Timestamp: 2, Usage: ai.Usage{TotalTokens: 100}},
	}
}

type retryEvents struct {
	scheduled []string
	finished  int
}

func (e *retryEvents) callbacks() *ai.RetryCallbacks {
	return &ai.RetryCallbacks{
		OnRetryScheduled: func(attempt, maxAttempts, _ int, errorMessage string) {
			e.scheduled = append(e.scheduled, fmt.Sprintf("%d/%d/%s", attempt, maxAttempts, errorMessage))
		},
		OnRetryFinished: func(bool, int, string) { e.finished++ },
	}
}

func compactWithRetry(t *testing.T, ctx context.Context, sess *Session, policy *ai.RetryPolicy, events *retryEvents) *compactionState {
	t.Helper()
	state := &compactionState{settings: CompactionSettings{
		Enabled: true, ReserveTokens: 1000, KeepRecentTokens: 1,
		Retry: policy, RetryCallbacks: events.callbacks(),
	}}
	sess.compact(ctx, state, retryCompactable())
	return state
}

func TestCompactionRetriesTransientSummaryErrors(t *testing.T) {
	t.Run("retries a transient terminated error and compacts", func(t *testing.T) {
		success := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "recovered summary"}}, StopReason: ai.StopStop}
		sess, calls := retryScriptSession(t, []*ai.AssistantMessage{retryErrorMessage("terminated"), retryErrorMessage("terminated"), success})
		var events retryEvents
		state := compactWithRetry(t, context.Background(), sess, &ai.RetryPolicy{Enabled: true, MaxRetries: 3}, &events)
		if state.checkpoint == nil || !strings.Contains(state.checkpoint.summary, "recovered summary") {
			t.Fatalf("checkpoint = %+v, want the recovered summary", state.checkpoint)
		}
		if calls() != 3 {
			t.Fatalf("summary requests = %d, want 3 (1 attempt + 2 retries)", calls())
		}
		if want := []string{"1/3/terminated", "2/3/terminated"}; strings.Join(events.scheduled, ",") != strings.Join(want, ",") || events.finished != 1 {
			t.Fatalf("retries scheduled %q, finished %d; want %q and 1", events.scheduled, events.finished, want)
		}
	})
	t.Run("does not retry a non-retryable error", func(t *testing.T) {
		sess, calls := retryScriptSession(t, []*ai.AssistantMessage{retryErrorMessage("insufficient_quota")})
		var events retryEvents
		state := compactWithRetry(t, context.Background(), sess, &ai.RetryPolicy{Enabled: true, MaxRetries: 3}, &events)
		if state.checkpoint != nil || calls() != 1 || len(events.scheduled) != 0 {
			t.Fatalf("checkpoint %v, %d requests, %d retries; want a failed compaction after 1 request", state.checkpoint, calls(), len(events.scheduled))
		}
	})
	t.Run("does not retry when retry is disabled", func(t *testing.T) {
		sess, calls := retryScriptSession(t, []*ai.AssistantMessage{retryErrorMessage("terminated")})
		var events retryEvents
		state := compactWithRetry(t, context.Background(), sess, &ai.RetryPolicy{Enabled: false, MaxRetries: 3}, &events)
		if state.checkpoint != nil || calls() != 1 || len(events.scheduled) != 0 {
			t.Fatalf("checkpoint %v, %d requests, %d retries; want a failed compaction after 1 request", state.checkpoint, calls(), len(events.scheduled))
		}
	})
	t.Run("stops retrying after maxRetries", func(t *testing.T) {
		sess, calls := retryScriptSession(t, []*ai.AssistantMessage{retryErrorMessage("terminated")})
		var events retryEvents
		state := compactWithRetry(t, context.Background(), sess, &ai.RetryPolicy{Enabled: true, MaxRetries: 2}, &events)
		if state.checkpoint != nil || calls() != 3 || len(events.scheduled) != 2 || events.finished != 1 {
			t.Fatalf("checkpoint %v, %d requests, %d retries, %d finished; want a failed compaction after 3 requests, 2 retries, 1 finished",
				state.checkpoint, calls(), len(events.scheduled), events.finished)
		}
	})
	t.Run("an abort during the backoff ends the compaction", func(t *testing.T) {
		sess, calls := retryScriptSession(t, []*ai.AssistantMessage{retryErrorMessage("terminated")})
		ctx, cancel := context.WithCancel(context.Background())
		var events retryEvents
		policy := &ai.RetryPolicy{Enabled: true, MaxRetries: 5, BaseDelayMs: 30_000}
		callbacks := events.callbacks()
		scheduled := callbacks.OnRetryScheduled
		callbacks.OnRetryScheduled = func(attempt, maxAttempts, delayMs int, errorMessage string) {
			scheduled(attempt, maxAttempts, delayMs, errorMessage)
			cancel()
		}
		state := &compactionState{settings: CompactionSettings{
			Enabled: true, ReserveTokens: 1000, KeepRecentTokens: 1, Retry: policy, RetryCallbacks: callbacks,
		}}
		done := make(chan struct{})
		go func() {
			sess.compact(ctx, state, retryCompactable())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the abort did not end the retry backoff")
		}
		if state.checkpoint != nil || calls() != 1 || len(events.scheduled) != 1 || events.finished != 1 {
			t.Fatalf("checkpoint %v, %d requests, %d retries, %d finished; want an aborted compaction after 1 request",
				state.checkpoint, calls(), len(events.scheduled), events.finished)
		}
	})
}
