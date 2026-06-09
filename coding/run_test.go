package coding

import (
	"context"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

func TestSessionRunAggregatesUsageAcrossToolLoop(t *testing.T) {
	dir := t.TempDir()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()

	// Two provider turns: first calls a tool, second answers. The faux provider
	// estimates usage per call, so the aggregate should exceed either single turn.
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("ls", map[string]any{}, "c1"),
		}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			ai.TextContent{Text: "all set"},
		}, ai.StopStop)),
	})

	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: dir, Tools: CreateAllTools(dir)})
	res, err := sess.Run(context.Background(), "list files")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "all set" {
		t.Fatalf("unexpected final text: %q", res.Text)
	}
	if res.StopReason != ai.StopStop {
		t.Fatalf("unexpected stop reason: %s", res.StopReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "ls" {
		t.Fatalf("tool calls wrong: %#v", res.ToolCalls)
	}
	// Messages: user, assistant(toolcall), toolResult, assistant(final).
	if len(res.Messages) != 4 {
		t.Fatalf("expected 4 new messages, got %d", len(res.Messages))
	}
	// Usage was aggregated across both assistant turns (faux estimates >0 output).
	if res.Usage.Output <= 0 {
		t.Fatalf("expected aggregated output tokens > 0, got %+v", res.Usage)
	}
	if res.Usage.TotalTokens < res.Usage.Output {
		t.Fatalf("aggregate total tokens look wrong: %+v", res.Usage)
	}
}

func TestSessionRunSurfacesError(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(&ai.AssistantMessage{StopReason: ai.StopError, ErrorMessage: "kaboom"}),
	})
	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), Tools: nil})
	res, err := sess.Run(context.Background(), "go")
	if err == nil {
		t.Fatal("expected error from Run")
	}
	if res == nil || res.ErrorMessage != "kaboom" {
		t.Fatalf("expected error result, got %#v", res)
	}
}
