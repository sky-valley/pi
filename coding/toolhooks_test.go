package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// TestBeforeToolCallPermissionGate verifies the SDK's BeforeToolCall hook can
// block a tool call — the native equivalent of pi's tool_call extension hook.
func TestBeforeToolCallPermissionGate(t *testing.T) {
	dir := t.TempDir()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("bash", map[string]any{"command": "rm -rf /"}, "c1"),
		}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)),
	})

	var blocked bool
	sess := NewSession(SessionOptions{
		Model: reg.GetModel(),
		Cwd:   dir,
		BeforeToolCall: func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			if c.ToolCall.Name == "bash" {
				if cmd, _ := c.Args["command"].(string); strings.Contains(cmd, "rm -rf") {
					blocked = true
					return &agent.BeforeToolCallResult{Block: true, Reason: "destructive command blocked by policy"}
				}
			}
			return nil
		},
	})

	res, err := sess.Run(context.Background(), "clean up")
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("BeforeToolCall hook was not invoked")
	}
	// The tool result for the blocked call must be an error carrying the reason.
	var found bool
	for _, m := range res.Messages {
		if tr, ok := m.(ai.ToolResultMessage); ok && tr.ToolCallID == "c1" {
			found = true
			if !tr.IsError {
				t.Fatal("blocked tool result should be an error")
			}
			if txt, _ := tr.Content[0].(ai.TextContent); !strings.Contains(txt.Text, "policy") {
				t.Fatalf("block reason missing: %#v", tr.Content)
			}
		}
	}
	if !found {
		t.Fatal("no tool result recorded for blocked call")
	}
}
