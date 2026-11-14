package coding

import (
	"context"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// The suite below ports
// test/suite/regressions/12119-copy-after-abort-or-error.test.ts (upstream
// ede360e4d): /copy uses LastAssistantText, which took the last assistant
// message. After an abort during a tool call the last messages are a
// tool-call-only message and an empty one; after a provider error it is an
// empty error message. Neither has text, so /copy had nothing to copy while
// the previous answer was on screen.

func TestLastAssistantText12119AfterAbortDuringToolCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	toolStarted := make(chan struct{})
	blockingTool := agent.AgentTool{
		Name:        "block",
		Label:       "Block",
		Description: "Blocks until aborted",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, _ string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			close(toolStarted)
			<-ctx.Done()
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "tool aborted"}}, Details: map[string]any{}}, nil
		},
	}
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), CustomTools: []agent.AgentTool{blockingTool}})
	reg.SetResponses([]providers.FauxResponseStep{
		fauxText("Here is the answer."),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("block", map[string]any{})}, ai.StopToolUse)),
	})

	if _, err := sess.Run(context.Background(), "first question"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sess.Run(context.Background(), "run the blocking tool")
	}()
	<-toolStarted
	sess.Abort()
	<-done

	if got := sess.LastAssistantText(); got != "Here is the answer." {
		t.Fatalf("LastAssistantText() = %q, want the previous answer", got)
	}
}

func TestLastAssistantText12119AfterProviderError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	reg.SetResponses([]providers.FauxResponseStep{
		fauxText("Here is the answer."),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{}, ai.StopError, providers.FauxMessageOptions{ErrorMessage: "400 invalid_request_error"})),
	})

	if _, err := sess.Run(context.Background(), "first question"); err != nil {
		t.Fatal(err)
	}
	_, _ = sess.Run(context.Background(), "second question")

	if got := sess.LastAssistantText(); got != "Here is the answer." {
		t.Fatalf("LastAssistantText() = %q, want the previous answer", got)
	}
}

// A reply that ended normally stays the last reply even without text: pi
// returns undefined, which the port spells "".
func TestLastAssistantText12119KeepsEmptyNormalReply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	reg.SetResponses([]providers.FauxResponseStep{
		fauxText("Here is the answer."),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{}, ai.StopStop)),
	})

	if _, err := sess.Run(context.Background(), "first question"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Run(context.Background(), "second question"); err != nil {
		t.Fatal(err)
	}

	if got := sess.LastAssistantText(); got != "" {
		t.Fatalf("LastAssistantText() = %q, want \"\"", got)
	}
}
