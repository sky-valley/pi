package ai

import (
	"strings"
	"testing"
)

// Hand-computed against pi packages/ai/src/utils/estimate.ts (upstream 09f10595).
// CHARS_PER_TOKEN=4, ESTIMATED_IMAGE_CHARS=4800; estimateTextTokens=ceil(len/4)
// over the JS UTF-16 .length.

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
}

func TestEstimateContextTokensEmpty(t *testing.T) {
	est := estimateContextTokens(Context{})
	if est.Tokens != 0 || est.UsageTokens != 0 || est.TrailingTokens != 0 || est.LastUsageIndex != -1 {
		t.Fatalf("empty context = %+v, want all-zero with LastUsageIndex -1", est)
	}
}

func TestEstimateContextTokensUserText(t *testing.T) {
	// "hello" -> UTF-16 length 5 -> ceil(5/4) = 2 tokens. No usage anchor, no
	// system prompt, no tools.
	ctx := Context{Messages: []Message{NewUserText("hello", 1)}}
	est := estimateContextTokens(ctx)
	if est.Tokens != 2 || est.LastUsageIndex != -1 {
		t.Fatalf("user text = %+v, want Tokens 2, LastUsageIndex -1", est)
	}
}

func TestEstimateContextTokensSystemPromptAndTools(t *testing.T) {
	// systemPrompt "abcd" (4) -> ceil(4/4)=1. user "hi" (2) -> ceil(2/4)=1.
	// tools prefix adds estimateTextTokens(JSON.stringify(tools)).
	tool := Tool{Name: "t", Description: "d"}
	ctx := Context{
		SystemPrompt: "abcd",
		Messages:     []Message{NewUserText("hi", 1)},
		Tools:        []Tool{tool},
	}
	toolsJSON := safeJSONStringify(ctx.Tools)
	wantPrefix := 1 /*system*/ + estimateTextTokens(toolsJSON)
	est := estimateContextTokens(ctx)
	if want := 1 /*user*/ + wantPrefix; est.Tokens != want {
		t.Fatalf("system+tools Tokens = %d, want %d (toolsJSON=%q)", est.Tokens, want, toolsJSON)
	}
	if est.TrailingTokens != 1+wantPrefix {
		t.Fatalf("TrailingTokens = %d, want %d", est.TrailingTokens, 1+wantPrefix)
	}
}

func TestEstimateMessageTokensImage(t *testing.T) {
	// One image block -> ESTIMATED_IMAGE_CHARS 4800 -> ceil(4800/4) = 1200.
	msg := UserMessage{Content: ContentList{ImageContent{Data: "x", MimeType: "image/png"}}}
	if got := estimateMessageTokens(msg); got != 1200 {
		t.Fatalf("image message tokens = %d, want 1200", got)
	}
}

func TestEstimateMessageTokensAssistantToolCall(t *testing.T) {
	// Assistant tool call: chars = name.length + JSON.stringify(arguments).length.
	tc := ToolCall{Name: "run", Arguments: map[string]any{"a": 1}}
	argsJSON := safeJSONStringify(tc.Arguments) // {"a":1} -> 7
	msg := AssistantMessage{Content: ContentList{tc}, StopReason: StopToolUse}
	wantChars := jsStringLength("run") + jsStringLength(argsJSON)
	want := (wantChars + charsPerToken - 1) / charsPerToken // ceil
	if got := estimateMessageTokens(msg); got != want {
		t.Fatalf("assistant tool call tokens = %d, want %d (argsJSON=%q)", got, want, argsJSON)
	}
}

func TestEstimateContextTokensUsageAnchor(t *testing.T) {
	// With a usage anchor, tokens = usageTokens + sum(messages after anchor).
	// The anchor's totalTokens is used; the system prompt / tools prefix is NOT
	// added once an anchor exists.
	assistant := AssistantMessage{
		Content:    ContentList{TextContent{Text: "ok"}},
		StopReason: StopStop,
		Usage:      Usage{TotalTokens: 1000},
	}
	trailing := NewUserText(strings.Repeat("x", 8), 3) // 8 chars -> ceil(8/4)=2
	ctx := Context{
		SystemPrompt: "ignored-because-anchor",
		Tools:        []Tool{{Name: "t"}},
		Messages:     []Message{NewUserText("hi", 1), assistant, trailing},
	}
	est := estimateContextTokens(ctx)
	if est.UsageTokens != 1000 {
		t.Fatalf("UsageTokens = %d, want 1000", est.UsageTokens)
	}
	if est.TrailingTokens != 2 {
		t.Fatalf("TrailingTokens = %d, want 2", est.TrailingTokens)
	}
	if est.Tokens != 1002 {
		t.Fatalf("Tokens = %d, want 1002 (no prefix added when anchored)", est.Tokens)
	}
	if est.LastUsageIndex != 1 {
		t.Fatalf("LastUsageIndex = %d, want 1", est.LastUsageIndex)
	}
}

func TestGetLastAssistantUsageInfoSkipsAbortedAndError(t *testing.T) {
	// Walk backwards; skip aborted/error assistants and zero-usage assistants.
	good := AssistantMessage{StopReason: StopStop, Usage: Usage{Input: 5, Output: 5}} // 10
	aborted := AssistantMessage{StopReason: StopAborted, Usage: Usage{TotalTokens: 999}}
	errored := AssistantMessage{StopReason: StopError, Usage: Usage{TotalTokens: 999}}
	ctx := Context{Messages: []Message{good, aborted, errored}}
	est := estimateContextTokens(ctx)
	if est.UsageTokens != 10 || est.LastUsageIndex != 0 {
		t.Fatalf("est = %+v, want UsageTokens 10 at index 0 (aborted/error skipped)", est)
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
	if got := ClampMaxTokensToContext(noWindow, Context{}, 8000); got != 8000 {
		t.Fatalf("no-window large = %d, want 8000", got)
	}
	if got := ClampMaxTokensToContext(noWindow, Context{}, 0); got != minMaxTokens {
		t.Fatalf("no-window floor = %d, want %d", got, minMaxTokens)
	}

	// Small context: available = window - estimate - safety is still > maxTokens,
	// so the clamp is a no-op (this is the golden-stability invariant).
	model := &Model{ContextWindow: 128000, MaxTokens: 16384}
	smallCtx := Context{SystemPrompt: "sys", Messages: []Message{NewUserText("hi", 1)}}
	if got := ClampMaxTokensToContext(model, smallCtx, 16384); got != 16384 {
		t.Fatalf("small-context clamp = %d, want 16384 (no-op)", got)
	}

	// Large context forces a clamp. Mirrors the upstream test:
	// contextWindow 10000, maxTokens 8000, a user message of 8000 'x' chars.
	// estimate = ceil(8000/4) = 2000; available = 10000 - 2000 - 4096 = 3904.
	big := &Model{ContextWindow: 10000, MaxTokens: 8000}
	bigCtx := Context{Messages: []Message{NewUserText(strings.Repeat("x", 8000), 1)}}
	if got := ClampMaxTokensToContext(big, bigCtx, 8000); got != 3904 {
		t.Fatalf("clamped default = %d, want 3904", got)
	}
	if got := ClampMaxTokensToContext(big, bigCtx, 7000); got != 3904 {
		t.Fatalf("clamped explicit = %d, want 3904", got)
	}

	// Floor at MIN_MAX_TOKENS when available would be negative.
	tiny := &Model{ContextWindow: 100, MaxTokens: 8000}
	hugeCtx := Context{Messages: []Message{NewUserText(strings.Repeat("x", 8000), 1)}}
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
