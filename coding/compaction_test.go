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

// TestFindCutPointSnapsForward pins pi's cutPoints algorithm (compaction.ts:380-448):
// when the keep-budget crossing lands on a tool result, the cut snaps FORWARD to
// the first valid cut point at or after it, so the boundary tool-result goes INTO
// the summarized portion. (The previous Go implementation snapped backward,
// keeping the tool result — that pinned a bug and was replaced.)
func TestFindCutPointSnapsForward(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1000 tokens each
	messages := []agent.AgentMessage{
		ai.NewUserText(big, 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "t", Name: "read", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse, Timestamp: 2},
		ai.ToolResultMessage{ToolCallID: "t", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: big}}, Timestamp: 3},
		ai.NewUserText(big, 4),
	}
	// Walking back: user@3 (1000) then toolResult@2 (1000) crosses 1500 at index 2.
	// First cut point >= 2 is the user at index 3 (forward snap).
	cp := findCutPoint(messages, 0, len(messages), 1500)
	if cp.firstKeptIndex != 3 {
		t.Fatalf("expected forward snap to index 3, got %d", cp.firstKeptIndex)
	}
	if cp.isSplitTurn {
		t.Fatal("cut on a user message must not be a split turn")
	}
}

// TestFindCutPointKeepEverythingEdge pins the default when the keep budget is
// never reached: the cut stays at the first valid cut point (keep everything).
func TestFindCutPointKeepEverythingEdge(t *testing.T) {
	messages := []agent.AgentMessage{
		ai.NewUserText("a", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "b"}}, StopReason: ai.StopStop, Timestamp: 2},
	}
	cp := findCutPoint(messages, 0, len(messages), 1_000_000)
	if cp.firstKeptIndex != 0 {
		t.Fatalf("expected keep-everything cut at 0, got %d", cp.firstKeptIndex)
	}

	// No valid cut points (all tool results) => startIndex.
	tr := []agent.AgentMessage{
		ai.ToolResultMessage{ToolCallID: "t", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "x"}}, Timestamp: 1},
	}
	cp = findCutPoint(tr, 0, len(tr), 1)
	if cp.firstKeptIndex != 0 || cp.isSplitTurn {
		t.Fatalf("expected startIndex cut with no cut points, got %+v", cp)
	}
}

// TestFindCutPointSplitTurn pins split-turn detection: a cut landing on an
// assistant message marks the turn started by the nearest preceding user message.
func TestFindCutPointSplitTurn(t *testing.T) {
	big := strings.Repeat("x", 4000)
	messages := []agent.AgentMessage{
		ai.NewUserText(big, 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 2},
		ai.NewUserText(big, 3),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 4},
	}
	// keep 400: crossing at the last assistant (index 3), a valid cut point itself.
	cp := findCutPoint(messages, 0, len(messages), 400)
	if cp.firstKeptIndex != 3 {
		t.Fatalf("expected cut at 3, got %d", cp.firstKeptIndex)
	}
	if !cp.isSplitTurn || cp.turnStartIndex != 2 {
		t.Fatalf("expected split turn starting at 2, got %+v", cp)
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
	sess.EnableCompaction(CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 1500})

	// Build a transcript large enough to exceed the 1000-token window. With
	// keepRecentTokens 1500 the cut lands on the user at index 10 (no split).
	big := strings.Repeat("y", 4000)
	var messages []agent.AgentMessage
	for i := 0; i < 6; i++ {
		messages = append(messages, ai.NewUserText(big, int64(i)))
		messages = append(messages, ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: int64(i)})
	}

	state := &compactionState{settings: CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 1500}}
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

// bigTranscript builds n user/assistant pairs of ~1000-token messages. The
// assistant at pair index 0 carries a read tool call for file-ops tracking tests.
func bigTranscript(n int) []agent.AgentMessage {
	big := strings.Repeat("y", 4000)
	var messages []agent.AgentMessage
	for i := 0; i < n; i++ {
		messages = append(messages, ai.NewUserText(big, int64(2*i)))
		content := ai.ContentList{ai.TextContent{Text: big}}
		if i == 0 {
			content = append(content, ai.ToolCall{ID: "r0", Name: "read", Arguments: map[string]any{"path": "/a/seen.go"}})
		}
		messages = append(messages, ai.AssistantMessage{Content: content, StopReason: ai.StopStop, Timestamp: int64(2*i + 1)})
	}
	return messages
}

// compactionTestSettings: window 1000 (set on the faux model), reserve 200,
// keep 1500 — the cut lands on a user message (no split) for ...user,assistant
// transcripts of ~1000-token messages.
var compactionTestSettings = CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 1500}

func newCompactionTestSession(t *testing.T) (*Session, *providers.FauxProviderRegistration) {
	t.Helper()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 1000}},
	})
	t.Cleanup(reg.Unregister)
	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})
	return sess, reg
}

func checkpointText(t *testing.T, m agent.AgentMessage) string {
	t.Helper()
	um, ok := m.(ai.UserMessage)
	if !ok {
		t.Fatalf("expected checkpoint user message, got %T", m)
	}
	return textOf(um.Content)
}

// TestCompactionPermanentAfterSmallUsage locks the A1 regression fix: once a
// compaction happened, a later request whose usage-based estimate is small must
// still get [checkpoint + recent], never the full history again.
func TestCompactionPermanentAfterSmallUsage(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nfirst summary"}}, ai.StopStop)),
	})

	state := &compactionState{settings: compactionTestSettings}
	messages := bigTranscript(6) // 12 messages, cut at index 10

	out := sess.compact(context.Background(), state, messages)
	if len(out) != 3 { // checkpoint + messages[10:12]
		t.Fatalf("first compaction: expected 3 messages, got %d", len(out))
	}
	if state.prefixLen != 10 || state.summary == "" {
		t.Fatalf("compaction state not recorded: prefixLen=%d summary=%q", state.prefixLen, state.summary)
	}

	// The next turn reports SMALL usage (the provider saw the compacted context).
	messages = append(messages,
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.TextContent{Text: "ok"}},
			Usage:      ai.Usage{TotalTokens: 100},
			StopReason: ai.StopStop, Timestamp: 100,
		},
		ai.NewUserText("next question", 101),
	)

	out = sess.compact(context.Background(), state, messages)
	if len(out) != 1+len(messages)-10 {
		t.Fatalf("compacted view lost: expected %d messages, got %d", 1+len(messages)-10, len(out))
	}
	if !strings.Contains(checkpointText(t, out[0]), "first summary") {
		t.Fatal("checkpoint summary missing after small-usage request")
	}
	if reg.PendingResponseCount() != 0 {
		t.Fatal("unexpected extra summarization call")
	}
	// A FULL history (len == len(messages)) must never come back.
	if len(out) >= len(messages) {
		t.Fatalf("compaction reverted to full history: %d -> %d", len(messages), len(out))
	}
}

// TestCompactionExtendsWithPreviousSummary locks A1(b) + I6: when context grows
// past the threshold again, the second compaction covers a larger prefix, the
// summarization request carries the previous summary in <previous-summary> tags
// with the UPDATE_SUMMARIZATION_PROMPT, and file ops from the first compaction
// carry over into the new summary's file lists.
func TestCompactionExtendsWithPreviousSummary(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nfirst summary"}}, ai.StopStop)),
	})

	state := &compactionState{settings: compactionTestSettings}
	messages := bigTranscript(6)
	sess.compact(context.Background(), state, messages)
	firstSummary := state.summary
	if state.prefixLen != 10 {
		t.Fatalf("first compaction prefixLen = %d, want 10", state.prefixLen)
	}
	// The first summary carries the file-ops appendix (pi stores it that way).
	if !strings.Contains(firstSummary, "<read-files>\n/a/seen.go\n</read-files>") {
		t.Fatalf("first summary missing file ops appendix:\n%s", firstSummary)
	}

	// Grow the transcript with big turns; the last assistant reports big usage.
	big := strings.Repeat("y", 4000)
	messages = append(messages,
		ai.NewUserText(big, 12),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, Usage: ai.Usage{TotalTokens: 5000}, StopReason: ai.StopStop, Timestamp: 13},
		ai.NewUserText(big, 14),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, Usage: ai.Usage{TotalTokens: 5000}, StopReason: ai.StopStop, Timestamp: 15},
	)

	var captured string
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			captured = textOf(req.Messages[0].(ai.UserMessage).Content)
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nupdated summary"}}, ai.StopStop)
		},
	})

	out := sess.compact(context.Background(), state, messages)

	// Second compaction covers a larger prefix (cut at the user at index 14).
	if state.prefixLen != 14 {
		t.Fatalf("second compaction prefixLen = %d, want 14", state.prefixLen)
	}
	if len(out) != 3 { // checkpoint + messages[14:16]
		t.Fatalf("expected 3 messages after extension, got %d", len(out))
	}
	if !strings.Contains(checkpointText(t, out[0]), "updated summary") {
		t.Fatal("extended summary not applied")
	}

	// Request shape: <conversation> ... </conversation>\n\n<previous-summary>\n
	// {prev}\n</previous-summary>\n\n{UPDATE_SUMMARIZATION_PROMPT}.
	if !strings.Contains(captured, "<previous-summary>\n"+firstSummary+"\n</previous-summary>\n\n") {
		t.Fatalf("previous summary tags missing/wrong:\n%s", captured)
	}
	if !strings.HasSuffix(captured, updateSummarizationPrompt) {
		t.Fatal("update summarization prompt variant not used")
	}
	if strings.HasSuffix(captured, summarizationPrompt) {
		t.Fatal("initial summarization prompt must not be used on re-compaction")
	}
	// Only the NEW messages (after the previous boundary) are serialized.
	if !strings.Contains(captured, "[Assistant]: "+big) {
		t.Fatal("new messages not serialized")
	}

	// File-ops carryover: the second summarized chunk has no tool calls, but the
	// read from the first compaction must persist in the new appendix.
	if !strings.Contains(state.summary, "<read-files>\n/a/seen.go\n</read-files>") {
		t.Fatalf("file ops did not carry over:\n%s", state.summary)
	}
}

// TestCompactionStaysCompactedDuringToolLoop locks A1(c): a multi-turn tool loop
// after compaction gets the compacted view on every request without
// re-summarizing.
func TestCompactionStaysCompactedDuringToolLoop(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nloop summary"}}, ai.StopStop)),
	})

	state := &compactionState{settings: compactionTestSettings}
	messages := bigTranscript(6)
	sess.compact(context.Background(), state, messages)

	for i := 0; i < 3; i++ {
		messages = append(messages,
			ai.AssistantMessage{
				Content:    ai.ContentList{ai.ToolCall{ID: "t", Name: "read", Arguments: map[string]any{"path": "/x.go"}}},
				Usage:      ai.Usage{TotalTokens: 150},
				StopReason: ai.StopToolUse, Timestamp: int64(200 + 2*i),
			},
			ai.ToolResultMessage{ToolCallID: "t", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "contents"}}, Timestamp: int64(201 + 2*i)},
		)
		out := sess.compact(context.Background(), state, messages)
		if len(out) != 1+len(messages)-10 {
			t.Fatalf("iteration %d: expected compacted view of %d messages, got %d", i, 1+len(messages)-10, len(out))
		}
		if !strings.Contains(checkpointText(t, out[0]), "loop summary") {
			t.Fatalf("iteration %d: checkpoint missing", i)
		}
	}
	if reg.PendingResponseCount() != 0 {
		t.Fatal("tool loop triggered extra summarization")
	}
}

// TestCompactionSplitTurnSummaries locks the TURN_PREFIX flow (pi compaction
// ts:725-800): a cut landing mid-turn produces a history summary (0.8*reserve
// budget) and a turn-prefix summary (TURN_PREFIX prompt, 0.5*reserve budget)
// merged with pi's exact separator.
func TestCompactionSplitTurnSummaries(t *testing.T) {
	sess, reg := newCompactionTestSession(t)

	type call struct {
		text      string
		maxTokens int
	}
	var calls []call
	step := func(text string) providers.FauxResponseStep {
		return func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			c := call{text: textOf(req.Messages[0].(ai.UserMessage).Content)}
			if opts != nil && opts.MaxTokens != nil {
				c.maxTokens = *opts.MaxTokens
			}
			calls = append(calls, c)
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: text}}, ai.StopStop)
		}
	}
	reg.SetResponses([]providers.FauxResponseStep{step("HIST"), step("PREFIX")})

	// keep 400: the crossing lands on the final assistant (index 11) => split
	// turn started by the user at index 10.
	settings := CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 400}
	state := &compactionState{settings: settings}
	messages := bigTranscript(6)

	out := sess.compact(context.Background(), state, messages)

	if len(calls) != 2 {
		t.Fatalf("expected 2 summarization calls, got %d", len(calls))
	}
	// History request: initial prompt, 0.8 * reserve budget.
	if !strings.HasSuffix(calls[0].text, summarizationPrompt) {
		t.Fatal("history request must use the initial summarization prompt")
	}
	if calls[0].maxTokens != 160 { // floor(0.8*200)
		t.Fatalf("history maxTokens = %d, want 160", calls[0].maxTokens)
	}
	// Turn-prefix request: TURN_PREFIX prompt, 0.5 * reserve budget.
	if !strings.HasSuffix(calls[1].text, turnPrefixSummarizationPrompt) {
		t.Fatal("turn-prefix request must use the turn-prefix prompt")
	}
	if calls[1].maxTokens != 100 { // floor(0.5*200)
		t.Fatalf("turn-prefix maxTokens = %d, want 100", calls[1].maxTokens)
	}

	// Merged summary format (compaction.ts:800).
	if !strings.Contains(checkpointText(t, out[0]), "HIST\n\n---\n\n**Turn Context (split turn):**\n\nPREFIX") {
		t.Fatalf("merged split-turn summary missing:\n%s", checkpointText(t, out[0]))
	}
	if state.prefixLen != 11 {
		t.Fatalf("split-turn prefixLen = %d, want 11", state.prefixLen)
	}
	if len(out) != 2 { // checkpoint + kept assistant
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
}

// TestSummaryTextBlocksJoinedWithNewline locks I3: assistant text blocks of the
// summarization response join with "\n" (pi compaction.js join("\n")).
func TestSummaryTextBlocksJoinedWithNewline(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			ai.TextContent{Text: "part one"},
			ai.TextContent{Text: "part two"},
		}, ai.StopStop)),
	})
	got := sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384)
	if got != "part one\npart two" {
		t.Fatalf("text blocks not joined with newline: %q", got)
	}
}

// TestUsageEstimateSkipsAbortedAndError locks I4: aborted/error assistant
// messages are skipped when picking the usage anchor (pi compaction.ts:143-151).
func TestUsageEstimateSkipsAbortedAndError(t *testing.T) {
	valid := ai.AssistantMessage{
		Content:    ai.ContentList{ai.TextContent{Text: "ok"}},
		Usage:      ai.Usage{TotalTokens: 50000},
		StopReason: ai.StopStop, Timestamp: 1,
	}
	aborted := ai.AssistantMessage{
		Content:    ai.ContentList{ai.TextContent{Text: "partial"}},
		Usage:      ai.Usage{TotalTokens: 70000},
		StopReason: ai.StopAborted, Timestamp: 2,
	}
	errored := ai.AssistantMessage{
		Content:      ai.ContentList{},
		Usage:        ai.Usage{TotalTokens: 90000},
		StopReason:   ai.StopError,
		ErrorMessage: "boom", Timestamp: 3,
	}

	// Anchor must be the earlier VALID usage, not the later aborted/error ones.
	got := estimateContextTokensUsageAware([]agent.AgentMessage{valid, aborted, errored})
	if got < 50000 || got >= 70000 {
		t.Fatalf("expected anchor at valid usage 50000 (+trailing), got %d", got)
	}

	// Only aborted/error usage present => pure heuristic.
	got = estimateContextTokensUsageAware([]agent.AgentMessage{ai.NewUserText("short", 1), aborted, errored})
	if got >= 1000 {
		t.Fatalf("aborted/error usage must not anchor the estimate, got %d", got)
	}
}

// TestSummarizationPassesReasoningAndHeaders locks I7: the summarization request
// carries the session's headers and — for reasoning models with a non-off
// thinking level — the thinking level (pi compaction.ts:526-539).
func TestSummarizationPassesReasoningAndHeaders(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-r", Reasoning: true, ContextWindow: 200000}},
	})
	defer reg.Unregister()

	var gotReasoning ai.ThinkingLevel
	var gotHeaders map[string]string
	capture := func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
		gotReasoning = opts.Reasoning
		gotHeaders = opts.Headers
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{capture, capture})

	sess := NewSession(SessionOptions{
		Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
		Headers: map[string]string{"X-Parity": "1"},
	})

	sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384)
	if gotReasoning != ai.ThinkingMedium { // session default thinking level
		t.Fatalf("reasoning level not passed: %q", gotReasoning)
	}
	if gotHeaders["X-Parity"] != "1" {
		t.Fatalf("session headers not passed: %v", gotHeaders)
	}

	// thinkingLevel off => no reasoning option (pi: thinkingLevel !== "off").
	sess.SetThinkingLevel(agent.ThinkOff)
	sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 2)}, 16384)
	if gotReasoning != "" {
		t.Fatalf("reasoning must be omitted when thinking level is off, got %q", gotReasoning)
	}
}

// TestSummarizationAbortedReturnsPartialText locks the compaction part of I13:
// pi throws only on stopReason "error" (compaction.js:466); an aborted
// summarization returns the text produced so far.
func TestSummarizationAbortedReturnsPartialText(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "partial so far"}}, ai.StopAborted)),
	})
	got := sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384)
	if got != "partial so far" {
		t.Fatalf("aborted summarization should return partial text, got %q", got)
	}

	// stopReason error still fails (pi throws).
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(&ai.AssistantMessage{
			Content: ai.ContentList{ai.TextContent{Text: "junk"}}, StopReason: ai.StopError, ErrorMessage: "boom",
		}),
	})
	if got := sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 2)}, 16384); got != "" {
		t.Fatalf("error summarization must fail, got %q", got)
	}
}

// TestTruncateForSummaryUTF16 locks I13: truncation counts UTF-16 code units
// (JS .length/.slice), not bytes, and never splits a rune.
func TestTruncateForSummaryUTF16(t *testing.T) {
	// 1500 two-byte runes = 3000 bytes but only 1500 UTF-16 units: no truncation.
	in := strings.Repeat("é", 1500)
	if got := truncateForSummary(in, 2000); got != in {
		t.Fatal("byte-based truncation: 1500 UTF-16 units must not be truncated at 2000")
	}

	// 1999 ASCII + astral rune (2 units) = 2001 units: truncate 1 unit; the
	// surrogate pair on the boundary is dropped whole, never split.
	in = strings.Repeat("a", 1999) + "\U0001F648"
	got := truncateForSummary(in, 2000)
	want := strings.Repeat("a", 1999) + "\n\n[... 1 more characters truncated]"
	if got != want {
		t.Fatalf("astral boundary truncation wrong:\n got %q\nwant %q", got[1990:], want[1990:])
	}

	// Plain over-limit case keeps exactly maxChars units.
	in = strings.Repeat("b", 2500)
	got = truncateForSummary(in, 2000)
	if !strings.HasPrefix(got, strings.Repeat("b", 2000)) || !strings.HasSuffix(got, "[... 500 more characters truncated]") {
		t.Fatalf("plain truncation wrong: %q", got[1995:])
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
