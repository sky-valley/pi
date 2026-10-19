package coding

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// Compaction and the transcript's system messages (upstream 9e05370b2). pi
// stores the replayed system message on the compaction entry and rebuilds
// [systemMessage, summary, kept non-system entries, later entries]; the port
// compacts in memory, so the same view is rebuilt per request.

func toolNames(tools []ai.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func systemCount(messages []ai.Message) int {
	n := 0
	for _, m := range messages {
		if m.MessageRole() == ai.RoleSystem {
			n++
		}
	}
	return n
}

// A compacted request still carries the prompt and the tool declarations: the
// system message that declared them was summarized away with the rest of the
// prefix, so the compacted view leads with its replay. The summarization request
// itself sees no prompt text.
func TestCompactionKeepsPromptAndToolsOnTheNextRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 1000}},
	})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	sess.EnableCompaction(compactionTestSettings)

	var requests []ai.TranscriptContext
	respond := func(text string) providers.FauxResponseStep {
		return func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			requests = append(requests, req)
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: text}}, ai.StopStop)
		}
	}
	reg.SetResponses([]providers.FauxResponseStep{respond("first")})
	if _, err := sess.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	declared, ok := ai.GetInitialSystemMessage(requests[0].Messages)
	if !ok || !strings.Contains(ai.GetSystemMessageText(declared), "expert coding assistant") {
		t.Fatalf("first request carries no coding prompt: %+v", requests[0].Messages)
	}
	declaredTools := toolNames(ai.GetCurrentTools(requests[0].Messages))

	sess.LoadHistory(append(sess.History(), bigTranscript(6)...))
	requests = nil
	reg.SetResponses([]providers.FauxResponseStep{respond("## Goal\nsummary"), respond("second")})
	if _, err := sess.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("expected a summarization request and the turn's request, got %d", len(requests))
	}

	summarization, compacted := requests[0], requests[1]
	if lead, ok := ai.GetInitialSystemMessage(summarization.Messages); !ok || ai.GetSystemMessageText(lead) != summarizationSystemPrompt {
		t.Fatalf("summarization request lost its own prompt: %+v", summarization.Messages)
	}
	for _, m := range ai.WithoutInitialSystemMessage(summarization.Messages) {
		if strings.Contains(userText(m), "expert coding assistant") || m.MessageRole() == ai.RoleSystem {
			t.Fatalf("summarization request carries the coding prompt: %q", userText(m))
		}
	}

	lead, ok := ai.GetInitialSystemMessage(compacted.Messages)
	if !ok {
		t.Fatalf("compacted request has no leading system message: roles %v", roles(compacted.Messages))
	}
	if got, want := ai.GetSystemMessageText(lead), ai.GetSystemMessageText(declared); got != want {
		t.Fatalf("compacted prompt drifted:\n--- got ---\n%s\n--- declared ---\n%s", got, want)
	}
	if got := toolNames(ai.GetCurrentTools(compacted.Messages)); !reflect.DeepEqual(got, declaredTools) || len(got) != 4 {
		t.Fatalf("compacted request declares tools %v, want %v", got, declaredTools)
	}
	if n := systemCount(compacted.Messages); n != 1 {
		t.Fatalf("compacted request carries %d system messages, want 1", n)
	}
	if !strings.HasPrefix(userText(compacted.Messages[1]), compactionSummaryPrefix) {
		t.Fatalf("the summary must follow the system message: roles %v", roles(compacted.Messages))
	}
}

func roles(messages []ai.Message) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, string(m.MessageRole()))
	}
	return out
}

func sectionText(value string) *string { return &value }

// The compacted view rebuilds pi's buildSessionContext order: the system
// message replayed at compaction time, the summary, the kept messages without
// their system messages (the replay already holds them), then everything after
// the compaction with its system messages.
func TestCompactionViewSkipsKeptSystemMessagesAndKeepsLaterOnes(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nsummary"}}, ai.StopStop)),
	})

	big := strings.Repeat("y", 4000)
	readTool := ai.Tool{Name: "read", Description: "Read a file"}
	leading := ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("A")}}, ToolsAdded: []ai.Tool{readTool}, Timestamp: 1}
	kept := ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("B")}}, Timestamp: 7}
	later := ai.SystemMessage{Content: ai.ContentList{ai.TextContent{Text: "later instructions"}}, ToolsRemoved: []ai.ToolReference{{Name: "read"}}, Timestamp: 10}
	messages := []agent.AgentMessage{
		leading,
		ai.NewUserText(big, 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 2},
		ai.NewUserText(big, 3),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 4},
		ai.NewUserText(big, 5), // 5: first kept
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 6}, // 6
		kept, // 7: kept, but folded into the replay
		ai.NewUserText("small", 8),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "small"}}, StopReason: ai.StopStop, Timestamp: 9},
	}
	state := &compactionState{settings: compactionTestSettings}
	sess.compact(context.Background(), state, messages)
	if cp := checkpointOf(t, state); cp.prefixLen != 5 {
		t.Fatalf("prefixLen = %d, want 5", cp.prefixLen)
	}

	messages = append(messages, later, ai.NewUserText("after", 11))
	out := sess.compact(context.Background(), state, messages)
	if reg.PendingResponseCount() != 0 {
		t.Fatal("the second request must reuse the compaction")
	}

	wantRoles := []string{"system", "user", "user", "assistant", "user", "assistant", "system", "user"}
	if got := roles(out); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("compacted view roles = %v, want %v", got, wantRoles)
	}
	replay, _ := out[0].(ai.SystemMessage)
	if got := ai.GetSystemMessageText(replay); got != "B" {
		t.Fatalf("replayed prompt = %q, want the prompt current at compaction (B)", got)
	}
	if got := toolNames(replay.ToolsAdded); !reflect.DeepEqual(got, []string{"read"}) {
		t.Fatalf("replayed tools = %v, want [read]", got)
	}
	if !strings.HasPrefix(userText(out[1]), compactionSummaryPrefix) {
		t.Fatalf("summary must follow the replay, got %q", userText(out[1]))
	}
	if got, ok := out[6].(ai.SystemMessage); !ok || ai.ContentText(got.Content) != "later instructions" {
		t.Fatalf("a system message after the compaction must stay in place, got %#v", out[6])
	}
	if got := toolNames(ai.GetCurrentTools(out)); len(got) != 0 {
		t.Fatalf("the later removal must still apply, current tools %v", got)
	}
}

// compaction.test.ts 'prepareCompaction does not treat system messages as
// conversation history': the system message is neither summarized nor a
// history of its own, so a split turn summarizes only its prefix and the
// history half is "No prior history.".
func TestCompactionDoesNotSummarizeSystemMessages(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	var prompts []string
	step := func(text string) providers.FauxResponseStep {
		return func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			prompts = append(prompts, userText(ai.WithoutInitialSystemMessage(req.Messages)[0]))
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: text}}, ai.StopStop)
		}
	}
	reg.SetResponses([]providers.FauxResponseStep{step("PREFIX"), step("UNEXPECTED")})

	system := ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("current prompt")}}, Timestamp: 1}
	messages := []agent.AgentMessage{
		system,
		ai.NewUserText("one long turn", 2),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "assistant suffix"}}, StopReason: ai.StopStop, Timestamp: 3},
	}
	state := &compactionState{settings: CompactionSettings{Enabled: true, ReserveTokens: 995, KeepRecentTokens: 1}}
	out := sess.compact(context.Background(), state, messages)

	if len(prompts) != 1 {
		t.Fatalf("expected only the turn-prefix summarization, got %d requests:\n%s", len(prompts), strings.Join(prompts, "\n=====\n"))
	}
	// The prefix sits under a Markdown conversation boundary, as in
	// agent-session-compaction.test.ts "uses the standalone compaction request
	// context" (pi #9652, upstream d192bd6dc).
	if want := "# Conversation\n[User]: one long turn\n\n# Instructions\n" + turnPrefixSummarizationPrompt; prompts[0] != want {
		t.Fatalf("turn-prefix request:\n--- got ---\n%s\n--- want ---\n%s", prompts[0], want)
	}
	if cp := checkpointOf(t, state); cp.prefixLen != 2 {
		t.Fatalf("first kept index = %d, want the assistant (2)", cp.prefixLen)
	}
	if want := "No prior history.\n\n---\n\n**Turn Context (split turn):**\n\nPREFIX"; !strings.HasPrefix(state.checkpoint.summary, want) {
		t.Fatalf("summary = %q, want prefix %q", state.checkpoint.summary, want)
	}
	if got := roles(out); !reflect.DeepEqual(got, []string{"system", "user", "assistant"}) {
		t.Fatalf("compacted view roles = %v", got)
	}
}

// The same fixture through prepareCompaction, asserting what upstream's test
// asserts.
func TestPrepareCompactionSkipsSystemMessages(t *testing.T) {
	system := ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("current prompt")}}, Timestamp: 1}
	user := ai.NewUserText("one long turn", 2)
	assistant := ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "assistant suffix"}}, StopReason: ai.StopStop, Timestamp: 3}

	preparation, ok := prepareCompaction([]agent.AgentMessage{system, user, assistant}, 0, 1)
	if !ok {
		t.Fatal("expected a preparation")
	}
	if preparation.firstKeptIndex != 2 || !preparation.isSplitTurn {
		t.Fatalf("firstKeptIndex = %d, isSplitTurn = %v; want 2, true", preparation.firstKeptIndex, preparation.isSplitTurn)
	}
	if len(preparation.messagesToSummarize) != 0 {
		t.Fatalf("messagesToSummarize = %v, want none", preparation.messagesToSummarize)
	}
	if !reflect.DeepEqual(preparation.turnPrefixMessages, []agent.AgentMessage{user}) {
		t.Fatalf("turnPrefixMessages = %v, want [user]", preparation.turnPrefixMessages)
	}
}

// testdata/compaction/capture.mts runs pi's prepareCompaction at 9e05370b2
// over paths carrying system messages — inside a split turn's prefix, among the
// history, and as the only history — and records the cut and both lists as
// indexes into the path, with the text each list serializes to. Neither list
// ever carries a system message.
func TestPrepareCompactionMatchesPiCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/compaction/prepare-9e05370b2.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Scenarios []struct {
			Name             string            `json:"name"`
			KeepRecentTokens int               `json:"keepRecentTokens"`
			Messages         []json.RawMessage `json:"messages"`
			Preparation      *struct {
				FirstKeptIndex      int    `json:"firstKeptIndex"`
				IsSplitTurn         bool   `json:"isSplitTurn"`
				MessagesToSummarize []int  `json:"messagesToSummarize"`
				TurnPrefixMessages  []int  `json:"turnPrefixMessages"`
				HistoryText         string `json:"historyText"`
				TurnPrefixText      string `json:"turnPrefixText"`
			} `json:"preparation"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Scenarios) == 0 {
		t.Fatal("the capture holds no scenarios")
	}
	for _, scenario := range capture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			messages := make([]agent.AgentMessage, 0, len(scenario.Messages))
			for _, raw := range scenario.Messages {
				message, err := ai.UnmarshalMessage(raw)
				if err != nil {
					t.Fatal(err)
				}
				messages = append(messages, message)
			}
			indexes := func(list []agent.AgentMessage) []int {
				out := []int{}
				for _, m := range list {
					index := -1
					for i, candidate := range messages {
						if reflect.DeepEqual(m, candidate) {
							index = i
							break
						}
					}
					out = append(out, index)
				}
				return out
			}

			preparation, ok := prepareCompaction(messages, 0, scenario.KeepRecentTokens)
			want := scenario.Preparation
			if ok != (want != nil) {
				t.Fatalf("prepared = %v, pi prepared = %v", ok, want != nil)
			}
			if want == nil {
				return
			}
			if preparation.firstKeptIndex != want.FirstKeptIndex || preparation.isSplitTurn != want.IsSplitTurn {
				t.Fatalf("cut = (%d, split %v), pi (%d, split %v)", preparation.firstKeptIndex, preparation.isSplitTurn, want.FirstKeptIndex, want.IsSplitTurn)
			}
			if got := indexes(preparation.messagesToSummarize); !reflect.DeepEqual(got, want.MessagesToSummarize) {
				t.Fatalf("messagesToSummarize = %v, pi %v", got, want.MessagesToSummarize)
			}
			if got := indexes(preparation.turnPrefixMessages); !reflect.DeepEqual(got, want.TurnPrefixMessages) {
				t.Fatalf("turnPrefixMessages = %v, pi %v", got, want.TurnPrefixMessages)
			}
			if got := serializeConversation(messagesAsLlm(preparation.messagesToSummarize)); got != want.HistoryText {
				t.Fatalf("history text drift from pi.\n--- got ---\n%s\n--- pi ---\n%s", got, want.HistoryText)
			}
			if got := serializeConversation(messagesAsLlm(preparation.turnPrefixMessages)); got != want.TurnPrefixText {
				t.Fatalf("turn prefix text drift from pi.\n--- got ---\n%s\n--- pi ---\n%s", got, want.TurnPrefixText)
			}
		})
	}
}

// A transcript whose only summarizable history is a system message has nothing
// to compact (pi prepareCompaction returns undefined): no summarization request
// and the view is unchanged.
func TestCompactionWithOnlySystemHistoryDoesNothing(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	called := false
	reg.SetResponses([]providers.FauxResponseStep{
		func(ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
			called = true
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "summary"}}, ai.StopStop)
		},
	})
	big := strings.Repeat("y", 4000)
	messages := []agent.AgentMessage{
		ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("prompt")}}, Timestamp: 1},
		ai.NewUserText(big, 2),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 3},
	}
	state := &compactionState{settings: compactionTestSettings}
	out := sess.compact(context.Background(), state, messages)
	if called {
		t.Fatal("a system message alone must not be summarized")
	}
	if len(out) != len(messages) || state.checkpoint != nil {
		t.Fatalf("view changed: %v (checkpoint %+v)", roles(out), state.checkpoint)
	}
}

// A system message is not a cut point (pi isCutPointMessage): a budget crossing
// on the tool result before it snaps past it to the user message, so the turn
// is not split.
func TestFindCutPointSkipsSystemMessages(t *testing.T) {
	big := strings.Repeat("x", 4000)
	messages := []agent.AgentMessage{
		ai.NewUserText(big, 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "t", Name: "read", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse, Timestamp: 2},
		ai.ToolResultMessage{ToolCallID: "t", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: big}}, Timestamp: 3},
		ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("patched")}}, Timestamp: 4},
		ai.NewUserText(big, 5),
	}
	cp := findCutPoint(messages, 0, len(messages), 1500)
	if cp.firstKeptIndex != 4 || cp.isSplitTurn {
		t.Fatalf("cut = %+v, want the user message at 4 without a split", cp)
	}
}

// Entries estimated at zero tokens do not count toward the keep budget (pi
// findCutPoint `if (messageTokens === 0) continue`): with nothing to keep, the
// walk passes a trailing system message and cuts at the assistant before it.
func TestFindCutPointPassesZeroTokenMessages(t *testing.T) {
	messages := []agent.AgentMessage{
		ai.NewUserText("a", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "b"}}, StopReason: ai.StopStop, Timestamp: 2},
		ai.SystemMessage{Sections: ai.SystemSections{{Name: "preamble", Value: sectionText("prompt")}}, Timestamp: 3},
	}
	cp := findCutPoint(messages, 0, len(messages), 0)
	if cp.firstKeptIndex != 1 || !cp.isSplitTurn || cp.turnStartIndex != 0 {
		t.Fatalf("cut = %+v, want the assistant at 1 splitting the turn started at 0", cp)
	}
}

// A split turn with no new history keeps the previous summary as its history
// half (pi compact: `previousSummary ?? "No prior history."`) —
// compaction-summary-reasoning.test.ts "preserves the previous summary without
// an empty history request for a split turn".
func TestCompactionSplitTurnSeedsHistoryWithPreviousSummary(t *testing.T) {
	sess, reg := newCompactionTestSession(t)
	var prompts []string
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			prompts = append(prompts, userText(ai.WithoutInitialSystemMessage(req.Messages)[0]))
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "PREFIX"}}, ai.StopStop)
		},
	})
	big := strings.Repeat("y", 4000)
	messages := append(bigTranscript(6),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: 12})
	state := &compactionState{
		settings:   CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 400},
		checkpoint: &compactionCheckpoint{prefixLen: 10, compactedLen: 10, summary: "PREV SUMMARY"},
	}
	sess.compact(context.Background(), state, messages)

	if len(prompts) != 1 || !strings.HasSuffix(prompts[0], turnPrefixSummarizationPrompt) {
		t.Fatalf("expected one turn-prefix request, got %d", len(prompts))
	}
	// Regression test for pi #9652 (upstream d192bd6dc): clear boundaries and
	// continuation wording avoid the reasoning-extraction false positive.
	if !strings.Contains(prompts[0], "# Conversation\n[User]: "+big) {
		t.Fatalf("turn-prefix request lacks the Markdown conversation boundary:\n%.300s", prompts[0])
	}
	if !strings.Contains(prompts[0], "# Instructions\nThe messages above are earlier context from an ongoing conversation.") {
		t.Fatalf("turn-prefix request lacks the continuation instructions:\n%.300s", prompts[0])
	}
	if want := "PREV SUMMARY\n\n---\n\n**Turn Context (split turn):**\n\nPREFIX"; !strings.HasPrefix(checkpointOf(t, state).summary, want) {
		t.Fatalf("summary = %q, want prefix %q", state.checkpoint.summary, want)
	}
}

// agent-session-compaction.test.ts 'checkpoints the replayed system state and
// folds summarized and retained system patches into it', over the port's
// in-memory compaction: the replay holds the declared prompt plus both patches'
// content, sections and tool removals, and neither patch stays in the view.
func TestCompactionCheckpointFoldsSummarizedAndRetainedSystemPatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 1000}},
	})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	reg.SetResponses([]providers.FauxResponseStep{fauxText("declared")})
	if _, err := sess.Run(context.Background(), "declare the prompt"); err != nil {
		t.Fatal(err)
	}
	declared, ok := sess.History()[0].(ai.SystemMessage)
	if !ok {
		t.Fatalf("expected declared system message, got %T", sess.History()[0])
	}

	messages := append(sess.History(),
		ai.SystemMessage{
			Content:      ai.ContentList{ai.TextContent{Text: "summarized instruction"}},
			Sections:     ai.SystemSections{{Name: "early", Value: sectionText("<early>1</early>")}},
			ToolsRemoved: []ai.ToolReference{{Name: "bash"}},
			Timestamp:    nowMillisCoding(),
		},
		ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "kept before patch"}}, Timestamp: nowMillisCoding()},
		ai.SystemMessage{
			Content:      ai.ContentList{ai.TextContent{Text: "retained instruction"}},
			Sections:     ai.SystemSections{{Name: "extra", Value: sectionText("<extra>late</extra>")}},
			ToolsRemoved: []ai.ToolReference{{Name: "read"}},
			Timestamp:    nowMillisCoding(),
		},
		ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "kept after patch"}}, Timestamp: nowMillisCoding()},
	)
	reg.SetResponses([]providers.FauxResponseStep{fauxText("compacted")})
	// Keep the last two user messages and the retained patch between them (5 +
	// 10 + 4 estimated tokens; a system message counts its content and sections
	// since upstream 466db0fec), so the cut lands on "kept before patch" as
	// upstream's firstKeptEntryId does.
	state := &compactionState{settings: CompactionSettings{Enabled: true, ReserveTokens: 999, KeepRecentTokens: 19}}
	out := sess.compact(context.Background(), state, messages)

	if got, want := roles(out), []string{"system", "user", "user", "user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if got := userText(out[2]); got != "kept before patch" {
		t.Fatalf("first kept = %q", got)
	}
	checkpoint := out[0].(ai.SystemMessage)
	if content, _ := checkpoint.StringContent(); content != "summarized instruction\n\nretained instruction" {
		t.Fatalf("checkpoint content = %q", content)
	}
	want := append(ai.SystemSections{}, declared.Sections.Entries()...)
	want.Set("early", sectionText("<early>1</early>"))
	want.Set("extra", sectionText("<extra>late</extra>"))
	if !reflect.DeepEqual(checkpoint.Sections.Entries(), want.Entries()) {
		t.Fatalf("checkpoint sections:\n%s\nwant:\n%s", formatSections(checkpoint.Sections.Entries()), formatSections(want.Entries()))
	}
	if got := toolNames(checkpoint.ToolsAdded); !reflect.DeepEqual(got, []string{"edit", "write"}) {
		t.Fatalf("checkpoint tools = %v, want the active tools without read and bash", got)
	}
}
