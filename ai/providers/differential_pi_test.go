package providers

// Differential tests: these assertions are ported directly from pi's own test
// suite (packages/ai/test/*.test.ts). pi's expected values are the ground truth
// for "works like pi" — each test cites the upstream file it mirrors. They run
// against buildOpenAIParams (request construction) and the live request headers,
// exactly like pi inspects its captured payload.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func openAIModel(overrides func(*ai.Model)) *ai.Model {
	m := &ai.Model{
		ID: "gpt-4o-mini", Name: "GPT-4o mini", Api: ai.APIOpenAICompletions, Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Input: []string{"text", "image"}, MaxTokens: 16384,
		Cost: ai.ModelCost{Input: 0.15, Output: 0.6},
	}
	if overrides != nil {
		overrides(m)
	}
	return m
}

func baseReq() ai.Context {
	return ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}
}

func has(m map[string]any, key string) bool { _, ok := m[key]; return ok }

// ---- Mirrors openai-completions-prompt-cache.test.ts ----

func TestDiffPromptCacheKeyForDirectOpenAI(t *testing.T) {
	body := buildOpenAIParams(openAIModel(nil), baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: "session-123"}})
	if body["prompt_cache_key"] != "session-123" {
		t.Fatalf("prompt_cache_key = %v, want session-123", body["prompt_cache_key"])
	}
	if has(body, "prompt_cache_retention") {
		t.Fatalf("prompt_cache_retention should be undefined for short retention, got %v", body["prompt_cache_retention"])
	}
}

func TestDiffPromptCacheRetentionLong(t *testing.T) {
	body := buildOpenAIParams(openAIModel(nil), baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: "session-456", CacheRetention: ai.CacheLong}})
	if body["prompt_cache_key"] != "session-456" {
		t.Fatalf("prompt_cache_key = %v", body["prompt_cache_key"])
	}
	if body["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention = %v, want 24h", body["prompt_cache_retention"])
	}
}

func TestDiffPromptCacheKeyClampedTo64(t *testing.T) {
	body := buildOpenAIParams(openAIModel(nil), baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: strings.Repeat("x", 67)}})
	if body["prompt_cache_key"] != strings.Repeat("x", 64) {
		t.Fatalf("prompt_cache_key not clamped to 64: len=%d", len(body["prompt_cache_key"].(string)))
	}
}

func TestDiffPromptCacheOmittedWhenNone(t *testing.T) {
	body := buildOpenAIParams(openAIModel(nil), baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: "session-789", CacheRetention: ai.CacheNone}})
	if has(body, "prompt_cache_key") || has(body, "prompt_cache_retention") {
		t.Fatalf("prompt cache fields should be omitted for none: %v", body)
	}
}

func TestDiffPromptCacheOmittedForNonOpenAIWithoutLongRetention(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.BaseURL = "https://proxy.example.com/v1"
		m.Compat = json.RawMessage(`{"supportsLongCacheRetention":false}`)
	})
	body := buildOpenAIParams(model, baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: "session-proxy", CacheRetention: ai.CacheLong}})
	if has(body, "prompt_cache_key") || has(body, "prompt_cache_retention") {
		t.Fatalf("prompt cache fields should be omitted for non-OpenAI w/o long retention: %v", body)
	}
}

func TestDiffPICacheRetentionEnv(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "long")
	body := buildOpenAIParams(openAIModel(nil), baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: "session-env"}})
	if body["prompt_cache_key"] != "session-env" || body["prompt_cache_retention"] != "24h" {
		t.Fatalf("PI_CACHE_RETENTION=long not honored: %v / %v", body["prompt_cache_key"], body["prompt_cache_retention"])
	}
}

func TestDiffSessionAffinityHeaders(t *testing.T) {
	// Mirrors the header assertions: when sendSessionAffinityHeaders is enabled
	// and caching is on, session_id / x-client-request-id / x-session-affinity
	// all carry the session id.
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	model := openAIModel(func(m *ai.Model) {
		m.BaseURL = server.URL
		m.Compat = json.RawMessage(`{"sendSessionAffinityHeaders":true}`)
	})
	StreamOpenAICompletions(context.Background(), model, baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "session-affinity"}}).Result()

	for _, h := range []string{"Session_id", "X-Client-Request-Id", "X-Session-Affinity"} {
		if gotHeaders.Get(h) != "session-affinity" {
			t.Fatalf("header %s = %q, want session-affinity", h, gotHeaders.Get(h))
		}
	}
}

func TestDiffSessionAffinityOmittedWhenCacheNone(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) {
		m.BaseURL = server.URL
		m.Compat = json.RawMessage(`{"sendSessionAffinityHeaders":true}`)
	})
	StreamOpenAICompletions(context.Background(), model, baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "s", CacheRetention: ai.CacheNone}}).Result()
	if gotHeaders.Get("Session_id") != "" {
		t.Fatalf("affinity headers should be omitted when cacheRetention=none")
	}
}

// ---- Mirrors openai-completions-empty-tools.test.ts ----

func TestDiffOmitsToolsWhenEmpty(t *testing.T) {
	req := baseReq()
	req.Tools = []ai.Tool{}
	body := buildOpenAIParams(openAIModel(nil), req, &OpenAIOptions{})
	if has(body, "tools") {
		t.Fatalf("tools field must be omitted for empty tools: %v", body["tools"])
	}
}

func TestDiffNoDefaultMaxTokenFields(t *testing.T) {
	// pi: "does not send default max token fields" — neither field unless explicit.
	body := buildOpenAIParams(openAIModel(nil), baseReq(), &OpenAIOptions{})
	if has(body, "max_tokens") || has(body, "max_completion_tokens") {
		t.Fatalf("no max-token field should be sent by default: %v", body)
	}
}

func TestDiffExplicitMaxTokensUsesCompletionField(t *testing.T) {
	mt := 1234
	body := buildOpenAIParams(openAIModel(nil), baseReq(), &OpenAIOptions{StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if has(body, "max_tokens") {
		t.Fatalf("OpenAI must not use max_tokens")
	}
	if v, _ := body["max_completion_tokens"].(int); v != 1234 {
		t.Fatalf("max_completion_tokens = %v, want 1234", body["max_completion_tokens"])
	}
}

// ---- Mirrors openai-completions-tool-choice.test.ts (strict-mode portion) ----

// TestDiffOpenRouterAnthropicCacheControl locks the divergence found by the live
// pi-vs-go request diff: OpenRouter routing an anthropic/ model applies
// Anthropic-style cache_control to the system prompt, last tool, and last
// user/assistant text block.
func TestDiffOpenRouterAnthropicCacheControl(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.ID = "anthropic/claude-x"
		m.Provider = "openrouter"
		m.BaseURL = "https://openrouter.ai/api/v1"
	})
	req := baseReq()
	req.Tools = []ai.Tool{{Name: "t", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.String()))}}
	body := buildOpenAIParams(model, req, &OpenAIOptions{})

	msgs, _ := body["messages"].([]map[string]any)
	// System prompt converted to a block array with cache_control.
	sysBlocks, ok := msgs[0]["content"].([]any)
	if !ok || len(sysBlocks) == 0 {
		t.Fatalf("system content not converted to blocks: %#v", msgs[0]["content"])
	}
	if _, has := sysBlocks[0].(map[string]any)["cache_control"]; !has {
		t.Fatalf("system prompt missing cache_control: %#v", sysBlocks[0])
	}
	// Last tool carries cache_control.
	tools, _ := body["tools"].([]map[string]any)
	if _, has := tools[len(tools)-1]["cache_control"]; !has {
		t.Fatalf("last tool missing cache_control")
	}
	// Non-anthropic OpenRouter model must NOT get cache_control.
	model2 := openAIModel(func(m *ai.Model) {
		m.ID = "meta/llama"
		m.Provider = "openrouter"
		m.BaseURL = "https://openrouter.ai/api/v1"
	})
	body2 := buildOpenAIParams(model2, baseReq(), &OpenAIOptions{})
	m2, _ := body2["messages"].([]map[string]any)
	if _, isArr := m2[0]["content"].([]any); isArr {
		t.Fatalf("non-anthropic OpenRouter model should not get cache_control blocks")
	}
}

func TestDiffOmitsStrictWhenCompatDisables(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		// moonshot disables strict mode in pi's detectCompat.
		m.Provider = "moonshotai"
		m.BaseURL = "https://api.moonshot.ai/v1"
	})
	req := baseReq()
	req.Tools = []ai.Tool{{Name: "t", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.String()))}}
	body := buildOpenAIParams(model, req, &OpenAIOptions{})
	tools, _ := body["tools"].([]map[string]any)
	if len(tools) == 0 {
		t.Fatal("expected a tool")
	}
	fn, _ := tools[0]["function"].(map[string]any)
	if has(fn, "strict") {
		t.Fatalf("strict must be omitted when compat disables strict mode: %v", fn["strict"])
	}
}

// ---- Detection matrix (mirrors pi detectCompat, openai-completions.ts:1075-1155) ----

func strPtr(s string) *string { return &s }

// reqWithTool returns baseReq with a single tool.
func reqWithTool() ai.Context {
	r := baseReq()
	r.Tools = []ai.Tool{{Name: "t", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.String()))}}
	return r
}

func TestDiffDetectZai(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.ID = "glm-4.6"
		m.Provider = "zai"
		m.BaseURL = "https://api.z.ai/api/coding/paas/v4"
		m.Reasoning = true
	})
	c := getOpenAICompat(model)
	// pi: zai is nonStandard -> supportsStore=false, supportsReasoningEffort=false,
	// thinkingFormat=zai, supportsLongCacheRetention=true (zai not excluded),
	// maxTokensField=max_completion_tokens (zai not in useMaxTokens).
	if c.SupportsStore {
		t.Fatalf("zai supportsStore should be false")
	}
	if c.SupportsReasoningEffort {
		t.Fatalf("zai supportsReasoningEffort should be false")
	}
	if c.ThinkingFormat != "zai" {
		t.Fatalf("zai thinkingFormat = %q, want zai", c.ThinkingFormat)
	}
	if !c.SupportsLongCacheRetention {
		t.Fatalf("zai supportsLongCacheRetention should be true")
	}
	if c.MaxTokensField != "max_completion_tokens" {
		t.Fatalf("zai maxTokensField = %q, want max_completion_tokens", c.MaxTokensField)
	}

	// Request shape (pi 64b51efb): thinking: {type:"enabled"|"disabled"} driven
	// by whether reasoning was requested; no reasoning_effort, store omitted.
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if thinking, _ := body["thinking"].(map[string]any); thinking == nil || thinking["type"] != "enabled" {
		t.Fatalf(`zai on: thinking = %v, want {"type":"enabled"}`, body["thinking"])
	}
	if has(body, "enable_thinking") {
		t.Fatalf("zai must not send enable_thinking, got %v", body["enable_thinking"])
	}
	if has(body, "reasoning_effort") {
		t.Fatalf("zai must not send reasoning_effort")
	}
	if has(body, "store") {
		t.Fatalf("zai (supportsStore=false) must not send store")
	}

	// No reasoning effort -> thinking: {type:"disabled"}.
	bodyOff := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	if thinking, _ := bodyOff["thinking"].(map[string]any); thinking == nil || thinking["type"] != "disabled" {
		t.Fatalf(`zai off: thinking = %v, want {"type":"disabled"}`, bodyOff["thinking"])
	}
	if has(bodyOff, "enable_thinking") {
		t.Fatalf("zai off must not send enable_thinking, got %v", bodyOff["enable_thinking"])
	}
}

func TestDiffZaiToolStream(t *testing.T) {
	// zaiToolStream is detected false by default; set via compat override and assert
	// tool_stream is sent only when tools are present (pi openai-completions.ts:540-542).
	model := openAIModel(func(m *ai.Model) {
		m.ID = "glm-4.6"
		m.Provider = "zai"
		m.BaseURL = "https://api.z.ai/api/coding/paas/v4"
		m.Compat = json.RawMessage(`{"zaiToolStream":true}`)
	})
	if !getOpenAICompat(model).ZaiToolStream {
		t.Fatalf("zaiToolStream override not applied")
	}
	body := buildOpenAIParams(model, reqWithTool(), &OpenAIOptions{})
	if body["tool_stream"] != true {
		t.Fatalf("expected tool_stream:true when zaiToolStream + tools, got %v", body["tool_stream"])
	}
	// No tools -> no tool_stream.
	body2 := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	if has(body2, "tool_stream") {
		t.Fatalf("tool_stream must be omitted when no tools present")
	}
}

func TestDiffDetectAntLing(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.ID = "ling-1t"
		m.Provider = "ant-ling"
		m.BaseURL = "https://api.ant-ling.com/v1"
		m.Reasoning = true
		m.ThinkingLevelMap = ai.ThinkingLevelMap{"high": strPtr("high")}
	})
	c := getOpenAICompat(model)
	// pi: ant-ling -> thinkingFormat=ant-ling, maxTokensField=max_tokens (useMaxTokens),
	// supportsReasoningEffort=false, supportsStrictMode=true (not excluded),
	// supportsLongCacheRetention=false.
	if c.ThinkingFormat != "ant-ling" {
		t.Fatalf("ant-ling thinkingFormat = %q", c.ThinkingFormat)
	}
	if c.MaxTokensField != "max_tokens" {
		t.Fatalf("ant-ling maxTokensField = %q, want max_tokens", c.MaxTokensField)
	}
	if c.SupportsReasoningEffort {
		t.Fatalf("ant-ling supportsReasoningEffort should be false")
	}
	if !c.SupportsStrictMode {
		t.Fatalf("ant-ling supportsStrictMode should be true")
	}
	if c.SupportsLongCacheRetention {
		t.Fatalf("ant-ling supportsLongCacheRetention should be false")
	}
	// Request shape: ant-ling sends reasoning:{effort} from thinkingLevelMap, max_tokens.
	mt := 500
	body := buildOpenAIParams(model, reqWithTool(),
		&OpenAIOptions{ReasoningEffort: "high", StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	r, ok := body["reasoning"].(map[string]any)
	if !ok || r["effort"] != "high" {
		t.Fatalf("ant-ling reasoning = %v, want {effort:high}", body["reasoning"])
	}
	if v, _ := body["max_tokens"].(int); v != 500 {
		t.Fatalf("ant-ling max_tokens = %v, want 500", body["max_tokens"])
	}
	// supportsStrictMode=true => strict:false present on the tool.
	tools, _ := body["tools"].([]map[string]any)
	fn, _ := tools[0]["function"].(map[string]any)
	if v, ok := fn["strict"].(bool); !ok || v != false {
		t.Fatalf("ant-ling tool should carry strict:false, got %v", fn["strict"])
	}
}

func TestDiffDetectGrok(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.ID = "grok-4"
		m.Provider = "xai"
		m.BaseURL = "https://api.x.ai/v1"
		m.Reasoning = true
	})
	c := getOpenAICompat(model)
	// pi: grok (xai) -> supportsReasoningEffort=false, supportsStore=false,
	// thinkingFormat=openai (default), supportsDeveloperRole=false (nonStandard).
	if c.SupportsReasoningEffort {
		t.Fatalf("grok supportsReasoningEffort should be false")
	}
	if c.SupportsStore {
		t.Fatalf("grok supportsStore should be false")
	}
	if c.SupportsDeveloperRole {
		t.Fatalf("grok supportsDeveloperRole should be false")
	}
	// Request shape: openai thinkingFormat but supportsReasoningEffort=false => no reasoning_effort.
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if has(body, "reasoning_effort") {
		t.Fatalf("grok must not send reasoning_effort (supportsReasoningEffort=false)")
	}
}

func TestDiffDetectCloudflareWorkersAI(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "cloudflare-workers-ai"
		m.BaseURL = "https://api.cloudflare.com/client/v4/accounts/x/ai/v1"
	})
	c := getOpenAICompat(model)
	// pi: workers-ai is NOT in useMaxTokens -> max_completion_tokens;
	// supportsLongCacheRetention=false; supportsStore=false; supportsStrictMode=true.
	if c.MaxTokensField != "max_completion_tokens" {
		t.Fatalf("cloudflare-workers-ai maxTokensField = %q, want max_completion_tokens", c.MaxTokensField)
	}
	if c.SupportsLongCacheRetention {
		t.Fatalf("cloudflare-workers-ai supportsLongCacheRetention should be false")
	}
	if c.SupportsStore {
		t.Fatalf("cloudflare-workers-ai supportsStore should be false")
	}
	if !c.SupportsStrictMode {
		t.Fatalf("cloudflare-workers-ai supportsStrictMode should be true")
	}
}

func TestDiffDetectCloudflareAiGateway(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "cloudflare-ai-gateway"
		m.BaseURL = "https://gateway.ai.cloudflare.com/v1/acct/gw/compat"
	})
	c := getOpenAICompat(model)
	// pi: ai-gateway IS in useMaxTokens -> max_tokens; supportsStrictMode=false;
	// supportsReasoningEffort=false; supportsLongCacheRetention=false.
	if c.MaxTokensField != "max_tokens" {
		t.Fatalf("cloudflare-ai-gateway maxTokensField = %q, want max_tokens", c.MaxTokensField)
	}
	if c.SupportsStrictMode {
		t.Fatalf("cloudflare-ai-gateway supportsStrictMode should be false")
	}
	if c.SupportsReasoningEffort {
		t.Fatalf("cloudflare-ai-gateway supportsReasoningEffort should be false")
	}
	if c.SupportsLongCacheRetention {
		t.Fatalf("cloudflare-ai-gateway supportsLongCacheRetention should be false")
	}
}

func TestDiffCloudflareAiGatewayAuthHeader(t *testing.T) {
	// pi createClient: cloudflare-ai-gateway puts the apiKey in cf-aig-authorization
	// and does NOT set the default Authorization Bearer.
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "cloudflare-ai-gateway"
		m.BaseURL = server.URL
	})
	StreamOpenAICompletions(context.Background(), model, baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "cf-key"}}).Result()
	if gotHeaders.Get("cf-aig-authorization") != "Bearer cf-key" {
		t.Fatalf("cf-aig-authorization = %q, want Bearer cf-key", gotHeaders.Get("cf-aig-authorization"))
	}
	if gotHeaders.Get("Authorization") != "" {
		t.Fatalf("default Authorization should be omitted for cloudflare-ai-gateway, got %q", gotHeaders.Get("Authorization"))
	}
}

func TestDiffDetectOpenRouterDeveloperRoleMatrix(t *testing.T) {
	// pi: isOpenRouterDeveloperRoleModel => anthropic/ or openai/ prefixed models
	// get supportsDeveloperRole=true; other OpenRouter models get false.
	cases := []struct {
		id   string
		want bool
	}{
		{"openai/gpt-5", true},
		{"anthropic/claude-x", true},
		{"meta/llama", false},
		{"google/gemini", false},
	}
	for _, tc := range cases {
		model := openAIModel(func(m *ai.Model) {
			m.ID = tc.id
			m.Provider = "openrouter"
			m.BaseURL = "https://openrouter.ai/api/v1"
			m.Reasoning = true
		})
		c := getOpenAICompat(model)
		if c.SupportsDeveloperRole != tc.want {
			t.Fatalf("openrouter %s supportsDeveloperRole = %v, want %v", tc.id, c.SupportsDeveloperRole, tc.want)
		}
		body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
		msgs, _ := body["messages"].([]map[string]any)
		role, _ := msgs[0]["role"].(string)
		wantRole := "system"
		if tc.want {
			wantRole = "developer"
		}
		if role != wantRole {
			t.Fatalf("openrouter %s system role = %q, want %q", tc.id, role, wantRole)
		}
	}
}

func TestDiffDeveloperRoleMatrixDirectProviders(t *testing.T) {
	// pi supportsDeveloperRole: !isNonStandard && !isOpenRouter (for non-prefixed).
	cases := []struct {
		provider string
		baseURL  string
		want     bool
	}{
		{"openai", "https://api.openai.com/v1", true},
		{"deepseek", "https://api.deepseek.com/v1", false}, // nonStandard
		{"xai", "https://api.x.ai/v1", false},              // nonStandard
		{"together", "https://api.together.ai/v1", false},  // nonStandard
	}
	for _, tc := range cases {
		model := openAIModel(func(m *ai.Model) {
			m.Provider = tc.provider
			m.BaseURL = tc.baseURL
			m.Reasoning = true
		})
		if got := getOpenAICompat(model).SupportsDeveloperRole; got != tc.want {
			t.Fatalf("%s supportsDeveloperRole = %v, want %v", tc.provider, got, tc.want)
		}
	}
}

func TestDiffStringThinkingFormat(t *testing.T) {
	// string-thinking format: thinking sent as a bare string; off tri-state via
	// thinkingLevelMap (pi openai-completions.ts:595-601).
	model := openAIModel(func(m *ai.Model) {
		m.ID = "custom"
		m.Provider = "custom"
		m.BaseURL = "https://proxy.example.com/v1"
		m.Reasoning = true
		m.ThinkingLevelMap = ai.ThinkingLevelMap{"high": strPtr("deep"), "off": strPtr("none")}
		m.Compat = json.RawMessage(`{"thinkingFormat":"string-thinking"}`)
	})
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if body["thinking"] != "deep" {
		t.Fatalf("string-thinking on: thinking = %v, want deep", body["thinking"])
	}
	// off -> send mapped off string.
	bodyOff := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	if bodyOff["thinking"] != "none" {
		t.Fatalf("string-thinking off: thinking = %v, want none", bodyOff["thinking"])
	}
	// off present-null -> omit.
	model.ThinkingLevelMap = ai.ThinkingLevelMap{"off": nil}
	bodyNull := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	if has(bodyNull, "thinking") {
		t.Fatalf("string-thinking off=null should omit thinking, got %v", bodyNull["thinking"])
	}
}

func TestDiffDeepseekThinkingOffGate(t *testing.T) {
	// deepseek format (pi 0369bdb8 / #5760): with an effort, thinking:{enabled}.
	// With no effort, thinking:{disabled} UNLESS thinkingLevelMap.off is present-
	// null, in which case the thinking key is omitted entirely (always-thinking
	// models like Kimi K2.7 Code reject a disabled payload). The data (kimi-k2.7
	// off:null) is post-0.79.4 and deferred; this pins the provider mechanic.
	mk := func(tm ai.ThinkingLevelMap) *ai.Model {
		return openAIModel(func(m *ai.Model) {
			m.ID = "kimi-like"
			m.Provider = "moonshotai-cn"
			m.BaseURL = "https://proxy.example.com/v1"
			m.Reasoning = true
			m.ThinkingLevelMap = tm
			m.Compat = json.RawMessage(`{"thinkingFormat":"deepseek"}`)
		})
	}
	// on -> thinking:{type:enabled}.
	bOn := buildOpenAIParams(mk(nil), baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if tm, _ := bOn["thinking"].(map[string]any); tm["type"] != "enabled" {
		t.Fatalf("deepseek on: thinking = %v, want {type:enabled}", bOn["thinking"])
	}
	// off, off absent -> thinking:{type:disabled}.
	bAbsent := buildOpenAIParams(mk(nil), baseReq(), &OpenAIOptions{})
	if tm, _ := bAbsent["thinking"].(map[string]any); tm["type"] != "disabled" {
		t.Fatalf("deepseek off (off absent): thinking = %v, want {type:disabled}", bAbsent["thinking"])
	}
	// off, off present-null -> omit thinking entirely.
	bNull := buildOpenAIParams(mk(ai.ThinkingLevelMap{"off": nil}), baseReq(), &OpenAIOptions{})
	if has(bNull, "thinking") {
		t.Fatalf("deepseek off (off:null) should omit thinking, got %v", bNull["thinking"])
	}
	// off, off mapped to a string -> still send {type:disabled} (off !== null).
	bStr := buildOpenAIParams(mk(ai.ThinkingLevelMap{"off": strPtr("none")}), baseReq(), &OpenAIOptions{})
	if tm, _ := bStr["thinking"].(map[string]any); tm["type"] != "disabled" {
		t.Fatalf("deepseek off (off mapped non-null): thinking = %v, want {type:disabled}", bStr["thinking"])
	}
}

// TestDeepseekDisabledThinkingGateLive drives the deepseek always-thinking gate
// (0369bdb8 / #5760) end-to-end through the catalog-resolved Kimi K2.7 Code model.
// The off:null data landed in npm 0.79.6 (it was deferred when the logic was
// ported), so this replaces the former TestDeepseekCatalogNoOffNull tripwire with
// a live gate, mirroring TestFable5DisabledThinkingGateLive. If a future regen
// drops off:null, the first assertion fails — the signal to re-confirm intent.
func TestDeepseekDisabledThinkingGateLive(t *testing.T) {
	m := ai.GetModel("moonshotai", "kimi-k2.7-code")
	if m == nil {
		t.Fatal("moonshotai/kimi-k2.7-code missing from catalog")
	}
	if got := getOpenAICompat(m).ThinkingFormat; got != "deepseek" {
		t.Fatalf("expected kimi-k2.7-code thinkingFormat=deepseek, got %q", got)
	}
	off, present := m.ThinkingLevelMap["off"]
	if !present || off != nil {
		t.Fatalf("expected catalog kimi-k2.7-code to carry off:null (gate live); got present=%v val=%v — "+
			"if upstream dropped off:null, re-confirm the disabled-thinking gate before changing this", present, off)
	}
	// No effort + off:null -> omit the thinking key entirely (always-thinking model
	// rejects a disabled payload).
	body := buildOpenAIParams(m, baseReq(), &OpenAIOptions{})
	if has(body, "thinking") {
		t.Fatalf("catalog kimi-k2.7-code with thinking off must omit the thinking key, got %v", body["thinking"])
	}
}

func TestDiffQwenThinkingFormat(t *testing.T) {
	// qwen format: enable_thinking boolean reflecting whether reasoning was requested.
	model := openAIModel(func(m *ai.Model) {
		m.ID = "qwen3"
		m.Provider = "custom"
		m.BaseURL = "https://proxy.example.com/v1"
		m.Reasoning = true
		m.Compat = json.RawMessage(`{"thinkingFormat":"qwen"}`)
	})
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if body["enable_thinking"] != true {
		t.Fatalf("qwen on: enable_thinking = %v, want true", body["enable_thinking"])
	}
	bodyOff := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	if bodyOff["enable_thinking"] != false {
		t.Fatalf("qwen off: enable_thinking = %v, want false", bodyOff["enable_thinking"])
	}
}

func TestDiffOpenAIOffTriState(t *testing.T) {
	// Default openai thinkingFormat off tri-state (pi openai-completions.ts:605-609).
	mk := func(tm ai.ThinkingLevelMap) *ai.Model {
		return openAIModel(func(m *ai.Model) {
			m.ID = "gpt-5"
			m.Reasoning = true
			m.ThinkingLevelMap = tm
		})
	}
	// off mapped to a string -> reasoning_effort = that string.
	b1 := buildOpenAIParams(mk(ai.ThinkingLevelMap{"off": strPtr("minimal")}), baseReq(), &OpenAIOptions{})
	if b1["reasoning_effort"] != "minimal" {
		t.Fatalf("off=minimal: reasoning_effort = %v, want minimal", b1["reasoning_effort"])
	}
	// off present-null -> omit (no reasoning_effort).
	b2 := buildOpenAIParams(mk(ai.ThinkingLevelMap{"off": nil}), baseReq(), &OpenAIOptions{})
	if has(b2, "reasoning_effort") {
		t.Fatalf("off=null should omit reasoning_effort, got %v", b2["reasoning_effort"])
	}
	// off absent -> default openai branch sends nothing (offEffortValue absent).
	b3 := buildOpenAIParams(mk(nil), baseReq(), &OpenAIOptions{})
	if has(b3, "reasoning_effort") {
		t.Fatalf("off absent should omit reasoning_effort in default openai branch, got %v", b3["reasoning_effort"])
	}
}

// ---- New behaviors (pi openai-completions.ts:538-626, 884-942) ----

func TestDiffEmptyToolsWithToolHistory(t *testing.T) {
	// hasToolHistory: when the conversation already has tool calls/results and no
	// tools are supplied, pi sends tools:[] (breaks Anthropic-via-proxy otherwise).
	req := baseReq()
	req.Messages = append(req.Messages,
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ToolCall{ID: "c1", Name: "f", Arguments: map[string]any{}}},
			Provider:   "openai",
			Api:        ai.APIOpenAICompletions,
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	)
	body := buildOpenAIParams(openAIModel(nil), req, &OpenAIOptions{})
	tools, ok := body["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools should be present (empty array) when conversation has tool history, got %T %v", body["tools"], body["tools"])
	}
	if len(tools) != 0 {
		t.Fatalf("tools should be an empty array, got %v", tools)
	}
}

func TestDiffNoEmptyToolsWithoutHistory(t *testing.T) {
	// No tool history and no tools -> tools omitted entirely.
	body := buildOpenAIParams(openAIModel(nil), baseReq(), &OpenAIOptions{})
	if has(body, "tools") {
		t.Fatalf("tools must be omitted with no tools and no history, got %v", body["tools"])
	}
}

func TestDiffToolChoice(t *testing.T) {
	body := buildOpenAIParams(openAIModel(nil), reqWithTool(), &OpenAIOptions{ToolChoice: "required"})
	if body["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", body["tool_choice"])
	}
	// Object form.
	tc := map[string]any{"type": "function", "function": map[string]any{"name": "t"}}
	body2 := buildOpenAIParams(openAIModel(nil), reqWithTool(), &OpenAIOptions{ToolChoice: tc})
	got, _ := body2["tool_choice"].(map[string]any)
	if got == nil || got["type"] != "function" {
		t.Fatalf("tool_choice object not plumbed: %v", body2["tool_choice"])
	}
	// Omitted when not set.
	body3 := buildOpenAIParams(openAIModel(nil), reqWithTool(), &OpenAIOptions{})
	if has(body3, "tool_choice") {
		t.Fatalf("tool_choice should be omitted when unset")
	}
}

func TestDiffRequiresToolResultName(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(`{"requiresToolResultName":true}`)
	})
	req := baseReq()
	req.Messages = append(req.Messages,
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "search", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "search", Content: ai.ContentList{ai.TextContent{Text: "r"}}},
	)
	body := buildOpenAIParams(model, req, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	var toolMsg map[string]any
	for _, m := range msgs {
		if m["role"] == "tool" {
			toolMsg = m
		}
	}
	if toolMsg == nil || toolMsg["name"] != "search" {
		t.Fatalf("tool result should carry name=search, got %v", toolMsg)
	}
}

func TestDiffRequiresAssistantAfterToolResult(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(`{"requiresAssistantAfterToolResult":true}`)
	})
	req := baseReq()
	req.Messages = append(req.Messages,
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "f", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "r"}}},
		ai.NewUserText("next", 9),
	)
	body := buildOpenAIParams(model, req, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	// Find the tool message; the message right after it must be a synthetic assistant.
	bridged := false
	for i, m := range msgs {
		if m["role"] == "tool" && i+1 < len(msgs) {
			if msgs[i+1]["role"] == "assistant" && msgs[i+1]["content"] == "I have processed the tool results." {
				bridged = true
			}
		}
	}
	if !bridged {
		t.Fatalf("expected synthetic assistant bridge after tool result before user message: %v", msgs)
	}
}

func TestDiffRequiresThinkingAsText(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Reasoning = true
		m.Compat = json.RawMessage(`{"requiresThinkingAsText":true}`)
	})
	req := baseReq()
	req.Messages = append(req.Messages,
		ai.AssistantMessage{
			Content:  ai.ContentList{ai.ThinkingContent{Thinking: "let me think", ThinkingSignature: "sig"}, ai.TextContent{Text: "answer"}},
			Provider: "openai", Api: ai.APIOpenAICompletions, Model: "gpt-4o-mini",
			StopReason: ai.StopStop,
		},
		ai.NewUserText("again", 9),
	)
	body := buildOpenAIParams(model, req, &OpenAIOptions{ReasoningEffort: "high"})
	msgs, _ := body["messages"].([]map[string]any)
	var asst map[string]any
	for _, m := range msgs {
		if m["role"] == "assistant" {
			asst = m
		}
	}
	blocks, ok := asst["content"].([]any)
	if !ok || len(blocks) < 2 {
		t.Fatalf("requiresThinkingAsText should emit content blocks, got %#v", asst["content"])
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "let me think" {
		t.Fatalf("first block should be thinking-as-text, got %v", first)
	}
}

func TestDiffReasoningDetailsRoundTrip(t *testing.T) {
	// reasoning_details: encrypted thoughtSignature on a tool call is parsed back
	// onto the assistant message (pi openai-completions.ts:884-896).
	model := openAIModel(nil)
	sig := `{"type":"reasoning.encrypted","id":"c1","data":"abc"}`
	req := baseReq()
	req.Messages = append(req.Messages,
		ai.AssistantMessage{
			Content: ai.ContentList{
				ai.ToolCall{ID: "c1", Name: "f", Arguments: map[string]any{}, ThoughtSignature: sig},
			},
			Provider: "openai", Api: ai.APIOpenAICompletions, Model: "gpt-4o-mini",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "r"}}},
	)
	body := buildOpenAIParams(model, req, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	var asst map[string]any
	for _, m := range msgs {
		if _, ok := m["tool_calls"]; ok {
			asst = m
		}
	}
	rd, ok := asst["reasoning_details"].([]any)
	if !ok || len(rd) != 1 {
		t.Fatalf("expected reasoning_details with 1 entry, got %#v", asst["reasoning_details"])
	}
	d, _ := rd[0].(map[string]any)
	if d["type"] != "reasoning.encrypted" || d["id"] != "c1" {
		t.Fatalf("reasoning_details entry not round-tripped: %v", d)
	}
}

func TestDiffToolResultImagesEmitUserMessage(t *testing.T) {
	// Image-bearing tool results emit a synthetic user message with image_url parts
	// (pi openai-completions.ts:945-979). Vision model required.
	model := openAIModel(nil) // Input includes "image"
	req := baseReq()
	req.Messages = append(req.Messages,
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "f", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse},
		ai.ToolResultMessage{
			ToolCallID: "c1", ToolName: "f",
			Content: ai.ContentList{ai.ImageContent{MimeType: "image/png", Data: "BASE64"}},
		},
	)
	body := buildOpenAIParams(model, req, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	// tool message content is the placeholder, followed by a user message with image.
	var toolMsg, userImgMsg map[string]any
	for _, m := range msgs {
		if m["role"] == "tool" {
			toolMsg = m
		}
		if m["role"] == "user" {
			if _, ok := m["content"].([]any); ok {
				userImgMsg = m
			}
		}
	}
	if toolMsg == nil || toolMsg["content"] != "(see attached image)" {
		t.Fatalf("image-only tool result should use placeholder content, got %v", toolMsg)
	}
	if userImgMsg == nil {
		t.Fatalf("expected a user message carrying the tool-result image")
	}
	parts, _ := userImgMsg["content"].([]any)
	foundImg := false
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["type"] == "image_url" {
			foundImg = true
		}
	}
	if !foundImg {
		t.Fatalf("user image message missing image_url part: %v", parts)
	}
}

func TestDiffOpenRouterRouting(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "openrouter"
		m.BaseURL = "https://openrouter.ai/api/v1"
		m.Compat = json.RawMessage(`{"openRouterRouting":{"order":["anthropic","openai"]}}`)
	})
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	prov, ok := body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("expected provider routing block, got %v", body["provider"])
	}
	order, _ := prov["order"].([]any)
	if len(order) != 2 || order[0] != "anthropic" {
		t.Fatalf("provider order not plumbed: %v", prov)
	}
	// Default (no override) -> no provider block.
	model2 := openAIModel(func(m *ai.Model) {
		m.Provider = "openrouter"
		m.BaseURL = "https://openrouter.ai/api/v1"
	})
	body2 := buildOpenAIParams(model2, baseReq(), &OpenAIOptions{})
	if has(body2, "provider") {
		t.Fatalf("provider block should be omitted with empty openRouterRouting")
	}
}

func TestDiffVercelGatewayRouting(t *testing.T) {
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "vercel"
		m.BaseURL = "https://ai-gateway.vercel.sh/v1"
		m.Compat = json.RawMessage(`{"vercelGatewayRouting":{"only":["openai"],"order":["openai","anthropic"]}}`)
	})
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	po, ok := body["providerOptions"].(map[string]any)
	if !ok {
		t.Fatalf("expected providerOptions, got %v", body["providerOptions"])
	}
	gw, _ := po["gateway"].(map[string]any)
	only, _ := gw["only"].([]string)
	order, _ := gw["order"].([]string)
	if len(only) != 1 || only[0] != "openai" {
		t.Fatalf("gateway only not plumbed: %v", gw)
	}
	if len(order) != 2 {
		t.Fatalf("gateway order not plumbed: %v", gw)
	}
	// Not a vercel gateway URL -> no providerOptions even with routing set.
	model2 := openAIModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(`{"vercelGatewayRouting":{"only":["openai"]}}`)
	})
	body2 := buildOpenAIParams(model2, baseReq(), &OpenAIOptions{})
	if has(body2, "providerOptions") {
		t.Fatalf("providerOptions should be omitted for non-vercel-gateway baseUrl")
	}
}
