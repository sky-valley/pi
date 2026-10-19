package coding

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// userText is a user message's text blocks joined, or "<non-user>" for any
// other role.
func userText(m interface{ MessageRole() ai.Role }) string {
	um, ok := m.(ai.UserMessage)
	if !ok {
		return "<non-user>"
	}
	var b strings.Builder
	for _, c := range um.Content {
		if tc, ok := c.(ai.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

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

// TestFindCutPointSnapsForward pins pi's findCutPoint (compaction.ts):
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

// TestFindCutPointFallsBackToLastCutPoint pins upstream 8bdcd4498: when the
// trailing tool results cross the keep-budget on their own, no valid cut point
// sits at or after the crossing index, and the cut falls back to the LAST cut
// point — the assistant message that issued those tool calls — rather than the
// first. Falling back to the first kept everything and summarized nothing, so a
// transcript whose tail alone overflowed the budget could never shrink (#9740).
func TestFindCutPointFallsBackToLastCutPoint(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1000 tokens
	messages := []agent.AgentMessage{
		ai.NewUserText(big, 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "t", Name: "read", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse, Timestamp: 2},
		ai.ToolResultMessage{ToolCallID: "t", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: big}}, Timestamp: 3},
	}
	// The trailing tool result alone (~1000 tokens) crosses a 500-token budget at
	// index 2; the only cut points are 0 and 1, both before it.
	cp := findCutPoint(messages, 0, len(messages), 500)
	if cp.firstKeptIndex != 1 {
		t.Fatalf("expected fallback to the last cut point (1), got %d", cp.firstKeptIndex)
	}
	if !cp.isSplitTurn || cp.turnStartIndex != 0 {
		t.Fatalf("expected split turn starting at 0, got %+v", cp)
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

// checkpointOf is the state's checkpoint, failing the test when there is none.
func checkpointOf(t *testing.T, state *compactionState) *compactionCheckpoint {
	t.Helper()
	if state.checkpoint == nil {
		t.Fatal("no compaction checkpoint")
	}
	return state.checkpoint
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
	if cp := checkpointOf(t, state); cp.prefixLen != 10 || cp.summary == "" {
		t.Fatalf("compaction state not recorded: prefixLen=%d summary=%q", cp.prefixLen, cp.summary)
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

// An empty summarization reply is a summary: pi's compact() returns "" and
// _runAutoCompaction appends it as the compaction. The checkpoint holds, so a
// later request reuses it instead of summarizing the same history again.
func TestCompactionKeepsAnEmptySummary(t *testing.T) {
	sess, reg := newCompactionTestSession(t) // window 1000: compacts past 800 tokens
	requests := 0
	emptyReply := func(ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		requests++
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: ""}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{emptyReply, emptyReply})

	// Ten 100-token messages; keeping 150 tokens cuts at the user at index 8.
	medium := strings.Repeat("m", 400)
	turn := func(i int) []agent.AgentMessage {
		return []agent.AgentMessage{
			ai.NewUserText(medium, int64(2*i)),
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: medium}}, StopReason: ai.StopStop, Timestamp: int64(2*i + 1)},
		}
	}
	var messages []agent.AgentMessage
	for i := range 5 {
		messages = append(messages, turn(i)...)
	}
	state := &compactionState{settings: CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 150}}
	emptyCheckpoint := compactionSummaryPrefix + compactionSummarySuffix

	out := sess.compact(context.Background(), state, messages)
	if cp := checkpointOf(t, state); cp.prefixLen != 8 || cp.summary != "" {
		t.Errorf("empty reply: prefixLen=%d summary=%q, want a checkpoint of \"\" keeping from 8", cp.prefixLen, cp.summary)
	}
	if len(out) != 3 || userText(out[0]) != emptyCheckpoint {
		t.Errorf("empty reply: view %v, want the empty checkpoint + messages[8:10]", roles(out))
	}

	// One more turn: the full transcript (1200 tokens) is over the threshold,
	// the compacted view (checkpoint + four kept messages) is not.
	messages = append(messages, turn(5)...)
	out = sess.compact(context.Background(), state, messages)
	if requests != 1 {
		t.Fatalf("sent %d summarization requests, want 1: the empty summary is the compaction", requests)
	}
	if len(out) != 5 || userText(out[0]) != emptyCheckpoint {
		t.Fatalf("next turn: view %v, want the empty checkpoint + messages[8:12]", roles(out))
	}
}

// A usage recorded before a compaction measured the context the compaction
// replaced. pi's _checkCompaction skips an assistant older than the latest
// compaction, and estimateProjectedContextTokens trusts only usage recorded
// after it, so the prompt after a compaction does not compact again on the
// usage that triggered it. Usage recorded after the compaction is trusted
// again. (The resumed-file case is TestResumedCompactionTriggerMatchesPi.)
func TestCompactionDistrustsUsageFromBeforeIt(t *testing.T) {
	sess, reg := newCompactionTestSession(t) // window 1000: compacts past 800 tokens
	requests := 0
	summary := func(ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		requests++
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "summary"}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{summary, summary, summary, summary})
	reply := func(text string, totalTokens int, ts int64) agent.AgentMessage {
		return ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, Usage: ai.Usage{TotalTokens: totalTokens}, StopReason: ai.StopStop, Timestamp: ts}
	}
	state := &compactionState{settings: CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 1}}
	messages := []agent.AgentMessage{ai.NewUserText("q1", 1), reply("a1", 100, 2), ai.NewUserText("q2", 3), reply("a2", 900, 4)}

	sess.compact(context.Background(), state, messages)
	if requests == 0 {
		t.Fatal("a2's usage of 900 did not compact")
	}
	compacted := requests

	messages = append(messages, ai.NewUserText("q3", 5))
	sess.compact(context.Background(), state, messages)
	if requests != compacted {
		t.Fatalf("compacted again on a2's usage, recorded before the compaction (%d more requests)", requests-compacted)
	}

	messages = append(messages, reply("a3", 900, 6), ai.NewUserText("q4", 7))
	sess.compact(context.Background(), state, messages)
	if requests == compacted {
		t.Fatal("a3's usage of 900, recorded after the compaction, did not compact")
	}
}

// pi discards an aborted compaction: _runAutoCompaction calls
// signal.throwIfAborted() after compact() returns and before appendCompaction,
// so neither the text produced so far nor an empty reply becomes a checkpoint.
// A split turn is no exception: pi's compact() goes on to the turn-prefix
// request after an aborted history summary (2 streamFn calls) and returns
// "partial\n\n---\n\n**Turn Context (split turn):**\n\n", which the session
// then drops. An AbortSignal fires for a timeout too, so a context whose
// deadline passes aborts the same way as one that is cancelled.
func TestCompactionAbortedDoesNotCheckpoint(t *testing.T) {
	historyOnly := bigTranscript(6)
	// Keeping 1 token cuts at the final assistant: q1, a1 are the history and
	// q2 the split turn's prefix.
	splitTurn := []agent.AgentMessage{
		ai.NewUserText("q1", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "a1"}}, StopReason: ai.StopStop, Timestamp: 2},
		ai.NewUserText("q2", 3),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "kept"}}, StopReason: ai.StopStop, Timestamp: 4},
	}
	if p, _ := prepareCompaction(splitTurn, 0, 1); !p.isSplitTurn || len(p.messagesToSummarize) != 2 || len(p.turnPrefixMessages) != 1 {
		t.Fatalf("setup: not a split turn with history: %+v", p)
	}
	for _, tc := range []struct {
		name      string
		messages  []agent.AgentMessage
		settings  CompactionSettings
		window    int
		text      string // what the aborted summary streamed first
		deadline  bool   // abort by an expiring deadline, not by cancel
		wantCalls int
	}{
		{"partial text", historyOnly, compactionTestSettings, 1000, "## Goal\npartial summar", false, 1},
		{"no text", historyOnly, compactionTestSettings, 1000, "", false, 1},
		{"deadline", historyOnly, compactionTestSettings, 1000, "## Goal\npartial summar", true, 1},
		{"split turn", splitTurn, CompactionSettings{Enabled: true, ReserveTokens: 2000, KeepRecentTokens: 1}, 2000, "partial", false, 2},
		{"split turn deadline", splitTurn, CompactionSettings{Enabled: true, ReserveTokens: 2000, KeepRecentTokens: 1}, 2000, "partial", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.deadline {
				var cancelDeadline context.CancelFunc
				ctx, cancelDeadline = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancelDeadline()
			}
			// The run is aborted while the first summary streams: every
			// request comes back aborted, the first with tc.text.
			calls := 0
			abortMidStream := func(streamCtx context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				calls++
				var content ai.ContentList
				if calls == 1 {
					if tc.deadline {
						<-streamCtx.Done()
					} else {
						cancel()
					}
					if tc.text != "" {
						content = ai.ContentList{ai.TextContent{Text: tc.text}}
					}
				}
				s := ai.NewAssistantMessageEventStream()
				s.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopAborted, Error: &ai.AssistantMessage{
					Api: model.Api, Provider: model.Provider, Model: model.ID,
					Content: content, StopReason: ai.StopAborted, ErrorMessage: "Request was aborted",
				}})
				s.End()
				return s
			}
			model := &ai.Model{ID: "m", Api: "faux", Provider: "faux", ContextWindow: tc.window, MaxTokens: 8192}
			sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, StreamFn: abortMidStream})

			state := &compactionState{settings: tc.settings}
			out := sess.compact(ctx, state, tc.messages)
			if calls != tc.wantCalls {
				t.Errorf("sent %d summarization requests, pi sends %d", calls, tc.wantCalls)
			}
			if state.checkpoint != nil {
				t.Fatalf("aborted compaction checkpointed %q", state.checkpoint.summary)
			}
			if len(out) != len(tc.messages) {
				t.Fatalf("aborted compaction changed the view: %d messages, want the full %d", len(out), len(tc.messages))
			}
		})
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
	firstSummary := checkpointOf(t, state).summary
	if state.checkpoint.prefixLen != 10 {
		t.Fatalf("first compaction prefixLen = %d, want 10", state.checkpoint.prefixLen)
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
		func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			captured = textOf(ai.WithoutInitialSystemMessage(req.Messages)[0].(ai.UserMessage).Content)
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nupdated summary"}}, ai.StopStop)
		},
	})

	out := sess.compact(context.Background(), state, messages)

	// Second compaction covers a larger prefix (cut at the user at index 14).
	if state.checkpoint.prefixLen != 14 {
		t.Fatalf("second compaction prefixLen = %d, want 14", state.checkpoint.prefixLen)
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
	if !strings.Contains(state.checkpoint.summary, "<read-files>\n/a/seen.go\n</read-files>") {
		t.Fatalf("file ops did not carry over:\n%s", state.checkpoint.summary)
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

// TestCompactionSplitTurnSummaries locks the TURN_PREFIX flow (pi compact): a
// cut landing mid-turn produces a history summary (0.8*reserve budget) and a
// turn-prefix summary (TURN_PREFIX prompt, 0.5*reserve budget) merged with pi's
// exact separator.
func TestCompactionSplitTurnSummaries(t *testing.T) {
	sess, reg := newCompactionTestSession(t)

	type call struct {
		text      string
		maxTokens int
	}
	var calls []call
	step := func(text string) providers.FauxResponseStep {
		return func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			c := call{text: textOf(ai.WithoutInitialSystemMessage(req.Messages)[0].(ai.UserMessage).Content)}
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

	// Merged summary format (pi compact).
	if !strings.Contains(checkpointText(t, out[0]), "HIST\n\n---\n\n**Turn Context (split turn):**\n\nPREFIX") {
		t.Fatalf("merged split-turn summary missing:\n%s", checkpointText(t, out[0]))
	}
	if cp := checkpointOf(t, state); cp.prefixLen != 11 {
		t.Fatalf("split-turn prefixLen = %d, want 11", cp.prefixLen)
	}
	if len(out) != 2 { // checkpoint + kept assistant
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
}

// pi 58302d34e (compaction routing sessions): summarization requests reuse a
// caller-supplied routing session ID instead of minting a fresh uuidv7 per
// request, while cache retention stays "none" — the routing ID is forwarded
// without enabling prompt caching. Callers without one (including branch
// summaries) keep fresh per-request ids (pi `options.sessionId ?? uuidv7()`).
func TestCompactionRoutingSessionIDForwarded(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	var sessionIDs []string
	var retentions []ai.CacheRetention
	capture := func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
		sessionIDs = append(sessionIDs, opts.SessionID)
		retentions = append(retentions, opts.CacheRetention)
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nsummary"}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{capture, capture})

	settings := compactionTestSettings
	settings.SessionID = "routing-session-1"
	state := &compactionState{settings: settings}
	sess.compact(context.Background(), state, bigTranscript(6))

	if len(sessionIDs) != 1 || sessionIDs[0] != "routing-session-1" {
		t.Fatalf("summarization sessionIDs = %v, want exactly the caller routing id", sessionIDs)
	}
	if retentions[0] != ai.CacheNone {
		t.Fatalf("summarization cacheRetention = %v, want none (a routing id must not enable caching)", retentions[0])
	}

	// Without a caller id, each request mints a fresh one.
	sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384)
	if len(sessionIDs) != 2 || sessionIDs[1] == "" || sessionIDs[1] == "routing-session-1" {
		t.Fatalf("callers without a session id must get a fresh one, got %v", sessionIDs)
	}
}

// TestSummaryTextBlocksJoinedWithNewline locks I3: assistant text blocks of the
// summarization response join with "\n" (pi-ai contentText, as pi compact
// reads the response).
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
// messages are skipped when picking the usage anchor (pi getAssistantUsage).
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

// TestUsageEstimateSkipsAllZeroUsage locks upstream cd95c274's calculateContextTokens
// guard: a successful (non-aborted/non-error) assistant message whose usage is
// all-zero (malformed/unreported usage) must NOT anchor the estimate. The earlier
// valid usage stays the anchor; if none exists, the pure heuristic is used.
func TestUsageEstimateSkipsAllZeroUsage(t *testing.T) {
	valid := ai.AssistantMessage{
		Content:    ai.ContentList{ai.TextContent{Text: "ok"}},
		Usage:      ai.Usage{TotalTokens: 50000},
		StopReason: ai.StopStop, Timestamp: 1,
	}
	// Successful turn but the provider reported all-zero usage (malformed).
	zeroUsage := ai.AssistantMessage{
		Content:    ai.ContentList{ai.TextContent{Text: "huge response with no usage"}},
		Usage:      ai.Usage{}, // all zero
		StopReason: ai.StopStop, Timestamp: 2,
	}

	// The all-zero turn must not override the earlier valid 50000 anchor.
	got := estimateContextTokensUsageAware([]agent.AgentMessage{valid, zeroUsage})
	if got < 50000 {
		t.Fatalf("all-zero usage overrode the valid anchor; expected >= 50000, got %d", got)
	}

	// Only all-zero usage present => pure heuristic, never an anchor of 0.
	got = estimateContextTokensUsageAware([]agent.AgentMessage{ai.NewUserText("short", 1), zeroUsage})
	pure := EstimateContextTokens([]agent.AgentMessage{ai.NewUserText("short", 1), zeroUsage})
	if got != pure {
		t.Fatalf("all-zero usage must fall back to the pure heuristic (%d), got %d", pure, got)
	}
}

// TestSummarizationPassesReasoningAndHeaders locks I7: the summarization request
// carries the session's headers and — for reasoning models with a non-off
// thinking level — the thinking level (pi createSummarizationOptions).
func TestSummarizationPassesReasoningAndHeaders(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-r", Reasoning: true, ContextWindow: 200000}},
	})
	defer reg.Unregister()

	var gotReasoning ai.ThinkingLevel
	var gotHeaders ai.ProviderHeaders
	capture := func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
		gotReasoning = opts.Reasoning
		gotHeaders = opts.Headers
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{capture, capture})

	sess := NewSession(SessionOptions{
		Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
		Headers: ai.ProviderHeaders{"X-Parity": ai.HeaderValue("1")},
	})

	sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384)
	if gotReasoning != ai.ThinkingMedium { // session default thinking level
		t.Fatalf("reasoning level not passed: %q", gotReasoning)
	}
	if gotHeaders["X-Parity"] == nil || *gotHeaders["X-Parity"] != "1" {
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
// an abort is not a summarization failure in pi, so it returns the text produced
// so far, while stopReason "error" fails.
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

// TestSummarizationLengthStopRejected locks upstream 97fa14e39 (#7048): a
// summarization that stopped on "length" holds a truncated summary, so it is a
// failure and must never become a session checkpoint — unlike an aborted one,
// whose text the summarizer still returns.
func TestSummarizationLengthStopRejected(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	lengthStopped := func() {
		reg.SetResponses([]providers.FauxResponseStep{
			providers.FauxStatic(providers.FauxAssistantMessage(
				ai.ContentList{ai.TextContent{Text: "## Goal\npartial summar"}}, ai.StopLength)),
		})
	}

	lengthStopped()
	if got := sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)}, 16384); got != "" {
		t.Fatalf("length-stopped summarization must fail, got %q", got)
	}

	// ...and it leaves no checkpoint behind: the view stays the full transcript
	// and nothing is cached for later requests.
	lengthStopped()
	messages := bigTranscript(6)
	state := &compactionState{settings: compactionTestSettings}
	got := sess.compact(context.Background(), state, messages)
	if len(got) != len(messages) {
		t.Fatalf("length-stopped summarization must not compact: got %d messages, want the full %d", len(got), len(messages))
	}
	if state.checkpoint != nil {
		t.Fatalf("truncated summary must not be checkpointed, got %q", state.checkpoint.summary)
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

// pi renders a tool call for the summarizer with Object.entries, i.e. in the
// key order the model authored (compaction/utils.ts). The summary prompt is
// model-visible, so a Go map's ordering would change what the summarizer reads.
func TestSerializeAssistantKeepsArgumentOrder(t *testing.T) {
	raw := `{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"list_dir",` +
		`"arguments":{"path":"/tmp","depth":1,"filter":{"z":true,"a":false}}}],` +
		`"api":"openai-completions","provider":"openai","model":"m","stopReason":"toolUse","timestamp":1}`
	msg, err := ai.UnmarshalMessage([]byte(raw))
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}
	got := serializeConversation([]ai.Message{msg})
	want := `[Assistant tool calls]: list_dir(path="/tmp", depth=1, filter={"z":true,"a":false})`
	if !strings.Contains(got, want) {
		t.Fatalf("serialized tool call lost argument order:\n got %s\nwant %s", got, want)
	}
}

// pi serializes tool-call arguments with JSON.stringify, which writes &, < and
// > as themselves; the summary prompt must not carry json.Marshal's \u0026.
func TestSerializeAssistantWritesArgumentsAsJSONStringify(t *testing.T) {
	raw := `{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"bash",` +
		`"arguments":{"command":"make && ./run <in >out"}}],` +
		`"api":"openai-completions","provider":"openai","model":"m","stopReason":"toolUse","timestamp":1}`
	msg, err := ai.UnmarshalMessage([]byte(raw))
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}
	got := serializeConversation([]ai.Message{msg})
	if want := `bash(command="make && ./run <in >out")`; !strings.Contains(got, want) {
		t.Fatalf("serialized tool call:\n got %s\nwant %s", got, want)
	}
}
