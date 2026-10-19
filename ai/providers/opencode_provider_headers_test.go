package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// CHARACTERIZATION tests — they were green before they were written, and that
// is the finding they record.
//
// They transliterate pi's packages/ai/test/opencode-provider-headers.test.ts,
// added by upstream 561a2e066 ("fix(ai): send OpenCode session header"). That
// commit wraps the opencode / opencode-go provider api tables in
// withOpenCodeSessionHeader, which injects x-opencode-session from
// options.sessionId when no source has already named the header — closing a gap
// where a bare @earendil-works/pi-ai consumer sent nothing, because pi only ever
// set the header from the coding-agent layer (core/provider-attribution.ts).
//
// The Go port has no such gap to close: it homes pi's coding-agent attribution
// in ai/providers/attribution.go, so ai.Stream* has always emitted the header
// for opencode models whenever SessionID is set. The port's delta for
// 561a2e066 is therefore EMPTY, and these assertions — pi's own, values
// included — are what says so rather than a category argument.
//
// One pi wrapper covers FOUR api tables, so this covers all four: the port
// reaches each from a different point in a different header order, and google's
// reaches the wire by a different mechanism entirely (applyAsRecord rather than
// applyAsDefaultHeaders). Pinning one adapter would have left the other three
// free to lose the header silently.
//
// They assert only the cells where the port matches pi on BOTH of pi's paths.
// Where pi's two paths disagree the port necessarily matches one and not the
// other, because one Go function stands in for both. Those cells are tracked
// as Scope queue entry 15 and never asserted here, because a green test
// carrying the port's own value would immunise them. Today they are the three
// bare-SDK cells: the port also sends x-opencode-client, also honours the
// opencode.ai host gate, and puts the session header below model.Headers. A
// cell that closes joins the assertions below (upstream a328aa89a closed a
// fourth, google's case-variant deletion marker); one that opens is named here.

// opencodeAdapter is one api table upstream's wrapper is attached to, paired
// with the port's existing wire-capture helper for it.
type opencodeAdapter struct {
	api       ai.Api
	providers []ai.ProviderId // the opencode providers whose table names this api
	capture   func(t *testing.T, model *ai.Model, opts ai.StreamOptions) http.Header
}

func opencodeAdapters() []opencodeAdapter {
	both := []ai.ProviderId{"opencode", "opencode-go"}
	return []opencodeAdapter{
		{ai.APIOpenAICompletions, both, captureOpenAIHeaders},
		{ai.APIOpenAIResponses, both, captureOpenAIResponsesHeaders},
		{ai.APIAnthropicMessages, both, func(t *testing.T, model *ai.Model, opts ai.StreamOptions) http.Header {
			return captureAnthropicHeaders(t, model, &AnthropicOptions{StreamOptions: opts})
		}},
		// opencode-go's table gained google-generative-ai in upstream 195120a91.
		{ai.APIGoogleGenerativeAI, both, captureGoogleHeaders},
	}
}

func opencodeModel(api ai.Api, provider ai.ProviderId) *ai.Model {
	return &ai.Model{
		ID: "test-model", Name: "Test model", Api: api, Provider: provider,
		Input: []string{"text"}, MaxTokens: 100, ContextWindow: 1000,
	}
}

func opencodeOptions(sessionID string, retention ai.CacheRetention, headers ai.ProviderHeaders) ai.StreamOptions {
	return ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", Headers: headers},
		SessionID:              sessionID,
		CacheRetention:         retention,
	}
}

// pi: "maps sessionId for %s requests even without cache retention" — the
// header is independent of cacheRetention, which gates only the separate
// prompt-cache and session-affinity headers.
func TestOpenCodeSessionHeaderWithoutCacheRetention(t *testing.T) {
	for _, adapter := range opencodeAdapters() {
		for _, provider := range adapter.providers {
			t.Run(string(adapter.api)+"/"+string(provider), func(t *testing.T) {
				h := adapter.capture(t, opencodeModel(adapter.api, provider),
					opencodeOptions("conversation-1", ai.CacheNone, nil))
				if got := h.Get("x-opencode-session"); got != "conversation-1" {
					t.Fatalf("x-opencode-session = %q, want conversation-1", got)
				}
			})
		}
	}
}

// StreamSimple* is not a second header path — each one copies StreamOptions and
// tail-calls its Stream* counterpart, which owns the only header build. pi's
// test runs both entry points because pi's wrapper really does wrap two
// functions; one case here records that the port cannot diverge between them.
func TestOpenCodeSessionHeaderOnStreamSimple(t *testing.T) {
	h := captureOpenAISimpleHeaders(t, opencodeModel(ai.APIOpenAICompletions, "opencode"),
		opencodeOptions("conversation-1", ai.CacheNone, nil))
	if got := h.Get("x-opencode-session"); got != "conversation-1" {
		t.Fatalf("x-opencode-session = %q, want conversation-1", got)
	}
}

// captureOpenAISimpleHeaders is captureOpenAIHeaders through the StreamSimple
// entry point, which is the only thing this file needs a helper of its own for.
func captureOpenAISimpleHeaders(t *testing.T, model *ai.Model, opts ai.StreamOptions) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamSimpleOpenAICompletions(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

		&ai.SimpleStreamOptions{StreamOptions: opts}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return got
}

// pi: "preserves a case-insensitive caller override" — the wrapper's hasHeader
// is key-only and case-insensitive, so a caller's value stands however it is
// spelled. The port reaches the same place from the other side: opts.Headers is
// its highest-precedence source, and net/http canonicalizes the name.
func TestOpenCodeSessionHeaderCallerOverride(t *testing.T) {
	for _, adapter := range opencodeAdapters() {
		t.Run(string(adapter.api), func(t *testing.T) {
			h := adapter.capture(t, opencodeModel(adapter.api, "opencode"),
				opencodeOptions("generated-value", "", ai.ProviderHeaders{"X-OpenCode-Session": ai.HeaderValue("caller-value")}))
			if got := h.Get("x-opencode-session"); got != "caller-value" {
				t.Fatalf("x-opencode-session = %q, want caller-value (the caller's spelling wins)", got)
			}
		})
	}
}

// pi: the same test's null case. A caller deletion marker is preserved rather
// than substituted, and downstream that means the header is not sent at all.
// The marker here is spelled differently from the session header, which still
// deletes it on google since upstream a328aa89a: providerHeadersToRecord now
// folds names case-insensitively, agreeing with pi's bare path, whose hasHeader
// already declined to inject the header.
func TestOpenCodeSessionHeaderCallerDeletionMarker(t *testing.T) {
	for _, adapter := range opencodeAdapters() {
		t.Run(string(adapter.api), func(t *testing.T) {
			h := adapter.capture(t, opencodeModel(adapter.api, "opencode"),
				opencodeOptions("generated-value", "", ai.ProviderHeaders{"X-OpenCode-Session": nil}))
			if got := h.Get("x-opencode-session"); got != "" {
				t.Fatalf("a caller deletion marker must suppress the header, got %q", got)
			}
		})
	}
}

// pi: "does not fabricate a session header when sessionId is absent".
func TestOpenCodeSessionHeaderAbsentWithoutSessionID(t *testing.T) {
	for _, adapter := range opencodeAdapters() {
		t.Run(string(adapter.api), func(t *testing.T) {
			h := adapter.capture(t, opencodeModel(adapter.api, "opencode"),
				opencodeOptions("", "", ai.ProviderHeaders{"x-custom": ai.HeaderValue("value")}))
			if got := h.Get("x-opencode-session"); got != "" {
				t.Fatalf("x-opencode-session must be absent without a session id, got %q", got)
			}
			if got := h.Get("x-custom"); got != "value" {
				t.Fatalf("x-custom = %q, want value (the request must have reached the wire)", got)
			}
		})
	}
}
