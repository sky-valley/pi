package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// The vendor SDKs fold a request's headers bundle by bundle — their own
// headers, their auth header, pi's `defaultHeaders`, the JSON body's
// content-type, then the per-request headers — and pi's header object is only
// the defaultHeaders bundle (see sdkHeaders). Every want here was measured on
// pi's wire: 8676a0dcd's src under node v26.4.0 with openai 6.40.0 and
// @anthropic-ai/sdk 0.124.0 (the versions its package-lock names), against a
// raw socket; openai 7.19.0 (pi-ai's since ab30693d6) folds the same bundles
// in the same order, re-measured at 2b0a123de.

// sdkAdapters are the adapters whose headers a vendor SDK folds.
var sdkAdapters = []string{"openai-completions", "openai-responses", "anthropic-messages"}

// runSDKWire sends one request through adapter with modelHeaders on the model
// and returns the headers that reached the server (nil when none did) and the
// stream's final message.
func runSDKWire(t *testing.T, adapter string, modelHeaders ai.ProviderHeaders, opts ai.StreamOptions) (http.Header, *ai.AssistantMessage) {
	t.Helper()
	sse := map[string]string{"openai-completions": completionsDoneSSE, "openai-completions-groq": completionsDoneSSE,
		"openai-responses": responsesSSE, "anthropic-messages": anthropicSSE}[adapter]
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	model := &ai.Model{BaseURL: server.URL, Input: []string{"text"}, MaxTokens: 4096, Headers: modelHeaders}
	var final *ai.AssistantMessage
	switch adapter {
	case "openai-completions":
		model.ID, model.Api, model.Provider = "gpt-test", ai.APIOpenAICompletions, "openai"
		final = StreamOpenAICompletions(context.Background(), model, req, &OpenAIOptions{StreamOptions: opts}).Result()
	case "openai-completions-groq":
		model.ID, model.Api, model.Provider = "llama", ai.APIOpenAICompletions, "groq"
		final = StreamOpenAICompletions(context.Background(), model, req, &OpenAIOptions{StreamOptions: opts}).Result()
	case "openai-responses":
		model.ID, model.Api, model.Provider = "gpt-test", ai.APIOpenAIResponses, "openai"
		final = StreamOpenAIResponses(context.Background(), model, req, &OpenAIResponsesOptions{StreamOptions: opts}).Result()
	case "anthropic-messages":
		model.ID, model.Api, model.Provider = "claude-test", ai.APIAnthropicMessages, "anthropic"
		final = StreamAnthropic(context.Background(), model, req, &AnthropicOptions{StreamOptions: opts}).Result()
	default:
		t.Fatalf("no SDK adapter %q", adapter)
	}
	return got, final
}

// The headers the SDK owns sit in bundles of their own: no spelling in pi's
// object takes their slot. content-type rides in the body bundle, above pi's
// object, so neither a consumer value nor a marker changes it; Accept,
// anthropic-version and the auth header ride below it, so a later source
// replaces them whatever the spellings of the earlier ones. Accept is openai's
// `application/json` whether or not the request streams. On anthropic pi's own
// object carries an accept of its own, so there two spellings in the object
// decide it by slot.
func TestSDKOwnHeadersSitOutsidePisObject(t *testing.T) {
	for _, tc := range []struct {
		name          string
		model, opts   ai.ProviderHeaders
		header        string
		want          string
		wantAnthropic string // when anthropic's differs
	}{
		{"accept default", nil, nil, "accept", "application/json", ""},
		{"content-type default", nil, nil, "content-type", "application/json", ""},
		{"content-type override", nil, ai.ProviderHeaders{"Content-Type": strPtr("text/plain")}, "content-type", "application/json", ""},
		{"content-type model override", ai.ProviderHeaders{"content-type": strPtr("text/plain")}, nil, "content-type", "application/json", ""},
		{"content-type marker", nil, ai.ProviderHeaders{"content-type": nil}, "content-type", "application/json", ""},
		{"accept override", nil, ai.ProviderHeaders{"Accept": strPtr("x/y")}, "accept", "x/y", ""},
		{"accept across spellings", ai.ProviderHeaders{"Accept": strPtr("a")}, ai.ProviderHeaders{"accept": strPtr("b")}, "accept", "b", "a"},
		{"authorization across spellings", ai.ProviderHeaders{"Authorization": strPtr("a")}, ai.ProviderHeaders{"authorization": strPtr("b")}, "authorization", "b", ""},
		{"x-api-key across spellings", ai.ProviderHeaders{"X-Api-Key": strPtr("a")}, ai.ProviderHeaders{"x-api-key": strPtr("b")}, "x-api-key", "b", ""},
		{"anthropic-version across spellings", ai.ProviderHeaders{"Anthropic-Version": strPtr("a")}, ai.ProviderHeaders{"anthropic-version": strPtr("b")}, "anthropic-version", "b", ""},
	} {
		for _, adapter := range sdkAdapters {
			t.Run(adapter+"/"+tc.name, func(t *testing.T) {
				h, final := runSDKWire(t, adapter, tc.model, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "k", Headers: tc.opts,
				}})
				want := tc.want
				if adapter == "anthropic-messages" && tc.wantAnthropic != "" {
					want = tc.wantAnthropic
				}
				wantSent(t, h, final, tc.header, want)
			})
		}
	}
}

// The beta namespace builds the per-request anthropic-beta header before the
// client builds any other, so its value is converted, and refused, ahead of
// the api key and of every header in pi's object. The configured value is
// padded here so the beta header (trimmed by getBetaFeatures) and the same
// value in pi's object report different indexes.
func TestAnthropicBetaHeaderIsConvertedFirst(t *testing.T) {
	bad := " " + string(rune(cjk))
	for _, tc := range []struct {
		name        string
		key         string
		model, opts ai.ProviderHeaders
		want        string
	}{
		{"consumer beta", "k", nil, ai.ProviderHeaders{"anthropic-beta": &bad}, byteStringError(0, cjk)},
		{"model beta", "k", ai.ProviderHeaders{"anthropic-beta": &bad}, nil, byteStringError(0, cjk)},
		{"ahead of the key", "k" + string(rune(0x100)), nil, ai.ProviderHeaders{"anthropic-beta": &bad}, byteStringError(0, cjk)},
		{"ahead of the key, model beta", "k" + string(rune(0x100)), ai.ProviderHeaders{"anthropic-beta": &bad}, nil, byteStringError(0, cjk)},
		{"ahead of the object", "k", nil, ai.ProviderHeaders{"anthropic-beta": &bad, "X-A": strPtr(string(rune(0x100)))}, byteStringError(0, cjk)},
		{"refused ahead of the key", "k" + string(rune(0x100)), nil, ai.ProviderHeaders{"anthropic-beta": strPtr("a\nb")}, invalidValueError("a\nb")},
		{"refused ahead of a refused key", "k\nz", nil, ai.ProviderHeaders{"anthropic-beta": strPtr("a\nb")}, invalidValueError("a\nb")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, final := runSDKWire(t, "anthropic-messages", tc.model, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: tc.key, Headers: tc.opts,
			}})
			wantNotSent(t, h, final, tc.want)
		})
	}
}
