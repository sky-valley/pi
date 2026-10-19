package coding

import (
	"context"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// pi's createAgentSession streams every request through
// ModelRuntime.streamSimple, which asserts a chat model before it prepares or
// dispatches anything (upstream a328aa89a; coding-agent
// model-runtime-images.test.ts "rejects image models at every chat entry point
// before provider dispatch"). A session over a non-chat model therefore ends
// its turn with pi's assertion as the error, and no request is made.

func imageSessionModel() *ai.Model {
	return &ai.Model{Type: ai.ModelTypeImage, ID: "img", Api: "faux", Provider: "faux",
		ContextWindow: 100000, MaxTokens: 8192}
}

const imageSessionModelError = "Model faux/img is not a chat model"

// countingStream is a StreamFn that records a dispatch and answers "ok".
func countingStream(dispatches *int) func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	return func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		*dispatches++
		s := ai.NewAssistantMessageEventStream()
		msg := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Api: model.Api,
			Provider: model.Provider, Model: model.ID, StopReason: ai.StopStop}
		s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ai.StopStop, Message: msg})
		s.End()
		return s
	}
}

func TestSessionRejectsNonChatModelBeforeDispatch(t *testing.T) {
	dispatches := 0
	sess := newTestSession(t, SessionOptions{Model: imageSessionModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
		StreamFn: countingStream(&dispatches)})

	res, err := sess.Run(context.Background(), "hi")
	if err == nil || err.Error() != imageSessionModelError {
		t.Fatalf("Run error = %v, want %q", err, imageSessionModelError)
	}
	if res == nil || res.StopReason != ai.StopError || res.ErrorMessage != imageSessionModelError {
		t.Fatalf("Run result = %+v, want an error turn carrying %q", res, imageSessionModelError)
	}
	if dispatches != 0 {
		t.Fatalf("dispatches = %d, want none: the assertion runs before the request", dispatches)
	}
}

// The default stream path asserts too. Without the assertion ai.StreamSimple
// would fail for a different reason — no provider is registered for "faux" —
// so the message tells the two apart.
func TestSessionDefaultStreamRejectsNonChatModel(t *testing.T) {
	sess := newTestSession(t, SessionOptions{Model: imageSessionModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})
	_, err := sess.Run(context.Background(), "hi")
	if err == nil || err.Error() != imageSessionModelError {
		t.Fatalf("Run error = %v, want %q", err, imageSessionModelError)
	}
}

// Summaries reuse the agent's stream function in pi (agent.streamFunction),
// so they meet the same assertion: the summary fails without a request.
func TestSessionSummarizationRejectsNonChatModel(t *testing.T) {
	dispatches := 0
	sess := newTestSession(t, SessionOptions{Model: imageSessionModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
		StreamFn: countingStream(&dispatches)})

	if summary, ok := sess.completeSummarization(context.Background(), "summarize", 1000, "s-1"); ok {
		t.Fatalf("summarization succeeded with %q, want it to fail on the chat assertion", summary)
	}
	if dispatches != 0 {
		t.Fatalf("dispatches = %d, want none", dispatches)
	}

	// The same session over a chat model does dispatch, so the zero above is
	// the assertion and not an unreachable stream function.
	chat := *imageSessionModel()
	chat.Type = ""
	sess = newTestSession(t, SessionOptions{Model: &chat, Cwd: t.TempDir(), NoTools: NoToolsAll,
		StreamFn: countingStream(&dispatches)})
	if _, ok := sess.completeSummarization(context.Background(), "summarize", 1000, "s-1"); !ok {
		t.Fatal("summarization over a chat model must succeed")
	}
	if dispatches != 1 {
		t.Fatalf("dispatches = %d, want 1", dispatches)
	}
}
