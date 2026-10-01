package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicProviderParsesStream(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing/incorrect api key header: %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("missing anthropic-version header")
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "claude-test", Name: "Claude Test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		BaseURL: server.URL, Input: []string{"text", "image"}, MaxTokens: 4096,
		Cost: ai.ModelCost{Input: 3, Output: 15},
	}
	req := ai.Context{
		SystemPrompt: "be helpful",
		Messages:     []ai.Message{ai.NewUserText("hi", 1)},
		Tools: []ai.Tool{{
			Name: "get_weather", Description: "weather",
			Parameters: ai.Object(ai.Prop("city", ai.String())),
		}},
	}
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}}}

	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req), &AnthropicOptions{StreamOptions: opts.StreamOptions}).Result()

	if final.StopReason != ai.StopToolUse {
		t.Fatalf("expected toolUse stop, got %s (err=%s)", final.StopReason, final.ErrorMessage)
	}
	if final.ResponseID != "msg_1" {
		t.Fatalf("expected responseId msg_1, got %q", final.ResponseID)
	}
	if len(final.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(final.Content))
	}
	text, ok := final.Content[0].(ai.TextContent)
	if !ok || text.Text != "Hello world" {
		t.Fatalf("text block wrong: %#v", final.Content[0])
	}
	tc, ok := final.Content[1].(ai.ToolCall)
	if !ok || tc.Name != "get_weather" || tc.Arguments["city"] != "Paris" {
		t.Fatalf("tool call wrong: %#v", final.Content[1])
	}
	// Usage + cost
	if final.Usage.Input != 10 || final.Usage.Output != 15 {
		t.Fatalf("usage wrong: %+v", final.Usage)
	}
	if final.Usage.Cost.Total == 0 {
		t.Fatalf("cost not computed: %+v", final.Usage.Cost)
	}

	// Request body shape
	if gotBody["model"] != "claude-test" {
		t.Fatalf("request model wrong: %v", gotBody["model"])
	}
	if _, ok := gotBody["system"]; !ok {
		t.Fatalf("system prompt not sent")
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not sent correctly: %v", gotBody["tools"])
	}
}

// Upstream f9a49869: a stream that reaches message_stop without ever reporting a
// stop reason (no message_delta) leaves the reason pending, which must fail
// rather than silently defaulting to "stop".
func TestAnthropicPendingStopReasonFailsStream(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		BaseURL: server.URL, MaxTokens: 4096,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}}}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("pending stop reason should fail, got %s", final.StopReason)
	}
	if final.ErrorMessage != "Anthropic stream ended without a stop reason" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// TestAnthropicReasoningTokens checks that output_tokens_details.thinking_tokens
// on the final message_delta populates Usage.Reasoning, and that absence leaves
// it 0 (pi sets reasoning only when the field is present).
func TestAnthropicReasoningTokens(t *testing.T) {
	run := func(t *testing.T, deltaUsage string) *ai.AssistantMessage {
		t.Helper()
		sse := "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + deltaUsage + "}\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(200)
			io.WriteString(w, sse)
		}))
		t.Cleanup(server.Close)
		model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
			BaseURL: server.URL, MaxTokens: 4096, Cost: ai.ModelCost{Input: 3, Output: 15}}
		req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
		return StreamAnthropic(context.Background(), model, ai.NormalizeContext(req),
			&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
			Result()
	}

	withThinking := run(t, `{"output_tokens":50,"output_tokens_details":{"thinking_tokens":37}}`)
	if withThinking.Usage.Reasoning != 37 {
		t.Fatalf("expected reasoning 37, got %+v", withThinking.Usage)
	}
	if withThinking.Usage.Output != 50 {
		t.Fatalf("output wrong: %+v", withThinking.Usage)
	}

	without := run(t, `{"output_tokens":50}`)
	if without.Usage.Reasoning != 0 {
		t.Fatalf("expected reasoning 0 when absent, got %+v", without.Usage)
	}
}

func TestAnthropicProviderErrorOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()
	model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, MaxTokens: 100}
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "429") {
		t.Fatalf("expected 429 error, got %s / %q", final.StopReason, final.ErrorMessage)
	}
}

func TestAnthropicRegisterAndStreamSimple(t *testing.T) {
	RegisterAnthropic()
	if _, ok := ai.GetApiProvider(ai.APIAnthropicMessages); !ok {
		t.Fatal("anthropic provider not registered")
	}
}

// anthropicCapture spins up a test server returning a fixed SSE body, runs the
// stream, and returns the captured request headers + decoded JSON body.
func anthropicCapture(t *testing.T, model *ai.Model, req ai.Context, opts *AnthropicOptions, sse string) (http.Header, map[string]any) {
	t.Helper()
	var gotHeaders http.Header
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	StreamAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()
	return gotHeaders, gotBody
}

// --- Job A.1: OAuth headers + stealth system prompt + tool-name canonicalization ---

func TestAnthropicOAuthHeadersAndStealth(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096,
	}
	// A "read" tool — OAuth stealth mode must canonicalize it to "Read".
	req := ai.Context{
		SystemPrompt: "be helpful",
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			&ai.AssistantMessage{
				// Same-model so transformMessages keeps the tool call.
				Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
				Content: ai.ContentList{ai.ToolCall{ID: "toolu_1", Name: "read", Arguments: map[string]any{"p": "x"}}},
			},
			ai.ToolResultMessage{ToolCallID: "toolu_1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
		},
		Tools: []ai.Tool{{Name: "read", Description: "read a file", Parameters: ai.Object(ai.Prop("p", ai.String()))}},
	}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-ant-oat-secret"}}}
	headers, body := anthropicCapture(t, model, req, opts, anthropicSSE)

	if got := headers.Get("authorization"); got != "Bearer sk-ant-oat-secret" {
		t.Fatalf("authorization header wrong: %q", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must not be set for OAuth: %q", got)
	}
	// Spelled through the constant so a version bump is one edit; the literal
	// itself is pinned by TestAnthropicClaudeCodeUserAgentPinnedToUpstream.
	if got := headers.Get("user-agent"); got != "claude-cli/"+claudeCodeVersion {
		t.Fatalf("user-agent wrong: %q", got)
	}
	if got := headers.Get("x-app"); got != "cli" {
		t.Fatalf("x-app wrong: %q", got)
	}
	// anthropic-beta must lead with the OAuth betas in pi's exact order. Since
	// upstream 4e69b0c28 they are the WHOLE header here: interleaved thinking now
	// needs a reasoning model with thinking explicitly on, which this is not, and
	// this model streams tool input eagerly. TestAnthropicInterleavedBetaGate
	// pins that gate on its own.
	beta := headers.Get("anthropic-beta")
	if beta != "claude-code-20250219,oauth-2025-04-20" {
		t.Fatalf("oauth betas missing/misordered: %q", beta)
	}

	// Stealth system prompt: first block is the Claude Code identity, then ours.
	system, ok := body["system"].([]any)
	if !ok || len(system) != 2 {
		t.Fatalf("expected 2 system blocks, got %v", body["system"])
	}
	first := system[0].(map[string]any)
	if first["text"] != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("stealth system prompt wrong: %v", first)
	}
	if system[1].(map[string]any)["text"] != "be helpful" {
		t.Fatalf("user system prompt missing: %v", system[1])
	}

	// Tool name canonicalized read -> Read in tools list.
	tools := body["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "Read" {
		t.Fatalf("tool name not canonicalized: %v", tools[0])
	}
	// Assistant tool_use name canonicalized read -> Read in messages.
	msgs := body["messages"].([]any)
	foundToolUse := false
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] != "assistant" {
			continue
		}
		for _, c := range mm["content"].([]any) {
			cc := c.(map[string]any)
			if cc["type"] == "tool_use" {
				foundToolUse = true
				if cc["name"] != "Read" {
					t.Fatalf("tool_use name not canonicalized: %v", cc)
				}
			}
		}
	}
	if !foundToolUse {
		t.Fatalf("assistant tool_use block not found in %v", msgs)
	}
}

// The Claude Code identity version is a byte-golden wire value, not an internal
// detail: it must equal upstream's `claudeCodeVersion` literal
// (packages/ai/src/api/anthropic-messages.ts, "2.1.280" as of upstream
// 3a624b82d). Every other assertion in the port spells this header through the
// constant, so this is the one place that pins the literal — without it a stale
// constant would keep the whole suite green while every OAuth request went out
// impersonating a retired Claude Code build.
func TestAnthropicClaudeCodeUserAgentPinnedToUpstream(t *testing.T) {
	h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
		APIKey: "sk-ant-oat-secret",
	}))
	wantUserAgent(t, h, "claude-cli/2.1.280")
}

// --- Job A.2: cache_control 1h beta on long retention ---

func TestAnthropicLongCacheRetentionTTL(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096,
	}
	req := ai.Context{
		SystemPrompt: "sys",
		Messages:     []ai.Message{ai.NewUserText("hi", 1)},
		Tools:        []ai.Tool{{Name: "t", Description: "d", Parameters: ai.Object(ai.Prop("q", ai.String()))}},
	}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}, CacheRetention: ai.CacheLong}}
	headers, body := anthropicCapture(t, model, req, opts, anthropicSSE)

	wantCC := map[string]any{"type": "ephemeral", "ttl": "1h"}
	checkCC := func(label string, blk map[string]any) {
		cc, ok := blk["cache_control"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing cache_control: %v", label, blk)
		}
		if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
			t.Fatalf("%s cache_control wrong, want %v got %v", label, wantCC, cc)
		}
	}
	// System block.
	checkCC("system[0]", body["system"].([]any)[0].(map[string]any))
	// Last tool gets cache_control.
	tools := body["tools"].([]any)
	checkCC("tools[last]", tools[len(tools)-1].(map[string]any))
	// Last user content block.
	msgs := body["messages"].([]any)
	lastUser := msgs[len(msgs)-1].(map[string]any)
	// The trailing string content must have become a marked block list.
	uc, ok := lastUser["content"].([]any)
	if !ok || len(uc) == 0 {
		t.Fatalf("messages[last].content = %#v, want a non-empty block list", lastUser["content"])
	}
	checkCC("messages[last].content[last]", uc[len(uc)-1].(map[string]any))

	// pi does NOT send any extended-cache anthropic-beta header; the 1h TTL is
	// carried entirely by cache_control. This model neither reasons nor was asked
	// to think, so since upstream 4e69b0c28 it earns no beta at all and the
	// header is absent — which is the stronger form of the same assertion.
	beta := headers.Get("anthropic-beta")
	if strings.Contains(beta, "extended-cache") || strings.Contains(beta, "ttl") {
		t.Fatalf("unexpected extended-cache beta header (pi emits none): %q", beta)
	}
	if beta != "" {
		t.Fatalf("expected no anthropic-beta header, got %q", beta)
	}
}

func TestAnthropicShortCacheRetentionNoTTL(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096,
	}
	req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}, CacheRetention: ai.CacheShort}}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	cc := body["system"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Fatalf("short retention should still be ephemeral: %v", cc)
	}
	if _, hasTTL := cc["ttl"]; hasTTL {
		t.Fatalf("short retention must not carry a ttl: %v", cc)
	}
}

// --- Job A.3: allowEmptySignature thinking-block replay ---

func anthropicThinkingReplay(t *testing.T, allowEmptySig bool) map[string]any {
	t.Helper()
	compat := []byte(`{}`)
	if allowEmptySig {
		compat = []byte(`{"allowEmptySignature":true}`)
	}
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true, Compat: compat,
	}
	req := ai.Context{
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			&ai.AssistantMessage{
				// Same model so transformMessages preserves the empty-sig thinking block.
				Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
				Content: ai.ContentList{
					ai.ThinkingContent{Thinking: "let me think", ThinkingSignature: ""},
					ai.TextContent{Text: "answer"},
				},
			},
			ai.NewUserText("again", 2),
		},
	}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	msgs := body["messages"].([]any)
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			return mm
		}
	}
	t.Fatalf("assistant message not found in %v", msgs)
	return nil
}

func TestAnthropicAllowEmptySignatureFalseConvertsToText(t *testing.T) {
	am := anthropicThinkingReplay(t, false)
	blocks := am["content"].([]any)
	// Empty-sig thinking must downgrade to a text block (pi default).
	first := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "let me think" {
		t.Fatalf("empty-sig thinking should become text, got %v", first)
	}
	for _, b := range blocks {
		if b.(map[string]any)["type"] == "thinking" {
			t.Fatalf("no thinking block expected when allowEmptySignature=false: %v", blocks)
		}
	}
}

func TestAnthropicAllowEmptySignatureTruePreservesThinking(t *testing.T) {
	am := anthropicThinkingReplay(t, true)
	blocks := am["content"].([]any)
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" {
		t.Fatalf("expected preserved thinking block, got %v", first)
	}
	if first["thinking"] != "let me think" {
		t.Fatalf("thinking text wrong: %v", first)
	}
	if sig, ok := first["signature"]; !ok || sig != "" {
		t.Fatalf("expected empty signature field present, got %v", first)
	}
}

// Mirrors pi anthropic-empty-thinking-signature-compat.test.ts (upstream
// 6731a0ba): a thinking block with empty text but a real signature is preserved
// as a thinking block (previously dropped), even without allowEmptySignature.
func TestAnthropicPreservesEmptyThinkingWithSignature(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true, Compat: []byte(`{}`),
	}
	req := ai.Context{
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			&ai.AssistantMessage{
				Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
				Content: ai.ContentList{
					ai.ThinkingContent{Thinking: "", ThinkingSignature: "signed-thinking"},
				},
			},
			ai.NewUserText("again", 2),
		},
	}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	var assistant map[string]any
	for _, m := range body["messages"].([]any) {
		if mm := m.(map[string]any); mm["role"] == "assistant" {
			assistant = mm
		}
	}
	if assistant == nil {
		t.Fatalf("assistant message not found in %v", body["messages"])
	}
	blocks := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("want exactly one block, got %v", blocks)
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "" || first["signature"] != "signed-thinking" {
		t.Fatalf("empty-text signed thinking block wrong: %v", first)
	}
}

// --- Job A.4: forceAdaptiveThinking / output_config.effort request shape ---
//
// Our port DOES implement forceAdaptiveThinking + output_config.effort, matching
// pi anthropic.ts:955-966 (params.thinking={type:"adaptive",display} and
// params.output_config={effort}). These tests pin that request shape, including
// pi's createClient rule (anthropic.ts:793) that the interleaved-thinking beta is
// skipped for adaptive models.

func TestAnthropicForceAdaptiveThinkingRequestShape(t *testing.T) {
	model := &ai.Model{
		ID: "claude-opus-adaptive", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
		Compat: []byte(`{"forceAdaptiveThinking":true}`),
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	// streamSimpleAnthropic maps reasoning -> effort for adaptive models.
	opts := &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}},
		Reasoning:     ai.ThinkingHigh,
	}
	var gotBody map[string]any
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()

	thinking, ok := gotBody["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking object missing: %v", gotBody["thinking"])
	}
	if thinking["type"] != "adaptive" {
		t.Fatalf("expected adaptive thinking, got %v", thinking)
	}
	if thinking["display"] != "summarized" {
		t.Fatalf("expected summarized display default, got %v", thinking)
	}
	oc, ok := gotBody["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing: %v", gotBody["output_config"])
	}
	if oc["effort"] != "high" {
		t.Fatalf("expected effort=high, got %v", oc)
	}
	// Adaptive models skip the interleaved-thinking beta header (pi anthropic.ts:793).
	if strings.Contains(gotHeaders.Get("anthropic-beta"), interleavedThinkingBeta) {
		t.Fatalf("adaptive model must not send interleaved beta: %q", gotHeaders.Get("anthropic-beta"))
	}
}

// streamSimpleAnthropic clamps max_tokens to the remaining context window and
// caps thinking_budget at max(0, maxTokens-1024) (upstream 09f10595).
func TestAnthropicStreamSimpleClampsMaxTokensAndBudget(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL,
		Input: []string{"text"}, MaxTokens: 8000, ContextWindow: 10000, Reasoning: true,
	}
	// 8000 'x' chars -> estimate 2000; available = 10000-2000-4096 = 3904.
	// adjustMaxTokensForThinking(3904, 8000, high=16384) -> maxTokens 8000,
	// budget max(0,8000-1024)=6976. Re-clamp maxTokens -> 3904.
	// thinking_budget = min(6976, max(0,3904-1024)) = min(6976,2880) = 2880.
	req := ai.Context{Messages: []ai.Message{ai.NewUserText(strings.Repeat("x", 8000), 1)}}
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, Reasoning: ai.ThinkingHigh}
	StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()

	if v, _ := gotBody["max_tokens"].(float64); v != 3904 {
		t.Fatalf("max_tokens = %v, want 3904", gotBody["max_tokens"])
	}
	thinking, ok := gotBody["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking object missing: %v", gotBody["thinking"])
	}
	if v, _ := thinking["budget_tokens"].(float64); v != 2880 {
		t.Fatalf("budget_tokens = %v, want 2880", thinking["budget_tokens"])
	}
}

// clampThinkingBudgetToAnswerRoom is pi's extracted clamp (simple-options.ts:75,
// upstream b23741269). The table covers the max(0,...) floor, which no wire test
// reaches: on the openai path a negative budget is swallowed by the budget<=0
// omit, and no anthropic test uses a ceiling below minAnswerTokens.
func TestClampThinkingBudgetToAnswerRoom(t *testing.T) {
	for _, tc := range []struct {
		budget, ceiling, want int
	}{
		{16384, 16384, 15360}, // budget meets the ceiling: answer room wins
		{8192, 4096, 3072},    // ceiling below the budget
		{1024, 100000, 1024},  // roomy ceiling: the budget wins
		{8192, 512, 0},        // ceiling under minAnswerTokens: floored, never negative
		{8192, 1024, 0},       // ceiling exactly minAnswerTokens
	} {
		if got := clampThinkingBudgetToAnswerRoom(tc.budget, tc.ceiling); got != tc.want {
			t.Errorf("clampThinkingBudgetToAnswerRoom(%d, %d) = %d, want %d", tc.budget, tc.ceiling, got, tc.want)
		}
	}
}

// streamSimpleAnthropic collapses xhigh AND max to high before reading the
// budget table (pi clampReasoning, simple-options.ts): the table has no
// xhigh/max rows, so an unclamped level would read a zero budget and fall to
// the 1024 floor instead of high's 16384.
func TestAnthropicStreamSimpleClampsXHighAndMaxToHighBudget(t *testing.T) {
	for _, level := range []ai.ThinkingLevel{ai.ThinkingXHigh, ai.ThinkingMax} {
		t.Run(string(level), func(t *testing.T) {
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &gotBody)
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, anthropicSSE)
			}))
			defer server.Close()
			model := &ai.Model{
				ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL,
				Input: []string{"text"}, MaxTokens: 32768, ContextWindow: 200000, Reasoning: true,
			}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, Reasoning: level}
			StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()

			thinking, ok := gotBody["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking object missing: %v", gotBody["thinking"])
			}
			if v, _ := thinking["budget_tokens"].(float64); v != 16384 {
				t.Fatalf("budget_tokens = %v, want high's 16384 (pi clampReasoning)", thinking["budget_tokens"])
			}
		})
	}
}

// --- E1: cloudflare-ai-gateway branch ---

func TestAnthropicCloudflareAIGateway(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct123")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "gw456")
	var gotHeaders http.Header
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "cloudflare-ai-gateway",
		BaseURL: server.URL + "/{CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}/anthropic",
		Input:   []string{"text"}, MaxTokens: 4096,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	// Use an sk-ant-oat key: pi resolves cloudflare-ai-gateway to header-owned
	// auth with no apiKey at all, so its OAuth sniff never fires however the
	// gateway key looks — no OAuth identity may leak through.
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-ant-oat-cfkey"}}}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	// URL placeholders substituted from env.
	if gotPath != "/acct123/gw456/anthropic/v1/messages" {
		t.Fatalf("cloudflare base url not resolved: %q", gotPath)
	}
	if got := gotHeaders.Get("cf-aig-authorization"); got != "Bearer sk-ant-oat-cfkey" {
		t.Fatalf("cf-aig-authorization wrong: %q", got)
	}
	// x-api-key and Authorization explicitly NOT set (pi sends null for both).
	if got := gotHeaders.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must not be set for cloudflare-ai-gateway: %q", got)
	}
	if got := gotHeaders.Get("authorization"); got != "" {
		t.Fatalf("authorization must not be set for cloudflare-ai-gateway: %q", got)
	}
	// OAuth sniff must not have fired: no Claude Code identity headers/betas.
	if strings.Contains(gotHeaders.Get("anthropic-beta"), "oauth-2025-04-20") {
		t.Fatalf("oauth betas must not be sent for cloudflare provider: %q", gotHeaders.Get("anthropic-beta"))
	}
	if gotHeaders.Get("x-app") != "" {
		t.Fatalf("x-app must not be set for cloudflare provider")
	}
}

func TestAnthropicCloudflareMissingEnvFailsStream(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "cloudflare-ai-gateway",
		BaseURL:   "https://gateway.ai.cloudflare.com/v1/{CLOUDFLARE_ACCOUNT_ID}/gw/anthropic",
		MaxTokens: 4096,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	want := "CLOUDFLARE_ACCOUNT_ID is required for provider cloudflare-ai-gateway but is not set."
	if final.ErrorMessage != want {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// --- E2: github-copilot dynamic headers ---

func TestAnthropicCopilotDynamicHeaders(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "github-copilot",
		Input: []string{"text", "image"}, MaxTokens: 4096,
	}
	// Last message is an assistant turn -> X-Initiator agent; include an image
	// in a user message -> Copilot-Vision-Request true.
	req := ai.Context{Messages: []ai.Message{
		ai.UserMessage{Content: ai.ContentList{
			ai.TextContent{Text: "look"},
			ai.ImageContent{MimeType: "image/png", Data: "AAAA"},
		}},
		&ai.AssistantMessage{Api: ai.APIAnthropicMessages, Provider: "github-copilot", Model: "claude-test",
			Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-ant-oat-copilot"}}}
	headers, _ := anthropicCapture(t, model, req, opts, anthropicSSE)

	if got := headers.Get("X-Initiator"); got != "agent" {
		t.Fatalf("X-Initiator wrong: %q", got)
	}
	if got := headers.Get("Openai-Intent"); got != "conversation-edits" {
		t.Fatalf("Openai-Intent wrong: %q", got)
	}
	if got := headers.Get("Copilot-Vision-Request"); got != "true" {
		t.Fatalf("Copilot-Vision-Request wrong: %q", got)
	}
	// Copilot branch precedes the OAuth sniff: bearer auth, no Claude Code identity.
	if got := headers.Get("authorization"); got != "Bearer sk-ant-oat-copilot" {
		t.Fatalf("authorization wrong: %q", got)
	}
	if strings.Contains(headers.Get("anthropic-beta"), "oauth-2025-04-20") {
		t.Fatalf("oauth betas must not leak into copilot branch: %q", headers.Get("anthropic-beta"))
	}
	if headers.Get("x-api-key") != "" {
		t.Fatalf("x-api-key must not be set for copilot")
	}
}

// --- E2b: ANTHROPIC_AUTH_TOKEN bearer auth (upstream 24e5cc04) ---

func TestAnthropicAuthTokenBearerHeader(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", MaxTokens: 4096,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	// No API key: the auth token alone must authenticate the request.
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{Env: map[string]string{ai.AnthropicAuthTokenEnv: "my-auth-token"}},
	}}
	headers, _ := anthropicCapture(t, model, req, opts, anthropicSSE)

	if got := headers.Get("authorization"); got != "Bearer my-auth-token" {
		t.Fatalf("authorization wrong: %q", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must not be set for auth-token auth: %q", got)
	}
	// Auth token is a plain bearer credential, not OAuth: no Claude Code identity.
	if strings.Contains(headers.Get("anthropic-beta"), "oauth-2025-04-20") {
		t.Fatalf("oauth betas must not be sent for auth-token auth: %q", headers.Get("anthropic-beta"))
	}
	if headers.Get("x-app") != "" {
		t.Fatalf("x-app must not be set for auth-token auth")
	}
}

func TestAnthropicExplicitKeyBeatsAuthToken(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", MaxTokens: 4096,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	// pi resolve() checks the stored/explicit credential key BEFORE the auth
	// token, so a resolved apiKey wins over ANTHROPIC_AUTH_TOKEN. The provider
	// only reads the token when no key was resolved (apiKey == "").
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-ant-plain-key", Env: map[string]string{ai.AnthropicAuthTokenEnv: "tok"}},
	}}
	headers, _ := anthropicCapture(t, model, req, opts, anthropicSSE)

	if got := headers.Get("x-api-key"); got != "sk-ant-plain-key" {
		t.Fatalf("explicit key must win: x-api-key = %q", got)
	}
	if got := headers.Get("authorization"); got != "" {
		t.Fatalf("authorization must not be set when an explicit key wins: %q", got)
	}
}

// --- E3: thinking tri-state ---

func TestAnthropicThinkingOmittedWhenNotProvided(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	// Generic registry path: plain StreamOptions -> ThinkingProvided stays false.
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	if _, ok := body["thinking"]; ok {
		t.Fatalf("thinking key must be OMITTED when not provided (pi undefined), got %v", body["thinking"])
	}
}

func TestAnthropicThinkingExplicitFalseSendsDisabled(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, ThinkingProvided: true, ThinkingEnabled: false}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("explicit false must send {type:disabled}, got %v", body["thinking"])
	}
}

func TestAnthropicThinkingOffNullOmitsDisabled(t *testing.T) {
	// pi 9ccfcd7c: thinkingLevelMap off:null marks {type:"disabled"} as
	// unsupported (Claude Fable 5) -> omit the thinking key entirely.
	model := &ai.Model{
		ID: "claude-fable-5", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
		ThinkingLevelMap: ai.ThinkingLevelMap{"off": nil, "xhigh": strPtr("xhigh")},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, ThinkingProvided: true, ThinkingEnabled: false}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	if _, ok := body["thinking"]; ok {
		t.Fatalf("off:null model must omit thinking key when off, got %v", body["thinking"])
	}
	if _, ok := body["output_config"]; ok {
		t.Fatalf("off:null model must not send output_config when off, got %v", body["output_config"])
	}
}

func TestAnthropicThinkingOffMappedStillSendsDisabled(t *testing.T) {
	// A thinkingLevelMap whose off maps to a non-null value keeps the
	// {type:"disabled"} payload (pi: `thinkingLevelMap?.off !== null`).
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
		ThinkingLevelMap: ai.ThinkingLevelMap{"off": strPtr("none")},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, ThinkingProvided: true, ThinkingEnabled: false}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("off mapped non-null must still send {type:disabled}, got %v", body["thinking"])
	}
}

func TestAnthropicThinkingEnabledSendsBudget(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{
		StreamOptions:    ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}},
		ThinkingProvided: true, ThinkingEnabled: true, ThinkingBudgetTokens: 2048,
	}
	_, body := anthropicCapture(t, model, req, opts, anthropicSSE)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2048) {
		t.Fatalf("enabled thinking shape wrong: %v", body["thinking"])
	}
	if thinking["display"] != "summarized" {
		t.Fatalf("default display wrong: %v", thinking)
	}
}

func TestAnthropicStreamSimpleNoReasoningDisablesThinking(t *testing.T) {
	// pi streamSimpleAnthropic passes thinkingEnabled:false when no reasoning is
	// requested -> explicit {type:disabled} (NOT omitted).
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req),
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
	thinking, ok := gotBody["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("streamSimple without reasoning must send {type:disabled}, got %v", gotBody["thinking"])
	}
}

// --- E4: session affinity suppressed when cacheRetention is none ---

func TestAnthropicSessionAffinityRetention(t *testing.T) {
	// pi 6184307c: sendSessionAffinityHeaders now comes from catalog compat
	// (fireworks no longer auto-detects), so set it explicitly as the catalog does.
	mk := func(retention ai.CacheRetention) http.Header {
		model := &ai.Model{
			ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "fireworks",
			Input: []string{"text"}, MaxTokens: 4096,
			Compat: json.RawMessage(`{"sendSessionAffinityHeaders":true}`),
		}
		req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
		opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
			SessionID:              "sess-1",
			CacheRetention:         retention,
		}}
		headers, _ := anthropicCapture(t, model, req, opts, anthropicSSE)
		return headers
	}
	if got := mk(ai.CacheShort).Get("x-session-affinity"); got != "sess-1" {
		t.Fatalf("x-session-affinity missing with short retention: %q", got)
	}
	if got := mk(ai.CacheNone).Get("x-session-affinity"); got != "" {
		t.Fatalf("x-session-affinity must be suppressed when retention=none (pi anthropic.ts:497): %q", got)
	}
}

// --- E5a: delta-vs-block-type guards ---

func TestAnthropicMismatchedDeltaDroppedSilently(t *testing.T) {
	// text_delta aimed at a tool_use block and thinking_delta aimed at a text
	// block must be dropped without corrupting state (pi anthropic.ts:586-620).
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"BAD"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"NOPE"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stream should complete cleanly: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	tc, ok := final.Content[0].(ai.ToolCall)
	if !ok || tc.Arguments["a"] != float64(1) {
		t.Fatalf("tool args corrupted by mismatched delta: %#v", final.Content[0])
	}
	text, ok := final.Content[1].(ai.TextContent)
	if !ok || text.Text != "ok" {
		t.Fatalf("text corrupted by mismatched thinking_delta: %#v", final.Content[1])
	}
}

// --- E5b: bare-CR SSE line breaks ---

func TestAnthropicBareCRSSE(t *testing.T) {
	// pi's decoder treats \r, \n, and \r\n all as line breaks.
	sse := strings.ReplaceAll(`event: message_start
data: {"type":"message_start","message":{"id":"msg_cr","usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"crlf-free"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`, "\n", "\r")
	model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("bare-CR SSE not parsed: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	text, ok := final.Content[0].(ai.TextContent)
	if !ok || text.Text != "crlf-free" {
		t.Fatalf("text wrong: %#v", final.Content)
	}
}

// --- E5c: onPayload error fails the stream ---

func TestAnthropicOnPayloadErrorFailsStream(t *testing.T) {
	model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
	}))
	defer server.Close()
	model.BaseURL = server.URL
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", OnPayload: func(payload any, m *ai.Model) (any, error) {
			return nil, errors.New("payload veto")
		}},
	}}
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "payload veto" {
		t.Fatalf("onPayload error must fail the stream: %s / %q", final.StopReason, final.ErrorMessage)
	}
	if requested {
		t.Fatalf("request must not be sent when onPayload errors")
	}
}

// TestFable5DisabledThinkingGateLive drives the 9ccfcd7c disabled-thinking gate
// end-to-end through the CATALOG-resolved Claude Fable 5 model (not a fabricated
// literal). The gate went live with the pi 0.79.3 catalog, which ships
// thinkingLevelMap off:null for fable-5 — so an anthropic request with thinking
// off must omit the thinking key entirely. If a future catalog regen drops the
// off key, the first assertion fails and the gate silently reverts: that is the
// signal to re-confirm upstream intent.
func TestFable5DisabledThinkingGateLive(t *testing.T) {
	m := ai.GetModel("anthropic", "claude-fable-5")
	if m == nil {
		t.Fatal("claude-fable-5 missing from catalog")
	}
	off, present := m.ThinkingLevelMap["off"]
	if !present || off != nil {
		t.Fatalf("expected catalog fable-5 to carry off:null (gate live); got present=%v val=%v — "+
			"if upstream dropped off:null, re-confirm the disabled-thinking gate before changing this", present, off)
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, ThinkingProvided: true, ThinkingEnabled: false}
	_, body := anthropicCapture(t, m, req, opts, anthropicSSE)
	if _, ok := body["thinking"]; ok {
		t.Fatalf("catalog fable-5 with thinking off must omit the thinking key, got %v", body["thinking"])
	}
}

func TestAnthropicRefusalPreservesExplanation(t *testing.T) {
	explanation := "This request triggered restrictions on violative cyber content and was blocked under Anthropic's Usage Policy. To learn more, provide feedback, or request an exemption based on how you use Claude, visit our help center: https://support.claude.com/en/articles/14604842-real-time-cyber-safeguards-on-claude."
	delta, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": "refusal",
			"stop_details": map[string]any{
				"type":        "refusal",
				"category":    "cyber",
				"explanation": explanation,
			},
		},
		"usage": map[string]any{"output_tokens": 0},
	})
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":412,"output_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		"data: " + string(delta) + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, MaxTokens: 4096}
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("blocked request", 1)}}),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if final.ErrorMessage != explanation {
		t.Fatalf("expected explanation as error message, got %q", final.ErrorMessage)
	}
}

func TestAnthropicRefusalWithoutExplanationFallback(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":0}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, MaxTokens: 4096}
	final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("blocked request", 1)}}),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if final.ErrorMessage != "The model refused to complete the request" {
		t.Fatalf("expected fallback refusal message, got %q", final.ErrorMessage)
	}
}

// TestAnthropicRawStopReason mirrors pi's anthropic-sse-parsing.test.ts additions
// in d7b02636: the wire stop_reason is preserved verbatim on rawStopReason, and
// "sensitive" now carries a descriptive error message instead of a bare error.
func TestAnthropicRawStopReason(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		wantStop   ai.StopReason
		wantErrMsg string
	}{
		{name: "end_turn", stopReason: "end_turn", wantStop: ai.StopStop},
		{name: "max_tokens", stopReason: "max_tokens", wantStop: ai.StopLength},
		{name: "sensitive", stopReason: "sensitive", wantStop: ai.StopError, wantErrMsg: "Provider stopped with: sensitive"},
		{name: "refusal", stopReason: "refusal", wantStop: ai.StopError, wantErrMsg: "The model refused to complete the request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_sensitive","usage":{"input_tokens":12,"output_tokens":0}}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"` + tt.stopReason + `"},"usage":{"output_tokens":0}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, sse)
			}))
			defer server.Close()

			model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, MaxTokens: 4096}
			final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("blocked request", 1)}}),
				&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
				Result()

			if final.StopReason != tt.wantStop {
				t.Fatalf("stopReason = %s, want %s", final.StopReason, tt.wantStop)
			}
			if final.RawStopReason != tt.stopReason {
				t.Fatalf("rawStopReason = %q, want %q", final.RawStopReason, tt.stopReason)
			}
			if final.ErrorMessage != tt.wantErrMsg {
				t.Fatalf("errorMessage = %q, want %q", final.ErrorMessage, tt.wantErrMsg)
			}
		})
	}
}

// TestAnthropic1hCacheWriteCost mirrors upstream 0be5bb6c
// (anthropic-cache-write-1h-cost): the 1h slice of cacheWrite is priced at 2x the
// model's input rate, the remaining (5m) slice at the normal cacheWrite rate.
// Driven through the catalog-resolved claude-opus-4-8 (input 5, cacheWrite 6.25
// per Mtok), so a catalog regen that shifts those rates trips this test.
func TestAnthropic1hCacheWriteCost(t *testing.T) {
	cacheWrite1hSSE := func(cacheCreation string) string {
		startUsage := `"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":1000000`
		if cacheCreation != "" {
			startUsage += "," + cacheCreation
		}
		return `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","usage":{` + startUsage + `}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":1000000}}

event: message_stop
data: {"type":"message_stop"}

`
	}

	// vitest's toBeCloseTo(x, 10): |got - want| < 10^-10 / 2.
	closeEnough := func(got, want float64) bool {
		d := got - want
		if d < 0 {
			d = -d
		}
		return d < 5e-11
	}

	t.Run("prices the 1h portion at 2x input and the rest at the 5m rate", func(t *testing.T) {
		m := ai.GetModel("anthropic", "claude-opus-4-8")
		if m == nil {
			t.Fatal("claude-opus-4-8 missing from catalog")
		}
		sse := cacheWrite1hSSE(`"cache_creation":{"ephemeral_5m_input_tokens":600000,"ephemeral_1h_input_tokens":400000}`)
		final := streamAnthropicSSE(t, m, sse)
		if final.Usage.CacheWrite != 1000000 {
			t.Fatalf("cacheWrite = %d, want 1000000", final.Usage.CacheWrite)
		}
		if final.Usage.CacheWrite1h != 400000 {
			t.Fatalf("cacheWrite1h = %d, want 400000", final.Usage.CacheWrite1h)
		}
		// 600k * 6.25/Mtok + 400k * (5*2)/Mtok = 3.75 + 4.0 = 7.75
		if !closeEnough(final.Usage.Cost.CacheWrite, 7.75) {
			t.Fatalf("cost.cacheWrite = %v, want 7.75", final.Usage.Cost.CacheWrite)
		}
	})

	// Regression for #9210 (upstream 667fc3dd3): Vercel AI Gateway sends cache
	// usage in message_delta, not message_start.
	t.Run("prices 1h cache writes reported only in message_delta", func(t *testing.T) {
		m := ai.GetModel("vercel-ai-gateway", "anthropic/claude-haiku-4.5")
		if m == nil {
			t.Fatal("vercel-ai-gateway/anthropic/claude-haiku-4.5 missing from catalog")
		}
		sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","usage":{"input_tokens":0,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":6535,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":6535}}}

event: message_stop
data: {"type":"message_stop"}
`
		final := streamAnthropicSSE(t, m, sse)
		if final.Usage.CacheWrite != 6535 {
			t.Fatalf("cacheWrite = %d, want 6535", final.Usage.CacheWrite)
		}
		if final.Usage.CacheWrite1h != 6535 {
			t.Fatalf("cacheWrite1h = %d, want 6535", final.Usage.CacheWrite1h)
		}
		want := float64(6535) * m.Cost.Input * 2 / 1_000_000
		if !closeEnough(final.Usage.Cost.CacheWrite, want) {
			t.Fatalf("cost.cacheWrite = %v, want %v", final.Usage.Cost.CacheWrite, want)
		}
	})

	t.Run("falls back to the 5m rate when no breakdown is reported", func(t *testing.T) {
		m := ai.GetModel("anthropic", "claude-opus-4-8")
		if m == nil {
			t.Fatal("claude-opus-4-8 missing from catalog")
		}
		final := streamAnthropicSSE(t, m, cacheWrite1hSSE(""))
		if final.Usage.CacheWrite != 1000000 {
			t.Fatalf("cacheWrite = %d, want 1000000", final.Usage.CacheWrite)
		}
		if final.Usage.CacheWrite1h != 0 {
			t.Fatalf("cacheWrite1h = %d, want 0", final.Usage.CacheWrite1h)
		}
		// 1M * 6.25/Mtok = 6.25
		if !closeEnough(final.Usage.Cost.CacheWrite, 6.25) {
			t.Fatalf("cost.cacheWrite = %v, want 6.25", final.Usage.Cost.CacheWrite)
		}
	})
}

// streamAnthropicSSE runs the anthropic stream against a test server returning a
// fixed SSE body and returns the final assistant message.
func streamAnthropicSSE(t *testing.T, model *ai.Model, sse string) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	defer server.Close()
	clone := *model
	clone.BaseURL = server.URL
	return StreamAnthropic(context.Background(), &clone, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
}

// mustBuildAnthropicParams builds a Messages request body, failing the test on
// the errors constrained sampling can raise.
func mustBuildAnthropicParams(t *testing.T, model *ai.Model, req ai.Context, oauth bool, opts *AnthropicOptions) map[string]any {
	t.Helper()
	params, err := buildAnthropicParams(model, ai.NormalizeContext(req), oauth, opts)
	if err != nil {
		t.Fatalf("buildAnthropicParams: %v", err)
	}
	return params
}

// Upstream 24bace27: with compat.supportsStrictTools, a json_schema-constrained
// tool gains `strict: true` and its input_schema becomes the full parameter
// schema with the legacy {type, properties, required} shape layered over it.
// Upstream 7915cdac6 converts the schema first: the input_schema is the STRICT
// conversion (closed object, every property required, non-nullable optionals
// widened to anyOf-with-null), and the legacy fields are read from it — the
// expectations below mirror anthropic-eager-tool-input-compat.test.ts.
func TestAnthropicStrictTools(t *testing.T) {
	model := func(compat string) *ai.Model {
		m := &ai.Model{ID: "claude-x", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
		if compat != "" {
			m.Compat = json.RawMessage(compat)
		}
		return m
	}
	params := ai.Object(ai.Prop("value", ai.String()), ai.Opt("optional", ai.Number()))
	params.Extra = map[string]any{"title": "StrictLookupInput"}
	tool := ai.Tool{
		Name: "rich", Description: "d", Parameters: params,
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{
			Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer,
		},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{tool}}

	firstTool := func(compat string) map[string]any {
		t.Helper()
		body := mustBuildAnthropicParams(t, model(compat), req, false, &AnthropicOptions{})
		tools, ok := body["tools"].([]map[string]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools wrong: %#v", body["tools"])
		}
		return tools[0]
	}

	strictTool := firstTool(`{"supportsStrictTools":true}`)
	if strictTool["strict"] != true {
		t.Fatalf("strict tool must carry strict:true: %#v", strictTool)
	}
	schema, _ := strictTool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("input_schema type: %#v", schema)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("strict input_schema must close the object: %#v", schema)
	}
	if schema["title"] != "StrictLookupInput" {
		t.Fatalf("strict input_schema must spread the full parameter schema: %#v", schema)
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 2 || props["value"] == nil {
		t.Fatalf("converted properties must win: %#v", schema["properties"])
	}
	if optional, _ := json.Marshal(props["optional"]); string(optional) != `{"anyOf":[{"type":"number"},{"type":"null"}]}` {
		t.Fatalf("optional property must be widened to allow null: %s", optional)
	}
	if req, _ := schema["required"].([]string); len(req) != 2 || req[0] != "value" || req[1] != "optional" {
		t.Fatalf("converted required must list every property: %#v", schema["required"])
	}

	plainTool := firstTool("")
	if _, has := plainTool["strict"]; has {
		t.Fatalf("strict must be absent without supportsStrictTools: %#v", plainTool)
	}
	schema, _ = plainTool["input_schema"].(map[string]any)
	if len(schema) != 3 {
		t.Fatalf("non-strict input_schema must stay the legacy 3-key shape: %#v", schema)
	}
}

// Upstream 295cc72b0: Anthropic strict tool use rejects keywords like
// minimum/maximum with a 400 for the whole request, so a "prefer" tool whose
// schema uses one is sent non-strict, and a "require" tool fails naming the
// first offending keyword. Replays testdata/anthropic-strict-keywords, captured
// from pi-ai 0.99.2's anthropic-messages stream.
func TestAnthropicStrictUnsupportedKeywordsMatchPi(t *testing.T) {
	raw, err := os.ReadFile("testdata/anthropic-strict-keywords/strict-keywords-0.99.2.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name       string          `json:"name"`
			Strict     string          `json:"strict"`
			Parameters json.RawMessage `json:"parameters"`
			SentStrict bool            `json:"sentStrict"`
			Error      string          `json:"error"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) == 0 {
		t.Fatal("empty capture")
	}
	model := &ai.Model{ID: "claude-x", Api: ai.APIAnthropicMessages, Provider: "test-anthropic", Input: []string{"text"}, MaxTokens: 100,
		Compat: json.RawMessage(`{"supportsStrictTools":true}`)}
	for _, row := range capture.Rows {
		var params ai.Schema
		if err := json.Unmarshal(row.Parameters, &params); err != nil {
			t.Fatal(err)
		}
		req := ai.Context{
			Messages: []ai.Message{ai.NewUserText("hi", 1)},
			Tools: []ai.Tool{{Name: "lookup", Description: "d", Parameters: &params,
				ConstrainedSampling: &ai.ConstrainedSamplingConfig{
					Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingStrictness(row.Strict),
				}}},
		}
		body, err := buildAnthropicParams(model, ai.NormalizeContext(req), false, &AnthropicOptions{})
		if row.Error != "" {
			if err == nil || err.Error() != row.Error {
				t.Errorf("%s/%s: err = %v, pi = %q", row.Name, row.Strict, err, row.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s: unexpected error %v", row.Name, row.Strict, err)
			continue
		}
		tools, _ := body["tools"].([]map[string]any)
		if got := len(tools) == 1 && tools[0]["strict"] == true; got != row.SentStrict {
			t.Errorf("%s/%s: strict = %v, pi = %v", row.Name, row.Strict, got, row.SentStrict)
		}
	}
}

// Characterization: the keyword check is Anthropic's alone: OpenAI's strict mode still takes a
// bounded integer (upstream 295cc72b0 leaves other providers unchanged).
func TestStrictUnsupportedKeywordsOnlyAnthropic(t *testing.T) {
	lower := 1.0
	tool := ai.Tool{Name: "lookup", Parameters: ai.Object(ai.Prop("n", &ai.Schema{Type: "integer", Minimum: &lower})),
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer}}
	strict, err := resolveJSONSchemaStrictSampling(tool, true, nil)
	if err != nil || !strict {
		t.Fatalf("without a provider check a minimum stays strict, got (%v, %v)", strict, err)
	}
}

// A tool that REQUIRES strict sampling fails the request on a provider without
// strict tools. Message captured verbatim from pi 0.82.0.
func TestAnthropicStrictToolsRequireFails(t *testing.T) {
	model := &ai.Model{ID: "claude-x", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools: []ai.Tool{{
			Name: "js_require", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.Integer())),
			ConstrainedSampling: &ai.ConstrainedSamplingConfig{
				Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingRequire,
			},
		}},
	}
	_, err := buildAnthropicParams(model, ai.NormalizeContext(req), false, &AnthropicOptions{})
	assertErrString(t, err, `Tool "js_require" requires JSON-schema constrained sampling, but strict tools are unsupported.`)
}

// Upstream 59ad3dea: content_block_start may already carry the block's first
// chunk (text, or thinking + signature). It must be kept and the following
// deltas appended to it, not used to replace it.
func TestAnthropicPreservesContentBlockStartContent(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_initial_content","usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Initial text"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" plus delta"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"Initial thinking","signature":"initial signature"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":" plus delta"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":" plus delta"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("Say hello.", 1)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL

	stream := StreamAnthropic(context.Background(), model, ai.NormalizeContext(req),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}})

	// The start events themselves must already expose the seeded content: the
	// builder is seeded before the partial snapshot is taken.
	var textStart, thinkingStart string
	for ev := range stream.Events() {
		switch ev.Type {
		case ai.EventTextStart:
			if c, ok := ev.Partial.Content[ev.ContentIndex].(ai.TextContent); ok {
				textStart = c.Text
			}
		case ai.EventThinkingStart:
			if c, ok := ev.Partial.Content[ev.ContentIndex].(ai.ThinkingContent); ok {
				thinkingStart = c.Thinking + "|" + c.ThinkingSignature
			}
		}
	}
	final := stream.Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if textStart != "Initial text" {
		t.Fatalf("text_start partial lost initial content: %q", textStart)
	}
	if thinkingStart != "Initial thinking|initial signature" {
		t.Fatalf("thinking_start partial lost initial content: %q", thinkingStart)
	}
	if len(final.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %#v", final.Content)
	}
	text, ok := final.Content[0].(ai.TextContent)
	if !ok || text.Text != "Initial text plus delta" {
		t.Fatalf("text block wrong: %#v", final.Content[0])
	}
	think, ok := final.Content[1].(ai.ThinkingContent)
	if !ok || think.Thinking != "Initial thinking plus delta" {
		t.Fatalf("thinking block wrong: %#v", final.Content[1])
	}
	if think.ThinkingSignature != "initial signature plus delta" {
		t.Fatalf("thinking signature wrong: %q", think.ThinkingSignature)
	}
}

// The anthropic-messages user-agent surface (pi 9d2ec7ffa's kimi-coding force,
// reversed into a plain default by 87af49dec) is pinned in
// pi_user_agent_wire_test.go alongside the other adapters.

// A shape allowedFallbackModels cannot read must not take the boolean compat
// flags down with it — getAnthropicCompat resolves every compat key on its own
// (see compatOverrides), so this key's failure costs only this key.
//
// The "generated shape" case is also the regen tripwire for the queued catalog
// update (upstream ed867e909): the generator now emits each permitted fallback as
// `{provider, model, cost}`. A decoder still expecting either bare strings or the
// older `{model, cost}` objects would leave the slice empty, which silently turns
// the whole feature off — no `fallbacks` field, no beta header, no fallback
// pricing — with no error anywhere. The sibling boolean key is here so the test
// also proves the whole blob decodes.
func TestAnthropicCompatSurvivesMalformedFallbackTargets(t *testing.T) {
	model := func(compat string) *ai.Model {
		return &ai.Model{
			ID: "claude-opus-5", Api: ai.APIAnthropicMessages, Provider: "anthropic",
			Compat: []byte(compat),
		}
	}

	t.Run("legacy string targets", func(t *testing.T) {
		c := getAnthropicCompat(model(`{"supportsTemperature":false,"allowedFallbackModels":["claude-opus-4-8"]}`))
		if c.supportsTemperature {
			t.Fatalf("supportsTemperature = true, want false")
		}
		if len(c.allowedFallbackModels) != 0 {
			t.Fatalf("want the unreadable field to yield nothing, got %+v", c.allowedFallbackModels)
		}
	})

	t.Run("generated shape decodes whole", func(t *testing.T) {
		c := getAnthropicCompat(model(`{"supportsTemperature":false,"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}]}`))
		if c.supportsTemperature {
			t.Fatalf("supportsTemperature = true, want false")
		}
		if len(c.allowedFallbackModels) != 1 {
			t.Fatalf("want one permitted fallback model, got %+v", c.allowedFallbackModels)
		}
		fb := c.allowedFallbackModels[0]
		if fb.Provider != "anthropic" || fb.Model != "claude-opus-4-8" {
			t.Fatalf("want {anthropic claude-opus-4-8}, got {%s %s}", fb.Provider, fb.Model)
		}
		if fb.Cost == nil {
			t.Fatal("want the fallback's local pricing, got nil")
		}
		if c := *fb.Cost; c.Input != 5 || c.Output != 25 || c.CacheRead != 0.5 || c.CacheWrite != 6.25 {
			t.Fatalf("want cost {5 25 0.5 6.25}, got %+v", c)
		}
	})
}
