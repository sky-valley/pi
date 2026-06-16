package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Provider attribution header tests. Faithful to pi
// core/provider-attribution.ts at upstream f8a77f47 (which adds the Vercel AI
// Gateway branch). The request-body differential tests do not cover headers, so
// these assert the exact header names + values land on the outgoing request,
// across each API/provider pi applies attribution to, plus the precedence rule
// (consumer opts.Headers override the defaults).

const attrDoneSSE = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// captureOpenAICompletionsHeaders runs the openai-completions provider against a
// stub server and returns the request headers. The model's Provider drives
// attribution; BaseURL is overwritten with the test server so the request is
// actually sent (host-based detection is unit-tested separately below).
func captureOpenAICompletionsHeaders(t *testing.T, provider ai.Provider, sessionID string, optsHeaders map[string]string) http.Header {
	t.Helper()
	t.Setenv("PI_TELEMETRY", "1")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, attrDoneSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: provider + "-test", Api: ai.APIOpenAICompletions, Provider: provider, BaseURL: server.URL,
		Input: []string{"text"}, MaxTokens: 4096,
	}
	opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: sessionID, Headers: optsHeaders}}
	StreamOpenAICompletions(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
	return got
}

func TestAttributionOpenRouter(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "openrouter", "", nil)
	want := map[string]string{
		"HTTP-Referer":            "https://pi.dev",
		"X-OpenRouter-Title":      "pi",
		"X-OpenRouter-Categories": "cli-agent",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
}

// f8a77f47: Vercel AI Gateway attribution branch.
func TestAttributionVercelGateway(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "vercel-ai-gateway", "", nil)
	if got := h.Get("http-referer"); got != "https://pi.dev" {
		t.Fatalf("http-referer = %q, want https://pi.dev", got)
	}
	if got := h.Get("x-title"); got != "pi" {
		t.Fatalf("x-title = %q, want pi", got)
	}
}

func TestAttributionNvidiaNim(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "nvidia", "", nil)
	if got := h.Get("X-BILLING-INVOKE-ORIGIN"); got != "Pi" {
		t.Fatalf("X-BILLING-INVOKE-ORIGIN = %q, want Pi", got)
	}
}

func TestAttributionCloudflare(t *testing.T) {
	for _, p := range []ai.Provider{"cloudflare-workers-ai", "cloudflare-ai-gateway"} {
		h := captureOpenAICompletionsHeaders(t, p, "", nil)
		if got := h.Get("User-Agent"); got != "pi-coding-agent" {
			t.Fatalf("provider %s: User-Agent = %q, want pi-coding-agent", p, got)
		}
	}
}

func TestAttributionOpenCodeSessionHeaders(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "opencode", "sess-123", nil)
	if got := h.Get("x-opencode-session"); got != "sess-123" {
		t.Fatalf("x-opencode-session = %q, want sess-123", got)
	}
	if got := h.Get("x-opencode-client"); got != "pi" {
		t.Fatalf("x-opencode-client = %q, want pi", got)
	}
}

// pi getSessionHeaders is gated on a sessionId being present.
func TestAttributionOpenCodeNoSessionWithoutID(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "opencode", "", nil)
	if got := h.Get("x-opencode-session"); got != "" {
		t.Fatalf("x-opencode-session must be absent without a session id, got %q", got)
	}
	if got := h.Get("x-opencode-client"); got != "" {
		t.Fatalf("x-opencode-client must be absent without a session id, got %q", got)
	}
}

// Non-attributed providers (e.g. plain openai) get no attribution headers.
func TestAttributionAbsentForUnattributedProvider(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "openai", "sess", nil)
	for _, k := range []string{"HTTP-Referer", "X-OpenRouter-Title", "X-BILLING-INVOKE-ORIGIN", "x-opencode-session", "x-title"} {
		if got := h.Get(k); got != "" {
			t.Fatalf("%s should be absent for plain openai, got %q", k, got)
		}
	}
}

// pi gates the default attribution headers on install telemetry; PI_TELEMETRY=0
// disables them. Session headers are independent of telemetry (getSessionHeaders
// is not gated), so they still flow.
func TestAttributionTelemetryDisabled(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "0")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, attrDoneSSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "or", Api: ai.APIOpenAICompletions, Provider: "openrouter", BaseURL: server.URL, MaxTokens: 4096}
	opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}
	StreamOpenAICompletions(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
	if v := got.Get("HTTP-Referer"); v != "" {
		t.Fatalf("HTTP-Referer must be absent when telemetry disabled, got %q", v)
	}
	if v := got.Get("X-OpenRouter-Title"); v != "" {
		t.Fatalf("X-OpenRouter-Title must be absent when telemetry disabled, got %q", v)
	}
}

// pi precedence: the attribution bundle is applied as options.headers, but
// consumer-supplied opts.Headers are Object.assigned over the defaults inside
// the bundle and win. (pi test: "lets provider and request headers override the
// defaults".)
func TestAttributionOptsHeadersOverrideDefaults(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "openrouter", "", map[string]string{
		"HTTP-Referer":       "https://consumer.example",
		"X-OpenRouter-Title": "consumer",
	})
	if got := h.Get("HTTP-Referer"); got != "https://consumer.example" {
		t.Fatalf("opts.Headers must override attribution default: HTTP-Referer = %q", got)
	}
	if got := h.Get("X-OpenRouter-Title"); got != "consumer" {
		t.Fatalf("opts.Headers must override attribution default: X-OpenRouter-Title = %q", got)
	}
	// Untouched default still present.
	if got := h.Get("X-OpenRouter-Categories"); got != "cli-agent" {
		t.Fatalf("X-OpenRouter-Categories = %q, want cli-agent", got)
	}
}

// pi precedence: auth.headers (= {...model.headers, ...providerHeaders,
// ...modelHeaders}) are Object.assigned over the attribution defaults in
// mergeProviderAttributionHeaders (sdk.ts), so model.Headers override an
// attribution default. This is the regression guarded by the header-precedence
// fix: a model whose Headers set HTTP-Referer to a custom value on an openrouter
// model must win over the https://pi.dev default.
func TestAttributionModelHeadersOverrideDefaults(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, attrDoneSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: "or", Api: ai.APIOpenAICompletions, Provider: "openrouter", BaseURL: server.URL, MaxTokens: 4096,
		Headers: map[string]string{"HTTP-Referer": "https://custom.example"},
	}
	opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}
	StreamOpenAICompletions(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
	if v := got.Get("HTTP-Referer"); v != "https://custom.example" {
		t.Fatalf("model.Headers must override attribution default: HTTP-Referer = %q, want https://custom.example", v)
	}
	// An attribution default the model did not override stays at pi's value.
	if v := got.Get("X-OpenRouter-Title"); v != "pi" {
		t.Fatalf("X-OpenRouter-Title = %q, want pi", v)
	}
}

// Same precedence rule for the Anthropic API path: model.Headers override an
// attribution default (HTTP-Referer on an openrouter-routed Anthropic model).
func TestAttributionAnthropicModelHeadersOverrideDefaults(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	model := &ai.Model{
		ID: "claude", Api: ai.APIAnthropicMessages, Provider: "openrouter", MaxTokens: 4096,
		Headers: map[string]string{"HTTP-Referer": "https://custom.example"},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	h, _ := anthropicCapture(t, model, req, &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}, anthropicSSE)
	if v := h.Get("HTTP-Referer"); v != "https://custom.example" {
		t.Fatalf("model.Headers must override attribution default: HTTP-Referer = %q, want https://custom.example", v)
	}
	if v := h.Get("X-OpenRouter-Title"); v != "pi" {
		t.Fatalf("X-OpenRouter-Title = %q, want pi", v)
	}
}

// pi test: "lets configured OpenCode headers override the defaults". Consumer
// opts.Headers override the session headers too.
func TestAttributionOptsHeadersOverrideSessionHeaders(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "opencode", "sess-123", map[string]string{
		"x-opencode-session": "configured-session",
		"x-opencode-client":  "configured-client",
	})
	if got := h.Get("x-opencode-session"); got != "configured-session" {
		t.Fatalf("x-opencode-session = %q, want configured-session", got)
	}
	if got := h.Get("x-opencode-client"); got != "configured-client" {
		t.Fatalf("x-opencode-client = %q, want configured-client", got)
	}
}

// Attribution is wired into every provider pi applies it to. Cover the other
// three APIs (responses, anthropic, google) for the Vercel branch.

func TestAttributionResponsesVercel(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, responsesSSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "m", Api: ai.APIOpenAIResponses, Provider: "vercel-ai-gateway", BaseURL: server.URL, MaxTokens: 4096}
	StreamOpenAIResponses(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if got.Get("http-referer") != "https://pi.dev" || got.Get("x-title") != "pi" {
		t.Fatalf("responses vercel attribution wrong: http-referer=%q x-title=%q", got.Get("http-referer"), got.Get("x-title"))
	}
}

func TestAttributionAnthropicOpenRouter(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	model := &ai.Model{ID: "claude", Api: ai.APIAnthropicMessages, Provider: "openrouter", MaxTokens: 4096}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	h, _ := anthropicCapture(t, model, req, &AnthropicOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}, anthropicSSE)
	if h.Get("HTTP-Referer") != "https://pi.dev" || h.Get("X-OpenRouter-Title") != "pi" {
		t.Fatalf("anthropic openrouter attribution wrong: HTTP-Referer=%q X-OpenRouter-Title=%q",
			h.Get("HTTP-Referer"), h.Get("X-OpenRouter-Title"))
	}
}

func TestAttributionGoogleVercel(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini", Api: ai.APIGoogleGenerativeAI, Provider: "vercel-ai-gateway", BaseURL: server.URL, MaxTokens: 4096}
	StreamGoogle(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&GoogleOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if got.Get("http-referer") != "https://pi.dev" || got.Get("x-title") != "pi" {
		t.Fatalf("google vercel attribution wrong: http-referer=%q x-title=%q", got.Get("http-referer"), got.Get("x-title"))
	}
}

// Host-based detection (pi matchesHost / isOpenRouterModel substring) is
// unit-tested directly so the BaseURL is free to be a real provider host rather
// than the test server.
func TestAttributionHostDetection(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	cases := []struct {
		name    string
		model   *ai.Model
		wantKey string
		wantVal string
	}{
		{"openrouter host", &ai.Model{Provider: "custom", BaseURL: "https://openrouter.ai/api/v1"}, "HTTP-Referer", "https://pi.dev"},
		{"openrouter legacy substring", &ai.Model{Provider: "custom", BaseURL: "not-a-url-openrouter.ai"}, "X-OpenRouter-Title", "pi"},
		{"nvidia host", &ai.Model{Provider: "custom", BaseURL: "https://integrate.api.nvidia.com/v1"}, "X-BILLING-INVOKE-ORIGIN", "Pi"},
		{"cloudflare api host", &ai.Model{Provider: "custom", BaseURL: "https://api.cloudflare.com/x"}, "User-Agent", "pi-coding-agent"},
		{"cloudflare gateway host", &ai.Model{Provider: "custom", BaseURL: "https://gateway.ai.cloudflare.com/x"}, "User-Agent", "pi-coding-agent"},
		{"vercel gateway host", &ai.Model{Provider: "custom", BaseURL: "https://ai-gateway.vercel.sh/v1"}, "http-referer", "https://pi.dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := getDefaultAttributionHeaders(tc.model)
			if h[tc.wantKey] != tc.wantVal {
				t.Fatalf("%s = %q, want %q", tc.wantKey, h[tc.wantKey], tc.wantVal)
			}
		})
	}
}

// pi: NVIDIA models routed through OpenRouter/Vercel get the gateway's headers,
// not NVIDIA's (provider/host takes precedence over the model id).
func TestAttributionNvidiaModelThroughGateways(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	or := getDefaultAttributionHeaders(&ai.Model{ID: "nvidia/nemotron", Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1"})
	if or["HTTP-Referer"] != "https://pi.dev" {
		t.Fatalf("openrouter HTTP-Referer = %q", or["HTTP-Referer"])
	}
	if _, ok := or["X-BILLING-INVOKE-ORIGIN"]; ok {
		t.Fatalf("NVIDIA header must not leak through OpenRouter")
	}
	v := getDefaultAttributionHeaders(&ai.Model{ID: "nvidia/nemotron", Provider: "vercel-ai-gateway", BaseURL: "https://ai-gateway.vercel.sh/v1"})
	if _, ok := v["X-BILLING-INVOKE-ORIGIN"]; ok {
		t.Fatalf("NVIDIA header must not leak through Vercel AI Gateway")
	}
}

// pi getSessionHeaders only fires for OpenCode targets, regardless of session id.
func TestAttributionSessionHeadersGatedToOpenCode(t *testing.T) {
	if getSessionAttributionHeaders(&ai.Model{Provider: "openai"}, "sess") != nil {
		t.Fatalf("session headers must be absent for non-opencode providers")
	}
	if getSessionAttributionHeaders(&ai.Model{Provider: "opencode-go"}, "sess")["x-opencode-client"] != "pi" {
		t.Fatalf("opencode-go must receive session headers")
	}
	if getSessionAttributionHeaders(&ai.Model{Provider: "custom", BaseURL: "https://opencode.ai/zen/v1"}, "sess")["x-opencode-session"] != "sess" {
		t.Fatalf("opencode.ai host must receive session headers")
	}
}
