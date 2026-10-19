package ai

import (
	"strings"
	"testing"
)

// Hand-computed against pi packages/ai/src/utils/estimate.ts (upstream 09f10595,
// 27075fe07) and checked against a src run at d2e7d8b7c: CHARS_PER_TOKEN=3.5,
// ESTIMATED_IMAGE_CHARS=4800; estimateTextTokens=ceil(len/3.5) over the JS
// UTF-16 .length.

func TestJSStringLength(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"héllo", 5},      // é is one BMP code unit
		{"a😀b", 4},        // emoji is a surrogate pair -> 2 code units
		{"\U0001F600", 2}, // astral char -> 2 code units
	}
	for _, c := range cases {
		if got := jsStringLength(c.s); got != c.want {
			t.Errorf("jsStringLength(%q) = %d, want %d", c.s, got, c.want)
		}
	}
	// Invalid UTF-8 reads as U+FFFD per byte, one unit each, as a JSON
	// decoder hands pi the same bytes.
	if got := jsStringLength("a\xffb\xed\xa0\x80"); got != 6 {
		t.Errorf("jsStringLength of invalid UTF-8 = %d, want 6", got)
	}
	// Request telemetry measures every completed block with it: counting must
	// not copy the text.
	long := strings.Repeat("a😀b", 1_000)
	if allocs := testing.AllocsPerRun(10, func() { jsStringLength(long) }); allocs != 0 {
		t.Errorf("jsStringLength allocates %v times per call, want none", allocs)
	}
}

func TestEstimateContextTokensEmpty(t *testing.T) {
	est := estimateContextTokens(NormalizeContext(Context{}).Messages)
	if est.Tokens != 0 || est.UsageTokens != 0 || est.TrailingTokens != 0 || est.LastUsageIndex != -1 {
		t.Fatalf("empty context = %+v, want all-zero with LastUsageIndex -1", est)
	}
}

func TestEstimateContextTokensUserText(t *testing.T) {
	// "hello" -> UTF-16 length 5 -> ceil(5/3.5) = 2 tokens. No usage anchor, no
	// system prompt, no tools.
	ctx := NormalizeContext(Context{Messages: []Message{NewUserText("hello", 1)}})
	est := estimateContextTokens(ctx.Messages)
	if est.Tokens != 2 || est.LastUsageIndex != -1 {
		t.Fatalf("user text = %+v, want Tokens 2, LastUsageIndex -1", est)
	}
}

func TestEstimateContextTokensSystemPromptAndTools(t *testing.T) {
	// systemPrompt "abcd" (4) -> ceil(4/3.5)=2. user "hi" (2) -> ceil(2/3.5)=1.
	// The leading system message adds estimateTextTokens(JSON.stringify(tools)).
	tool := Tool{Name: "t", Description: "d"}
	tools := []Tool{tool}
	ctx := NormalizeContext(Context{
		SystemPrompt: "abcd",
		Messages:     []Message{NewUserText("hi", 1)},
		Tools:        tools,
	})
	toolsJSON := safeJSONStringify(tools)
	wantPrefix := 2 /*system*/ + estimateTextTokens(toolsJSON)
	est := estimateContextTokens(ctx.Messages)
	if want := 1 /*user*/ + wantPrefix; est.Tokens != want {
		t.Fatalf("system+tools Tokens = %d, want %d (toolsJSON=%q)", est.Tokens, want, toolsJSON)
	}
	if est.TrailingTokens != 1+wantPrefix {
		t.Fatalf("TrailingTokens = %d, want %d", est.TrailingTokens, 1+wantPrefix)
	}
}

func TestEstimateMessageTokensImage(t *testing.T) {
	// One image block -> ESTIMATED_IMAGE_CHARS 4800 -> ceil(4800/3.5) = 1372.
	msg := UserMessage{Content: ContentList{ImageContent{Data: "x", MimeType: "image/png"}}}
	if got := estimateMessageTokens(msg); got != 1372 {
		t.Fatalf("image message tokens = %d, want 1372", got)
	}
}

func TestEstimateMessageTokensAssistantToolCall(t *testing.T) {
	// Assistant tool call: chars = name.length + JSON.stringify(arguments).length.
	tc := ToolCall{Name: "run", Arguments: map[string]any{"a": 1}}
	argsJSON := safeJSONStringify(tc.Arguments) // {"a":1} -> 7
	msg := AssistantMessage{Content: ContentList{tc}, StopReason: StopToolUse}
	// ceil((3 + 7) / 3.5) = 3.
	want := 3
	if got := estimateMessageTokens(msg); got != want {
		t.Fatalf("assistant tool call tokens = %d, want %d (argsJSON=%q)", got, want, argsJSON)
	}
}

func TestEstimateContextTokensUsageAnchor(t *testing.T) {
	// With a usage anchor, tokens = usageTokens + sum(messages after anchor).
	// The anchor's totalTokens is used; the leading system message (prompt and
	// tools) sits before the anchor, so it is not added once an anchor exists.
	assistant := AssistantMessage{
		Content:    ContentList{TextContent{Text: "ok"}},
		StopReason: StopStop,
		Usage:      Usage{TotalTokens: 1000},
		Timestamp:  2, // after the preceding user (ts 1), so its usage anchors the prefix
	}
	trailing := NewUserText(strings.Repeat("x", 8), 3) // 8 chars -> ceil(8/3.5)=3
	ctx := NormalizeContext(Context{
		SystemPrompt: "ignored-because-anchor",
		Tools:        []Tool{{Name: "t"}},
		Messages:     []Message{NewUserText("hi", 1), assistant, trailing},
	})
	est := estimateContextTokens(ctx.Messages)
	if est.UsageTokens != 1000 {
		t.Fatalf("UsageTokens = %d, want 1000", est.UsageTokens)
	}
	if est.TrailingTokens != 3 {
		t.Fatalf("TrailingTokens = %d, want 3", est.TrailingTokens)
	}
	if est.Tokens != 1003 {
		t.Fatalf("Tokens = %d, want 1003 (no prefix added when anchored)", est.Tokens)
	}
	// The index is into the normalized transcript, which leads with the system
	// message: the anchoring assistant is its third message.
	if est.LastUsageIndex != 2 {
		t.Fatalf("LastUsageIndex = %d, want 2", est.LastUsageIndex)
	}
}

func TestGetLastAssistantUsageInfoSkipsAbortedAndError(t *testing.T) {
	// Walk backwards; skip aborted/error assistants and zero-usage assistants.
	good := AssistantMessage{StopReason: StopStop, Usage: Usage{Input: 5, Output: 5}} // 10
	aborted := AssistantMessage{StopReason: StopAborted, Usage: Usage{TotalTokens: 999}}
	errored := AssistantMessage{StopReason: StopError, Usage: Usage{TotalTokens: 999}}
	est := estimateContextTokens([]Message{good, aborted, errored})
	if est.UsageTokens != 10 || est.LastUsageIndex != 0 {
		t.Fatalf("est = %+v, want UsageTokens 10 at index 0 (aborted/error skipped)", est)
	}
}

func estimateTestAssistant(timestamp int64, totalTokens int) AssistantMessage {
	return AssistantMessage{
		Content:    ContentList{TextContent{Text: "kept"}},
		StopReason: StopStop,
		Usage:      Usage{Input: totalTokens, TotalTokens: totalTokens},
		Timestamp:  timestamp,
	}
}

func TestEstimateIgnoresStaleUsageAfterNewerInsertedMessage(t *testing.T) {
	// Mirrors pi context-estimate.test.ts (upstream 8973ae28): the assistant's
	// usage (ts 100) is stale because a newer prefix message (the summary user
	// message, ts 200) was inserted before it, so it cannot describe the current
	// prefix. Falls back to a full estimate with no anchor. Wrapped in
	// NormalizeContext as upstream 9e05370b2 wraps it.
	ctx := NormalizeContext(Context{
		SystemPrompt: "system",
		Messages: []Message{
			NewUserText("summary", 200),
			estimateTestAssistant(100, 9_500),
			NewUserText(strings.Repeat("x", 4_000), 300),
		},
	})
	est := estimateContextTokens(ctx.Messages)
	if est.Tokens != 1_149 || est.UsageTokens != 0 || est.TrailingTokens != 1_149 || est.LastUsageIndex != -1 {
		t.Fatalf("stale-usage estimate = %+v, want {Tokens:1149 UsageTokens:0 TrailingTokens:1149 LastUsageIndex:-1}", est)
	}
	// buildBaseOptions(model, context).maxTokens: contextWindow 10000, maxTokens
	// 8000 -> min(8000, 10000 - 1149 - 4096) = 4755 (upstream 27075fe07).
	model := &Model{ContextWindow: 10_000, MaxTokens: 8_000}
	if got := ClampMaxTokensToContext(model, ctx, SimpleMaxTokensDefault(model, nil)); got != 4_755 {
		t.Fatalf("clamped maxTokens = %d, want 4755", got)
	}
}

// TestEstimateReservesThreePointFiveCharsPerToken mirrors pi
// context-estimate.test.ts "reserves 3.5 characters per token for new text
// when limiting output" (upstream 27075fe07, #10497): large new inputs need
// more room than chars/4 allows.
func TestEstimateReservesThreePointFiveCharsPerToken(t *testing.T) {
	ctx := NormalizeContext(Context{
		Messages: []Message{
			estimateTestAssistant(100, 2_000),
			NewUserText(strings.Repeat("x", 3_500), 200),
		},
	})
	est := estimateContextTokens(ctx.Messages)
	if est.Tokens != 3_000 || est.UsageTokens != 2_000 || est.TrailingTokens != 1_000 || est.LastUsageIndex != 0 {
		t.Fatalf("estimate = %+v, want {Tokens:3000 UsageTokens:2000 TrailingTokens:1000 LastUsageIndex:0}", est)
	}
	model := &Model{ContextWindow: 10_000, MaxTokens: 8_000}
	if got := ClampMaxTokensToContext(model, ctx, SimpleMaxTokensDefault(model, nil)); got != 2_904 {
		t.Fatalf("clamped maxTokens = %d, want 2904", got)
	}
}

func TestEstimateUsesUsageAfterResponseToInsertedContext(t *testing.T) {
	// The later assistant (ts 400) responds to the inserted context (ts 300), so
	// its usage applies to the current prefix and anchors the estimate again.
	ctx := NormalizeContext(Context{
		Messages: []Message{
			NewUserText("summary", 200),
			estimateTestAssistant(100, 9_500),
			NewUserText("new prompt", 300),
			estimateTestAssistant(400, 2_000),
			NewUserText("tail", 500),
		},
	})
	est := estimateContextTokens(ctx.Messages)
	if est.Tokens != 2_002 || est.UsageTokens != 2_000 || est.TrailingTokens != 2 || est.LastUsageIndex != 3 {
		t.Fatalf("post-insert estimate = %+v, want {Tokens:2002 UsageTokens:2000 TrailingTokens:2 LastUsageIndex:3}", est)
	}
}

func TestCalculateContextTokensFallback(t *testing.T) {
	// totalTokens preferred; else sum of input+output+cacheRead+cacheWrite.
	if got := calculateContextTokens(Usage{TotalTokens: 42, Input: 1}); got != 42 {
		t.Fatalf("totalTokens path = %d, want 42", got)
	}
	if got := calculateContextTokens(Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}); got != 10 {
		t.Fatalf("component sum = %d, want 10", got)
	}
}

func TestClampMaxTokensToContext(t *testing.T) {
	// contextWindow <= 0 -> max(MIN_MAX_TOKENS, maxTokens).
	noWindow := &Model{ContextWindow: 0}
	if got := ClampMaxTokensToContext(noWindow, TranscriptContext{}, 8000); got != 8000 {
		t.Fatalf("no-window large = %d, want 8000", got)
	}
	if got := ClampMaxTokensToContext(noWindow, TranscriptContext{}, 0); got != minMaxTokens {
		t.Fatalf("no-window floor = %d, want %d", got, minMaxTokens)
	}

	// Small context: available = window - estimate - safety is still > maxTokens,
	// so the clamp is a no-op (this is the golden-stability invariant).
	model := &Model{ContextWindow: 128000, MaxTokens: 16384}
	smallCtx := NormalizeContext(Context{SystemPrompt: "sys", Messages: []Message{NewUserText("hi", 1)}})
	if got := ClampMaxTokensToContext(model, smallCtx, 16384); got != 16384 {
		t.Fatalf("small-context clamp = %d, want 16384 (no-op)", got)
	}

	// Large context forces a clamp. Mirrors the upstream test:
	// contextWindow 10000, maxTokens 8000, a user message of 8000 'x' chars.
	// estimate = ceil(8000/3.5) = 2286; available = 10000 - 2286 - 4096 = 3618
	// (upstream 311f0e020).
	big := &Model{ContextWindow: 10000, MaxTokens: 8000}
	bigCtx := NormalizeContext(Context{Messages: []Message{NewUserText(strings.Repeat("x", 8000), 1)}})
	if got := ClampMaxTokensToContext(big, bigCtx, 8000); got != 3618 {
		t.Fatalf("clamped default = %d, want 3618", got)
	}
	if got := ClampMaxTokensToContext(big, bigCtx, 7000); got != 3618 {
		t.Fatalf("clamped explicit = %d, want 3618", got)
	}

	// Floor at MIN_MAX_TOKENS when available would be negative.
	tiny := &Model{ContextWindow: 100, MaxTokens: 8000}
	hugeCtx := NormalizeContext(Context{Messages: []Message{NewUserText(strings.Repeat("x", 8000), 1)}})
	if got := ClampMaxTokensToContext(tiny, hugeCtx, 8000); got != minMaxTokens {
		t.Fatalf("over-budget floor = %d, want %d", got, minMaxTokens)
	}
}

func TestSimpleMaxTokensDefault(t *testing.T) {
	model := &Model{MaxTokens: 4096}
	if got := SimpleMaxTokensDefault(model, nil); got != 4096 {
		t.Fatalf("nil opts = %d, want 4096", got)
	}
	if got := SimpleMaxTokensDefault(model, &SimpleStreamOptions{}); got != 4096 {
		t.Fatalf("no explicit = %d, want 4096", got)
	}
	mt := 1234
	if got := SimpleMaxTokensDefault(model, &SimpleStreamOptions{StreamOptions: StreamOptions{MaxTokens: &mt}}); got != 1234 {
		t.Fatalf("explicit = %d, want 1234", got)
	}
}

// estimateTestTool is `tool("base_tool")` from the upstream suites:
// {name, description: "base_tool tool", parameters: Type.Object({})}.
func estimateTestTool() Tool {
	return Tool{Name: "base_tool", Description: "base_tool tool", Parameters: Object()}
}

func TestEstimateMessageTokensSystemMessage(t *testing.T) {
	// pi 9e05370b2 estimateMessageTokens system arm: text tokens of
	// getSystemMessageText ("abcd\n\nefgh" -> 3) + tokens of JSON.stringify of
	// toolsAdded (99 chars -> 29) + of toolsRemoved (`[{"name":"gone"}]` -> 5).
	// 37 captured under node at d2e7d8b7c (3.5 chars per token, 27075fe07).
	sys := NewSystemText("abcd", 50)
	sys.Sections = SystemSections{{Name: "s", Value: strp("efgh")}}
	sys.ToolsAdded = []Tool{estimateTestTool()}
	sys.ToolsRemoved = []ToolReference{{Name: "gone"}}
	if got := estimateMessageTokens(sys); got != 37 {
		t.Fatalf("system message tokens = %d, want 37", got)
	}
}

func TestEstimateSystemMessageTimestampInvalidatesStaleUsage(t *testing.T) {
	// Every message's timestamp advances the prefix watermark, system messages
	// included: a system message newer than a response makes that response's
	// usage stale. Expected values captured under node at d2e7d8b7c.
	messages := []Message{
		NewSystemText("newer prompt", 200),
		estimateTestAssistant(100, 9_500),
		NewUserText("tail", 300),
	}
	est := estimateContextTokens(messages)
	if est.Tokens != 8 || est.UsageTokens != 0 || est.TrailingTokens != 8 || est.LastUsageIndex != -1 {
		t.Fatalf("estimate = %+v, want {Tokens:8 UsageTokens:0 TrailingTokens:8 LastUsageIndex:-1}", est)
	}
}

func TestEstimateCountsTrailingSystemMessagesAfterTheAnchor(t *testing.T) {
	late := NewSystemText("late!", 3)
	late.ToolsAdded = []Tool{estimateTestTool()}
	messages := []Message{NewUserText("u", 1), estimateTestAssistant(2, 9_500), late}
	est := estimateContextTokens(messages)
	// Captured under node at d2e7d8b7c.
	if est.Tokens != 9_531 || est.UsageTokens != 9_500 || est.TrailingTokens != 31 || est.LastUsageIndex != 1 {
		t.Fatalf("estimate = %+v, want {Tokens:9531 UsageTokens:9500 TrailingTokens:31 LastUsageIndex:1}", est)
	}
}

// JSON.stringify writes <, >, & and U+2028/U+2029 as themselves, one code unit
// each; encoding/json writes six-character escapes. A tool whose schema text
// uses them must weigh what pi weighs, which is exactly what the same tool
// with plain letters in their place weighs.
func TestEstimateToolsTokensCountsJSONStringifyText(t *testing.T) {
	tool := func(desc string) []Tool { return []Tool{{Name: "t", Description: desc}} }
	plain := estimateToolsTokens(tool("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	for _, desc := range []string{
		"<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<",
		"&&&&&&&&>>>>>>>>&&&&&&&&>>>>>>>>",
		strings.Repeat("\u2028", 16) + strings.Repeat("\u2029", 16),
	} {
		if got := estimateToolsTokens(tool(desc)); got != plain {
			t.Errorf("estimateToolsTokens(%q) = %d, want %d (same JSON.stringify length)", desc, got, plain)
		}
	}
}
