package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

func TestShouldCompactThreshold(t *testing.T) {
	s := CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}
	if shouldCompact(100, 200000, s) {
		t.Fatal("small context should not compact")
	}
	if !shouldCompact(190000, 200000, s) {
		t.Fatal("context above window-reserve should compact")
	}
	if shouldCompact(190000, 200000, CompactionSettings{Enabled: false}) {
		t.Fatal("disabled compaction must never trigger")
	}
}

func TestFindCutIndexSkipsToolResult(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1000 tokens each
	messages := []agent.AgentMessage{
		ai.NewUserText(big, 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "t", Name: "read", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse, Timestamp: 2},
		ai.ToolResultMessage{ToolCallID: "t", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: big}}, Timestamp: 3},
		ai.NewUserText(big, 4),
	}
	cut := findCutIndex(messages, 1500)
	// The kept run must not start on a tool-result.
	if cut >= 0 && cut < len(messages) && messages[cut].MessageRole() == ai.RoleToolResult {
		t.Fatalf("cut landed on a tool-result at index %d", cut)
	}
}

func TestCompactionSummarizesAndKeepsRecent(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 1000}}, // tiny window forces compaction
	})
	defer reg.Unregister()
	model := reg.GetModel()

	// The summarization call returns this checkpoint text.
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nfinish the port"}}, ai.StopStop)),
	})

	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})
	sess.EnableCompaction(CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 400})

	// Build a transcript large enough to exceed the 1000-token window.
	big := strings.Repeat("y", 4000)
	var messages []agent.AgentMessage
	for i := 0; i < 6; i++ {
		messages = append(messages, ai.NewUserText(big, int64(i)))
		messages = append(messages, ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: int64(i)})
	}

	state := &compactionState{settings: CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 400}}
	out := sess.compact(context.Background(), state, messages)

	if len(out) >= len(messages) {
		t.Fatalf("compaction did not reduce messages: %d -> %d", len(messages), len(out))
	}
	first, ok := out[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("first compacted message should be the summary user message, got %T", out[0])
	}
	tc, _ := first.Content[0].(ai.TextContent)
	if !strings.Contains(tc.Text, "finish the port") {
		t.Fatalf("summary checkpoint missing: %q", tc.Text)
	}
}

func TestUsageAwareTokenEstimate(t *testing.T) {
	// When the last assistant turn reports real usage, that drives the estimate
	// (plus a heuristic for trailing messages) rather than the char/4 guess.
	messages := []agent.AgentMessage{
		ai.NewUserText("short", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.TextContent{Text: "ok"}},
			Usage:      ai.Usage{TotalTokens: 50000},
			StopReason: ai.StopStop, Timestamp: 2,
		},
		ai.NewUserText("a follow-up question", 3),
	}
	got := estimateContextTokensUsageAware(messages)
	if got < 50000 {
		t.Fatalf("usage-aware estimate should be >= the reported 50000, got %d", got)
	}
	if pure := EstimateContextTokens(messages); got <= pure*10 {
		t.Fatalf("usage-aware (%d) should dominate the pure heuristic (%d)", got, pure)
	}
}
