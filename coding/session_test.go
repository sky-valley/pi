package coding

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// TestSessionEndToEndWithFauxProvider drives a full coding session through the
// real agent loop + faux provider: the model asks to write a file via the write
// tool, then reports done. Verifies the tool ran and the file was created.
func TestSessionEndToEndWithFauxProvider(t *testing.T) {
	dir := t.TempDir()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", Reasoning: false}},
	})
	defer reg.Unregister()

	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("write", map[string]any{"path": "hello.txt", "content": "hi from pi\n"}, "call_1"),
		}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			ai.TextContent{Text: "Done — wrote hello.txt."},
		}, ai.StopStop)),
	})

	sess := NewSession(SessionOptions{
		Model: reg.GetModel(),
		Cwd:   dir,
		Tools: CreateAllTools(dir),
	})

	var out bytes.Buffer
	final, err := sess.RunPrint(context.Background(), &out, "create hello.txt with a greeting")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "Done") {
		t.Fatalf("unexpected final text: %q", final)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(data) != "hi from pi\n" {
		t.Fatalf("file not written by tool: %v / %q", err, data)
	}
	// Output should show the tool activity.
	if !strings.Contains(out.String(), "write") {
		t.Fatalf("tool activity not rendered: %q", out.String())
	}
}

func TestSessionUsesCodingSystemPrompt(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	var capturedSystem string
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			capturedSystem = req.SystemPrompt
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
		},
	})
	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: "/tmp/proj", Tools: CreateCodingTools("/tmp/proj")})
	if _, err := sess.RunPrint(context.Background(), &bytes.Buffer{}, "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedSystem, "expert coding assistant operating inside pi") {
		t.Fatalf("system prompt not applied: %q", capturedSystem)
	}
}

func TestResolveModelFromCatalog(t *testing.T) {
	m, err := ResolveModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "anthropic" || m.Api != ai.APIAnthropicMessages {
		t.Fatalf("unexpected model: %#v", m)
	}
	// Bare id resolution.
	if _, err := ResolveModel("claude-haiku-4-5"); err != nil {
		t.Fatalf("bare id resolution failed: %v", err)
	}
	if _, err := ResolveModel("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

var _ = agent.ThinkOff
