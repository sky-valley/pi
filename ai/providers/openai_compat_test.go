package providers

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

func captureOpenAIBody(t *testing.T, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) map[string]any {
	t.Helper()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model.BaseURL = server.URL
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	opts.APIKey = "k"
	StreamSimpleOpenAICompletions(context.Background(), model, ai.NormalizeContext(req), opts).Result()
	return gotBody
}

func TestOpenAICompatReasoningModel(t *testing.T) {
	model := &ai.Model{ID: "gpt-5-mini", Api: ai.APIOpenAICompletions, Provider: "openai", Reasoning: true, MaxTokens: 2048}
	mt := 2048
	body := captureOpenAIBody(t, model,
		ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&ai.SimpleStreamOptions{Reasoning: ai.ThinkingHigh, StreamOptions: ai.StreamOptions{MaxTokens: &mt}})

	// Reasoning OpenAI model: developer role, max_completion_tokens, store:false, reasoning_effort.
	msgs, _ := body["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("expected developer role for reasoning model, got %v", first["role"])
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Fatalf("expected max_completion_tokens, got keys %v", keysOf(body))
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("should not send max_tokens for OpenAI")
	}
	if body["store"] != false {
		t.Fatalf("expected store:false, got %v", body["store"])
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort high, got %v", body["reasoning_effort"])
	}
}

func TestOpenAICompatNonReasoningUsesSystemRole(t *testing.T) {
	model := &ai.Model{ID: "gpt-4.1", Api: ai.APIOpenAICompletions, Provider: "openai", Reasoning: false, MaxTokens: 2048}
	body := captureOpenAIBody(t, model,
		ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}, nil)
	msgs, _ := body["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("expected system role for non-reasoning model, got %v", first["role"])
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("non-reasoning model must not send reasoning_effort")
	}
}

func TestOpenAICompatTogetherUsesMaxTokensAndReasoningObject(t *testing.T) {
	model := &ai.Model{ID: "x", Api: ai.APIOpenAICompletions, Provider: "together", Reasoning: true, MaxTokens: 1000}
	mt := 1000
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		// Match pi: a max-token field is only sent when explicitly requested; this
		// test pins the *field choice* (max_tokens vs max_completion_tokens).
		&ai.SimpleStreamOptions{Reasoning: ai.ThinkingMedium, StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if _, ok := body["max_tokens"]; !ok {
		t.Fatalf("together should use max_tokens, got %v", keysOf(body))
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("together must not use max_completion_tokens")
	}
	if _, ok := body["reasoning"]; !ok {
		t.Fatalf("together should send a reasoning object, got %v", keysOf(body))
	}
	// together does not support reasoning_effort.
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("together must not send reasoning_effort")
	}
}

// pi 2fe21b40 added isZai to detectCompat's useMaxTokens disjunction, so every
// route into isZai (both providers and both base URLs) now yields max_tokens.
func TestOpenAICompatZaiUsesMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		baseURL  string
	}{
		{"provider zai", "zai", ""},
		{"provider zai-coding-cn", "zai-coding-cn", ""},
		{"baseUrl api.z.ai", "custom", "https://api.z.ai/api/coding/paas/v4"},
		{"baseUrl open.bigmodel.cn", "custom", "https://open.bigmodel.cn/api/paas/v4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: "glm-5.1", Api: ai.APIOpenAICompletions, Provider: tc.provider, BaseURL: tc.baseURL, Reasoning: true, MaxTokens: 1000}
			if got := getOpenAICompat(model).MaxTokensField; got != "max_tokens" {
				t.Fatalf("zai maxTokensField = %q, want max_tokens", got)
			}
		})
	}
}

func TestOpenAICompatZaiSendsMaxTokensInBody(t *testing.T) {
	model := &ai.Model{ID: "glm-5.1", Api: ai.APIOpenAICompletions, Provider: "zai", Reasoning: true, MaxTokens: 1000}
	mt := 123
	// captureOpenAIBody rewrites BaseURL to the stub server, so this pins the
	// provider-name route into isZai; the URL routes are covered above.
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if v, _ := body["max_tokens"].(float64); v != 123 {
		t.Fatalf("zai max_tokens = %v, want 123 (keys %v)", body["max_tokens"], keysOf(body))
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("zai must not use max_completion_tokens")
	}
}

// pi c185d412 added isDeepSeek to detectCompat's useMaxTokens disjunction, so
// built-in and custom DeepSeek API models now yield max_tokens. The catalog at
// npm 0.84.1 predates the fix and carries no maxTokensField for deepseek, so
// runtime detection is what governs the resolved field for built-ins too.
func TestOpenAICompatDeepSeekUsesMaxTokens(t *testing.T) {
	models := []*ai.Model{
		{ID: "custom-deepseek-model", Api: ai.APIOpenAICompletions, Provider: "deepseek"},
		{ID: "custom-deepseek-model", Api: ai.APIOpenAICompletions, Provider: "custom-deepseek", BaseURL: "https://api.deepseek.com"},
		{ID: "custom-uppercase-deepseek-model", Api: ai.APIOpenAICompletions, Provider: "custom-deepseek-upper", BaseURL: "https://API.DeepSeek.COM"},
	}
	// Every built-in deepseek model, derived from the catalog rather than named:
	// 12f59336a collapsed deepseek-v4-flash and deepseek-v4-flash-vision-exp into
	// deepseek-flash at 0.86.0, and a regen must not be able to leave this test
	// either red or quietly vacuous.
	catalog := ai.GetModels("deepseek")
	if len(catalog) == 0 {
		t.Fatal("the catalog has no deepseek models; this test would prove nothing")
	}
	models = append(models, catalog...)
	for _, m := range models {
		t.Run(m.Provider+"/"+m.ID, func(t *testing.T) {
			if got := getOpenAICompat(m).MaxTokensField; got != "max_tokens" {
				t.Fatalf("maxTokensField = %q, want max_tokens", got)
			}
		})
	}
}

// pi b647d1879 made the deepseek.com baseURL probe case-insensitive and hoisted
// isDeepSeek above the isNonStandard disjunction, so an uppercase DeepSeek
// baseURL now resolves the whole DeepSeek profile, not just maxTokensField.
// Only the DeepSeek probe is case-insensitive upstream; every other baseURL
// probe in detectCompat stays case-sensitive.
func TestOpenAICompatDeepSeekBaseURLIsCaseInsensitive(t *testing.T) {
	upper := &ai.Model{
		ID: "custom-uppercase-deepseek-model", Api: ai.APIOpenAICompletions,
		Provider: "custom-deepseek-upper", BaseURL: "https://API.DeepSeek.COM",
	}
	got := getOpenAICompat(upper)
	if got.MaxTokensField != "max_tokens" {
		t.Errorf("maxTokensField = %q, want max_tokens", got.MaxTokensField)
	}
	if got.ThinkingFormat != "deepseek" {
		t.Errorf("thinkingFormat = %q, want deepseek", got.ThinkingFormat)
	}
	if !got.RequiresReasoningContentOnAssistantMessages {
		t.Error("requiresReasoningContentOnAssistantMessages = false, want true")
	}
	// isDeepSeek now feeds isNonStandard, so these two flip off as well.
	if got.SupportsStore {
		t.Error("supportsStore = true, want false (DeepSeek is non-standard)")
	}
	if got.SupportsDeveloperRole {
		t.Error("supportsDeveloperRole = true, want false (DeepSeek is non-standard)")
	}
}

// The case-insensitive probe is DeepSeek-only: detectCompat's shared baseURL
// helper must stay case-sensitive for every other provider, exactly as upstream
// left them. An uppercase host for another provider therefore does NOT match.
func TestOpenAICompatOtherBaseURLProbesStayCaseSensitive(t *testing.T) {
	for _, tc := range []struct{ name, baseURL string }{
		{"zai", "https://API.Z.AI/v1"},
		{"moonshot", "https://API.Moonshot.CN/v1"},
		{"chutes", "https://LLM.Chutes.AI/v1"},
		{"together", "https://API.Together.AI/v1"},
		{"nvidia", "https://Integrate.API.Nvidia.COM/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: "custom-" + tc.name, BaseURL: tc.baseURL}
			if got := getOpenAICompat(m).MaxTokensField; got != "max_completion_tokens" {
				t.Fatalf("maxTokensField = %q, want max_completion_tokens (uppercase host must not match)", got)
			}
		})
	}
}

func TestOpenAICompatDeepSeekSendsMaxTokensInBody(t *testing.T) {
	// Copy: GetModel returns the shared registry entry and captureOpenAIBody
	// rewrites BaseURL to the stub server. That rewrite also means only the
	// provider-name route into isDeepSeek is reachable here; the baseUrl route
	// is covered by the compat table above.
	catalog := ai.GetModels("deepseek")
	if len(catalog) == 0 {
		t.Fatal("the catalog has no deepseek models; this test would prove nothing")
	}
	model := *catalog[0]
	mt := 123
	body := captureOpenAIBody(t, &model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if v, _ := body["max_tokens"].(float64); v != 123 {
		t.Fatalf("deepseek max_tokens = %v, want 123 (keys %v)", body["max_tokens"], keysOf(body))
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("deepseek must not use max_completion_tokens")
	}
}

func TestOpenAICompatDeepSeekReasoningContent(t *testing.T) {
	model := &ai.Model{ID: "deepseek-reasoner", Api: ai.APIOpenAICompletions, Provider: "deepseek", Reasoning: true, MaxTokens: 1000}
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "earlier"}}, Provider: "deepseek", Api: ai.APIOpenAICompletions, Model: "deepseek-reasoner", StopReason: ai.StopStop, Timestamp: 2},
			ai.NewUserText("again", 3),
		}},
		&ai.SimpleStreamOptions{Reasoning: ai.ThinkingHigh})
	// deepseek thinkingFormat -> thinking:{type:enabled}, and assistant messages carry reasoning_content.
	if _, ok := body["thinking"]; !ok {
		t.Fatalf("deepseek should send thinking, got %v", keysOf(body))
	}
	msgs, _ := body["messages"].([]any)
	foundReasoningContent := false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			if _, ok := mm["reasoning_content"]; ok {
				foundReasoningContent = true
			}
		}
	}
	if !foundReasoningContent {
		t.Fatalf("deepseek assistant messages should include reasoning_content")
	}
}

// Mirrors the upstream openai-completions-empty-tools.test.ts additions
// (09f10595): streamSimple now sends a context-clamped default maxTokens.
func TestOpenAICompatStreamSimpleSendsClampedDefaultMaxTokens(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", MaxTokens: 8000, ContextWindow: 10000}
	// 8000 'x' user chars -> estimate ceil(8000/3.5)=2286; available = 10000-2286-4096 = 3618
	// (upstream 311f0e020).
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText(strings.Repeat("x", 8000), 1)}}, nil)
	if v, _ := body["max_completion_tokens"].(float64); v != 3618 {
		t.Fatalf("default max_completion_tokens = %v, want 3618", body["max_completion_tokens"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("OpenAI must not send max_tokens")
	}
}

func TestOpenAICompatStreamSimpleClampsExplicitMaxTokens(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", MaxTokens: 8000, ContextWindow: 10000}
	mt := 7000
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText(strings.Repeat("x", 8000), 1)}},
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if v, _ := body["max_completion_tokens"].(float64); v != 3618 {
		t.Fatalf("explicit clamped max_completion_tokens = %v, want 3618", body["max_completion_tokens"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestOpenAICompatVLLMPriority ports pi's openai-completions-vllm-priority.test.ts
// (upstream 256f63024): compat.vllmPriority rides as the top-level `priority`
// request field, and no field is sent when it is unset. The check is
// `!== undefined`, so an explicit 0 — vLLM's own server default, and the
// highest priority — must still be sent.
func TestOpenAICompatVLLMPriority(t *testing.T) {
	req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	model := func(compat string) *ai.Model {
		m := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", MaxTokens: 2048}
		if compat != "" {
			m.Compat = json.RawMessage(compat)
		}
		return m
	}

	body := captureOpenAIBody(t, model(`{"vllmPriority":10}`), req, nil)
	if body["priority"] != float64(10) {
		t.Fatalf("priority = %#v, want 10", body["priority"])
	}

	body = captureOpenAIBody(t, model(`{"vllmPriority":0}`), req, nil)
	if got, ok := body["priority"]; !ok || got != float64(0) {
		t.Fatalf("priority = %#v (present=%v), want 0", got, ok)
	}

	// An explicit null is PRESENT, not absent. vllmPriority is the one key
	// getCompat reads bare (`vllmPriority: model.compat.vllmPriority`, no `??`),
	// so pi's `compat.vllmPriority !== undefined` guard passes on null and the
	// body carries `"priority":null`. Every other compat key resolves through
	// `??`, where null falls through to the default instead.
	body = captureOpenAIBody(t, model(`{"vllmPriority":null}`), req, nil)
	if got, ok := body["priority"]; !ok || got != nil {
		t.Fatalf("priority = %#v (present=%v), want an explicit null", got, ok)
	}

	body = captureOpenAIBody(t, model(""), req, nil)
	if got, ok := body["priority"]; ok {
		t.Fatalf("priority must be absent without compat.vllmPriority, got %#v", got)
	}

	body = captureOpenAIBody(t, model(`{"supportsStore":false}`), req, nil)
	if got, ok := body["priority"]; ok {
		t.Fatalf("priority must be absent when a sibling compat key is set, got %#v", got)
	}
}

// pi af7359b90 drops Cerebras from supportsStrictMode — the provider rejects
// the strict field on tools (#9804). Both halves of that commit move together:
// the generator emits supportsStrictMode:false onto every catalog cerebras
// model, and detectCompat's default stops claiming it, so a CUSTOM cerebras
// model carrying no compat key resolves the same way a catalog one does. Only
// this second half is hand-written Go; the first arrives with the regen.
func TestOpenAICompatCerebrasOmitsStrict(t *testing.T) {
	models := []*ai.Model{
		{ID: "gpt-oss-120b", Api: ai.APIOpenAICompletions, Provider: "cerebras"},
		{ID: "custom-model", Api: ai.APIOpenAICompletions, Provider: "custom-cerebras", BaseURL: "https://api.cerebras.ai/v1"},
	}
	for _, model := range models {
		req := baseReq()
		req.Tools = []ai.Tool{{
			Name:                "t1",
			Description:         "strict tool",
			Parameters:          ai.Object(ai.Prop("x", ai.String())),
			ConstrainedSampling: &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingJSONSchema},
		}}
		body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
		tools, _ := body["tools"].([]map[string]any)
		if len(tools) == 0 {
			t.Fatalf("%s/%s: expected a tool in the body (keys %v)", model.Provider, model.ID, keysOf(body))
		}
		fn, _ := tools[0]["function"].(map[string]any)
		if has(fn, "strict") {
			t.Fatalf("%s/%s: cerebras must not be sent strict, got %v", model.Provider, model.ID, fn["strict"])
		}
	}
}
