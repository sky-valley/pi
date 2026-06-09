package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// TestSummarizationRequestShape pins the faithful summarization request builder
// (pi compaction.ts generateSummary + utils.ts): a dedicated system prompt, the
// serialized conversation wrapped in <conversation>...</conversation>, tool-result
// truncation to 2000 chars, a capped maxTokens, and the read/modified file lists
// appended to the returned summary text.
func TestSummarizationRequestShape(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 200000}},
	})
	defer reg.Unregister()
	model := reg.GetModel()
	model.MaxTokens = 1_000_000 // large so the 0.8*reserve cap wins

	var captured ai.Context
	var capturedMax int
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			captured = req
			if opts != nil && opts.MaxTokens != nil {
				capturedMax = *opts.MaxTokens
			}
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\ncheckpoint"}}, ai.StopStop)
		},
	})

	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})

	bigResult := strings.Repeat("z", 5000) // > 2000 chars, must be truncated
	older := []agent.AgentMessage{
		ai.NewUserText("please refactor the parser", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{
				ai.TextContent{Text: "reading files"},
				ai.ToolCall{ID: "r1", Name: "read", Arguments: map[string]any{"path": "/a/only_read.go"}},
				ai.ToolCall{ID: "e1", Name: "edit", Arguments: map[string]any{"path": "/a/changed.go"}},
				ai.ToolCall{ID: "r2", Name: "read", Arguments: map[string]any{"path": "/a/changed.go"}},
			},
			StopReason: ai.StopToolUse, Timestamp: 2,
		},
		ai.ToolResultMessage{ToolCallID: "r1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: bigResult}}, Timestamp: 3},
	}

	const reserve = 16384
	summary := sess.summarize(context.Background(), older, reserve)

	// System prompt present and exact.
	if captured.SystemPrompt != summarizationSystemPrompt {
		t.Fatalf("summarization system prompt missing/wrong:\n%q", captured.SystemPrompt)
	}

	// Single user message with the <conversation> wrapper + the summarization prompt.
	if len(captured.Messages) != 1 {
		t.Fatalf("expected 1 summarization message, got %d", len(captured.Messages))
	}
	um, ok := captured.Messages[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("expected user message, got %T", captured.Messages[0])
	}
	text := textOf(um.Content)
	if !strings.HasPrefix(text, "<conversation>\n") || !strings.Contains(text, "\n</conversation>\n\n") {
		t.Fatalf("conversation wrapper missing: %q", text)
	}
	if !strings.HasSuffix(text, summarizationPrompt) {
		t.Fatalf("summarization prompt not appended after </conversation>")
	}
	if !strings.Contains(text, "[User]: please refactor the parser") {
		t.Fatalf("user turn not serialized: %q", text)
	}
	if !strings.Contains(text, "[Assistant tool calls]: read(path=\"/a/only_read.go\")") {
		t.Fatalf("tool-call serialization missing: %q", text)
	}

	// Tool result truncated to 2000 chars + marker.
	if !strings.Contains(text, "[... 3000 more characters truncated]") {
		t.Fatalf("tool result not truncated to 2000 chars: %q", text[len(text)-200:])
	}
	if strings.Count(text, "z") > 2100 {
		t.Fatalf("tool result kept too many chars (truncation failed)")
	}

	// maxTokens = floor(0.8 * reserve) since model.MaxTokens is huge.
	if capturedMax != 13107 {
		t.Fatalf("maxTokens = floor(0.8*%d) expected 13107, got %d", reserve, capturedMax)
	}

	// File lists appended to the summary: only_read.go is read-only; changed.go
	// was edited (so excluded from read-files, present in modified-files).
	if !strings.Contains(summary, "<read-files>\n/a/only_read.go\n</read-files>") {
		t.Fatalf("read-files list missing/wrong:\n%s", summary)
	}
	if !strings.Contains(summary, "<modified-files>\n/a/changed.go\n</modified-files>") {
		t.Fatalf("modified-files list missing/wrong:\n%s", summary)
	}
	if strings.Contains(summary, "<read-files>\n/a/changed.go") {
		t.Fatalf("changed.go must not appear in read-files")
	}
}

// TestSummarizationMaxTokensClampedByModel verifies model.maxTokens caps the
// 0.8*reserve budget when it is smaller.
func TestSummarizationMaxTokensClampedByModel(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 200000}},
	})
	defer reg.Unregister()
	model := reg.GetModel()
	model.MaxTokens = 4096 // smaller than floor(0.8*16384)=13107

	var capturedMax int
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			if opts != nil && opts.MaxTokens != nil {
				capturedMax = *opts.MaxTokens
			}
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
		},
	})

	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})
	sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384)

	if capturedMax != 4096 {
		t.Fatalf("expected maxTokens clamped to model.MaxTokens 4096, got %d", capturedMax)
	}
}
