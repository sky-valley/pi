package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{APIKey: "test-key"}}

	final := StreamAnthropic(context.Background(), model, req, &AnthropicOptions{StreamOptions: opts.StreamOptions}).Result()

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

func TestAnthropicProviderErrorOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()
	model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, MaxTokens: 100}
	final := StreamAnthropic(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
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
	StreamAnthropic(context.Background(), model, req, opts).Result()
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-ant-oat-secret"}}
	headers, body := anthropicCapture(t, model, req, opts, anthropicSSE)

	if got := headers.Get("authorization"); got != "Bearer sk-ant-oat-secret" {
		t.Fatalf("authorization header wrong: %q", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must not be set for OAuth: %q", got)
	}
	if got := headers.Get("user-agent"); got != "claude-cli/2.1.75" {
		t.Fatalf("user-agent wrong: %q", got)
	}
	if got := headers.Get("x-app"); got != "cli" {
		t.Fatalf("x-app wrong: %q", got)
	}
	// anthropic-beta must lead with the OAuth betas in pi's exact order.
	beta := headers.Get("anthropic-beta")
	if !strings.HasPrefix(beta, "claude-code-20250219,oauth-2025-04-20") {
		t.Fatalf("oauth betas missing/misordered: %q", beta)
	}
	// Interleaved-thinking beta still appended (default on, not adaptive).
	if !strings.Contains(beta, interleavedThinkingBeta) {
		t.Fatalf("interleaved beta missing: %q", beta)
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "test-key", CacheRetention: ai.CacheLong}}
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
	uc := lastUser["content"].([]any)
	checkCC("messages[last].content[last]", uc[len(uc)-1].(map[string]any))

	// pi does NOT send any extended-cache anthropic-beta header; the 1h TTL is
	// carried entirely by cache_control. Only the interleaved-thinking beta is set.
	beta := headers.Get("anthropic-beta")
	if strings.Contains(beta, "extended-cache") || strings.Contains(beta, "ttl") {
		t.Fatalf("unexpected extended-cache beta header (pi emits none): %q", beta)
	}
	if beta != interleavedThinkingBeta {
		t.Fatalf("expected only interleaved beta, got %q", beta)
	}
}

func TestAnthropicShortCacheRetentionNoTTL(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096,
	}
	req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k", CacheRetention: ai.CacheShort}}
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}
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
		StreamOptions: ai.StreamOptions{APIKey: "k"},
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
	StreamSimpleAnthropic(context.Background(), model, req, opts).Result()

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
	// Use an sk-ant-oat key: pi checks the cloudflare branch BEFORE the OAuth
	// sniff (anthropic.ts:802 vs :848), so no OAuth identity must leak through.
	final := StreamAnthropic(context.Background(), model, req,
		&AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-ant-oat-cfkey"}}).Result()
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
	final := StreamAnthropic(context.Background(), model, req,
		&AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-ant-oat-copilot"}}
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

// --- E3: thinking tri-state ---

func TestAnthropicThinkingOmittedWhenNotProvided(t *testing.T) {
	model := &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, Reasoning: true,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	// Generic registry path: plain StreamOptions -> ThinkingProvided stays false.
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}, ThinkingProvided: true, ThinkingEnabled: false}
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}, ThinkingProvided: true, ThinkingEnabled: false}
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}, ThinkingProvided: true, ThinkingEnabled: false}
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
		StreamOptions:    ai.StreamOptions{APIKey: "k"},
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
	StreamSimpleAnthropic(context.Background(), model, req,
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	thinking, ok := gotBody["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("streamSimple without reasoning must send {type:disabled}, got %v", gotBody["thinking"])
	}
}

// --- E4: session affinity suppressed when cacheRetention is none ---

func TestAnthropicSessionAffinityRetention(t *testing.T) {
	// fireworks auto-enables sendSessionAffinityHeaders.
	mk := func(retention ai.CacheRetention) http.Header {
		model := &ai.Model{
			ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "fireworks",
			Input: []string{"text"}, MaxTokens: 4096,
		}
		req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
		opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{
			APIKey: "k", SessionID: "sess-1", CacheRetention: retention,
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
	opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamAnthropic(context.Background(), model, req, opts).Result()
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
	final := StreamAnthropic(context.Background(), model, req,
		&AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
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
		APIKey: "k",
		OnPayload: func(payload any, m *ai.Model) (any, error) {
			return nil, errors.New("payload veto")
		},
	}}
	final := StreamAnthropic(context.Background(), model, req, opts).Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "payload veto" {
		t.Fatalf("onPayload error must fail the stream: %s / %q", final.StopReason, final.ErrorMessage)
	}
	if requested {
		t.Fatalf("request must not be sent when onPayload errors")
	}
}

// TestFable5DisabledThinkingGateLatency is a TRIPWIRE, not a behavior test.
// Upstream 9ccfcd7c added both the off:null gate (anthropic.ts) and a
// generate-models rule emitting off:null for fable-5 — but never regenerated
// models.generated.ts, and no release ships the data yet. Our catalog (0.79.1)
// faithfully mirrors that: fable-5 has xhigh only, so the gate is latent in
// BOTH codebases. When a future catalog regen adds "off":null to fable-5, this
// test fails: that's the signal the gate goes LIVE — confirm the omit behavior
// end-to-end (TestAnthropicThinkingOffNullOmitsDisabled covers the mechanics),
// then update this assertion to expect the off key.
func TestFable5DisabledThinkingGateLatency(t *testing.T) {
	m := ai.GetModel("anthropic", "claude-fable-5")
	if m == nil {
		t.Fatal("claude-fable-5 missing from catalog")
	}
	if _, present := m.ThinkingLevelMap["off"]; present {
		t.Fatal("catalog now carries off for claude-fable-5 — the disabled-thinking gate just went live; " +
			"verify omit behavior against the new npm build and update this tripwire")
	}
	if v, ok := m.ThinkingLevelMap["xhigh"]; !ok || v == nil || *v != "xhigh" {
		t.Fatalf("fable-5 thinkingLevelMap unexpected: %v", m.ThinkingLevelMap)
	}
}
