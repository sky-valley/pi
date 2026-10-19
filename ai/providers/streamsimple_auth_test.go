package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Tests for pi 8b5899dce ("fix(ai): restore stream compatibility"), which put an
// eager auth assertion back at the top of the anthropic and google streamSimple
// adapters. Upstream raises it SYNCHRONOUSLY, before a stream exists. Under the
// port's G3 ruling (ai/stream.go:90) a setup failure pi throws is rendered as a
// terminal "error" event on the returned stream instead, so StreamSimple* keeps
// its single return value; what carries over from the eager throw is its
// PRECEDENCE — the auth failure preempts every other setup failure on that path,
// and still arrives as one terminal error event with no "start" before it.
//
// The two adapters get DIFFERENT gates upstream and they are not unified here:
//   - google: `const apiKey = options?.apiKey; if (!apiKey) throw` — no header
//     escape hatch (google-generative-ai.ts:302).
//   - anthropic: assertRequestAuth(provider, apiKey, headers), which also
//     passes on a non-empty "authorization", "x-api-key" or
//     "cf-aig-authorization" request header (anthropic-messages.ts:301,854).

// drainSimple returns the event types a stream emitted and its final message.
func drainSimple(stream *ai.AssistantMessageEventStream) ([]ai.EventType, *ai.AssistantMessage) {
	var types []ai.EventType
	for ev := range stream.Events() {
		types = append(types, ev.Type)
	}
	return types, stream.Result()
}

// --- google ---

func googleAuthModel(levels map[ai.ModelThinkingLevel]*string) *ai.Model {
	return &ai.Model{
		ID: "gemini-3.7-flash", Api: ai.APIGoogleGenerativeAI, Provider: "test-google",
		BaseURL: "https://example.invalid/v1beta", Reasoning: true, ThinkingLevelMap: levels,
		Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 4096,
	}
}

const googleNoKeyErr = "No API key for provider: test-google"

// TestGoogleStreamSimpleMissingKeyPreemptsOtherSetupFailures pins the eager
// api-key check upstream added at the top of google's streamSimple. Every other
// setup failure on that path used to win because it is reached first: the
// custom-fetch guard lives inside StreamGoogle (google.go:362) ahead of the
// in-stream key check, and the thinking-level mapping resolves in
// StreamSimpleGoogle before the stream is even built (google.go:94).
//
// Each precedence case asserts a control first — the error the SAME request
// reports when a key IS supplied — so the precedence assertion cannot pass
// vacuously.
func TestGoogleStreamSimpleMissingKeyPreemptsOtherSetupFailures(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	customClient := &http.Client{}

	cases := []struct {
		name    string
		model   func() *ai.Model
		opts    func(apiKey string) *ai.SimpleStreamOptions
		control string // the error the same request reports WITH a key
	}{
		{
			name:  "no other setup failure",
			model: func() *ai.Model { return googleAuthModel(nil) },
			opts: func(apiKey string) *ai.SimpleStreamOptions {
				return &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
					ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: apiKey},
				}}
			},
		},
		{
			name:  "preempts the custom-fetch guard",
			model: func() *ai.Model { return googleAuthModel(nil) },
			opts: func(apiKey string) *ai.SimpleStreamOptions {
				return &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
					ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: apiKey, HTTPClient: customClient},
				}}
			},
			control: "Custom fetch is not supported by the Google Generative AI adapter",
		},
		{
			name: "preempts an unmappable thinking level",
			model: func() *ai.Model {
				return googleAuthModel(levelMap(map[ai.ModelThinkingLevel]string{"high": "extreme"}))
			},
			opts: func(apiKey string) *ai.SimpleStreamOptions {
				return &ai.SimpleStreamOptions{
					StreamOptions: ai.StreamOptions{
						ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: apiKey},
					},
					Reasoning: ai.ThinkingHigh,
				}
			},
			control: "Unsupported Google thinking level mapping for test-google/gemini-3.7-flash: high -> extreme",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.control != "" {
				_, final := drainSimple(StreamSimpleGoogle(context.Background(), tc.model(), ai.NormalizeContext(req), tc.opts("gemini-key")))
				if final.ErrorMessage != tc.control {
					t.Fatalf("with a key: error = %q, want %q (the precedence check would be vacuous)",
						final.ErrorMessage, tc.control)
				}
			}

			types, final := drainSimple(StreamSimpleGoogle(context.Background(), tc.model(), ai.NormalizeContext(req), tc.opts("")))
			if len(types) != 1 || types[0] != ai.EventError {
				t.Fatalf("events = %v, want a single error event", types)
			}
			if final.StopReason != ai.StopError || final.ErrorMessage != googleNoKeyErr {
				t.Fatalf("final = stop %s, err %q, want stop error / %q", final.StopReason, final.ErrorMessage, googleNoKeyErr)
			}
		})
	}
}

// TestGoogleStreamSimpleMissingKeyWithNilOptions covers the nil-options call,
// pi's `streamSimple(model, context)` with no options object at all.
func TestGoogleStreamSimpleMissingKeyWithNilOptions(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	types, final := drainSimple(StreamSimpleGoogle(context.Background(), googleAuthModel(nil), ai.NormalizeContext(req), nil))
	if len(types) != 1 || types[0] != ai.EventError {
		t.Fatalf("events = %v, want a single error event", types)
	}
	if final.ErrorMessage != googleNoKeyErr {
		t.Fatalf("error = %q, want %q", final.ErrorMessage, googleNoKeyErr)
	}
}

// TestGoogleStreamKeepsCustomFetchPrecedence guards the OTHER half: 8b5899dce
// touched streamSimple only, so the full Stream path must keep reporting the
// custom-fetch guard ahead of its in-stream key check (google.go:362 before
// :366, deliberately "as it does in pi").
func TestGoogleStreamKeepsCustomFetchPrecedence(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &GoogleOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{HTTPClient: &http.Client{}},
	}}
	_, final := drainSimple(StreamGoogle(context.Background(), googleAuthModel(nil), ai.NormalizeContext(req), opts))
	const want = "Custom fetch is not supported by the Google Generative AI adapter"
	if final.ErrorMessage != want {
		t.Fatalf("error = %q, want %q", final.ErrorMessage, want)
	}
}

// --- anthropic ---

const streamSimpleAuthSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicSimpleCapture runs StreamSimpleAnthropic against a local server and
// returns the headers the request carried (nil when the request never went out)
// plus the final message.
func anthropicSimpleCapture(t *testing.T, model *ai.Model, opts *ai.SimpleStreamOptions) (http.Header, *ai.AssistantMessage) {
	t.Helper()
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, streamSimpleAuthSSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	_, final := drainSimple(StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts))
	return gotHeaders, final
}

func anthropicAuthModel() *ai.Model {
	return &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "test-anthropic", MaxTokens: 4096}
}

func strHeader(v string) *string { return &v }

const anthropicNoKeyErr = "No API key for provider: test-anthropic"

// TestAnthropicStreamSimpleAcceptsHeaderOwnedAuth pins pi's assertRequestAuth
// (anthropic-messages.ts:301), the gate 8b5899dce re-attached to streamSimple:
//
//	if (apiKey) return;
//	if (hasHeader(headers, "authorization") || hasHeader(headers, "x-api-key")
//	    || hasHeader(headers, "cf-aig-authorization")) return;
//	throw new Error(`No API key for provider: ${provider}`);
//
// A request whose auth lives in a header carries no apiKey at all — that is how
// the Models runtime hands ANTHROPIC_AUTH_TOKEN (Authorization: Bearer, see
// ai/builtins_models.go:119) and a Cloudflare AI Gateway credential to the
// adapter. pi streams such a request; the port used to reject it, because its
// gate read the api key and the ANTHROPIC_AUTH_TOKEN env value only.
//
// The wire half matters too: pi passes `apiKey ?? null` to the SDK, which sends
// no x-api-key when there is no key, so a header-owned request must not carry an
// empty one.
func TestAnthropicStreamSimpleAcceptsHeaderOwnedAuth(t *testing.T) {
	// A stale ANTHROPIC_AUTH_TOKEN in the ambient environment would authenticate
	// these requests through the port's other credential source and hide the gap.
	t.Setenv(ai.AnthropicAuthTokenEnv, "")

	cases := []struct {
		name    string
		headers ai.ProviderHeaders
	}{
		{name: "authorization", headers: ai.ProviderHeaders{"Authorization": strHeader("Bearer gw-token")}},
		{name: "x-api-key", headers: ai.ProviderHeaders{"x-api-key": strHeader("gw-key")}},
		// As cloudflareAIGatewayAuth resolves it: the markers are what lets the
		// SDK send it, since cf-aig-authorization alone passes pi's gate but not
		// the SDK's validateHeaders (TestAdapterAuthHeadersMatchPi).
		{name: "cf-aig-authorization", headers: ai.ProviderHeaders{
			"cf-aig-authorization": strHeader("Bearer cf-token"), "Authorization": nil, "x-api-key": nil,
		}},
		// pi's hasHeader lowercases the name it compares, so the spelling of the
		// caller's key does not matter.
		{name: "uppercase spelling", headers: ai.ProviderHeaders{"AUTHORIZATION": strHeader("Bearer gw-token")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{Headers: tc.headers},
			}}
			headers, final := anthropicSimpleCapture(t, anthropicAuthModel(), opts)
			if headers == nil {
				t.Fatalf("request never went out: stop %s, err %q", final.StopReason, final.ErrorMessage)
			}
			if final.StopReason == ai.StopError {
				t.Fatalf("stream failed: %q", final.ErrorMessage)
			}
			if _, sent := headers["X-Api-Key"]; sent && tc.name != "x-api-key" {
				t.Fatalf("x-api-key = %q, want the header absent when auth is header-owned", headers.Get("x-api-key"))
			}
		})
	}
}

// TestAnthropicStreamSimpleCopilotHeaderOwnedAuthSendsNoBearer covers the
// branch that header-owned auth newly makes reachable on a github-copilot
// model. pi's copilot branch is `authToken: apiKey ?? null`, so an absent key
// sends NO Authorization at all and the request rides on whichever header
// authorized it — an empty "Bearer " would be a credential the SDK never emits.
func TestAnthropicStreamSimpleCopilotHeaderOwnedAuthSendsNoBearer(t *testing.T) {
	t.Setenv(ai.AnthropicAuthTokenEnv, "")

	model := anthropicAuthModel()
	model.Provider = "github-copilot"
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			Headers: ai.ProviderHeaders{"x-api-key": strHeader("gw-key")},
		},
	}}
	headers, final := anthropicSimpleCapture(t, model, opts)
	if headers == nil {
		t.Fatalf("request never went out: stop %s, err %q", final.StopReason, final.ErrorMessage)
	}
	if got := headers.Get("x-api-key"); got != "gw-key" {
		t.Fatalf("x-api-key = %q, want the caller's header", got)
	}
	if _, sent := headers["Authorization"]; sent {
		t.Fatalf("authorization = %q, want the header absent when there is no key to bear", headers.Get("authorization"))
	}
}

// TestAnthropicStreamSimpleRejectsEmptyAndSuppressedHeaders pins the other half
// of pi's hasHeader: a value that is null (a deletion marker) or blank is not a
// credential (`value !== null && value.trim().length > 0`).
func TestAnthropicStreamSimpleRejectsEmptyAndSuppressedHeaders(t *testing.T) {
	t.Setenv(ai.AnthropicAuthTokenEnv, "")

	cases := []struct {
		name    string
		headers ai.ProviderHeaders
	}{
		{name: "no headers at all", headers: nil},
		{name: "deletion marker", headers: ai.ProviderHeaders{"Authorization": nil, "cf-aig-authorization": nil}},
		{name: "blank value", headers: ai.ProviderHeaders{"x-api-key": strHeader("   ")}},
		{name: "unrelated header", headers: ai.ProviderHeaders{"x-foo": strHeader("y")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{Headers: tc.headers},
			}}
			model := anthropicAuthModel()
			model.BaseURL = "https://example.invalid"
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			types, final := drainSimple(StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts))
			if len(types) != 1 || types[0] != ai.EventError {
				t.Fatalf("events = %v, want a single error event", types)
			}
			if final.StopReason != ai.StopError || final.ErrorMessage != anthropicNoKeyErr {
				t.Fatalf("final = stop %s, err %q, want stop error / %q", final.StopReason, final.ErrorMessage, anthropicNoKeyErr)
			}
		})
	}
}

// TestAnthropicStreamSimpleAuthTokenStillAuthenticates guards the port's extra
// credential source. pi's api adapter never reads ANTHROPIC_AUTH_TOKEN — its
// provider resolver turns the env value into an Authorization header before the
// adapter sees it (providers/anthropic.ts, ported at ai/builtins_models.go:119)
// — but this port ALSO reads it inside the adapter (anthropic.go:512) so the
// compat path (ai.StreamSimple → withEnvAPIKeySimple, which deliberately leaves
// APIKey empty for it) authenticates. Widening the gate to pi's header set must
// not cost that.
func TestAnthropicStreamSimpleAuthTokenStillAuthenticates(t *testing.T) {
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			Env: map[string]string{ai.AnthropicAuthTokenEnv: "my-auth-token"},
		},
	}}
	model := anthropicAuthModel()
	model.Provider = "anthropic" // the adapter reads the token for this provider only
	headers, final := anthropicSimpleCapture(t, model, opts)
	if headers == nil {
		t.Fatalf("request never went out: stop %s, err %q", final.StopReason, final.ErrorMessage)
	}
	if got := headers.Get("authorization"); got != "Bearer my-auth-token" {
		t.Fatalf("authorization = %q, want the bearer token", got)
	}
	if _, sent := headers["X-Api-Key"]; sent {
		t.Fatalf("x-api-key = %q, want the header absent for auth-token auth", headers.Get("x-api-key"))
	}
}

// TestAnthropicStreamSimpleGatewayHeaderOwnedAuthSendsNoEmptyGatewayBearer
// covers the third branch header-owned auth newly makes reachable. Widening the
// gate to pi's header set lets a cloudflare-ai-gateway request through with no
// api key, and the adapter's header assembly then built the gateway bundle from that
// empty key — synthesizing `cf-aig-authorization: "Bearer "`, a credential pi
// never sends. pi's adapter never mentions Cloudflare at all: on the
// "API key or header-owned auth" branch it passes `apiKey: apiKey ?? null` and
// sends the caller's headers and nothing else (8b5899dce:packages/ai/src/api/
// anthropic-messages.ts:954-975). The gateway bundle is the port's own inline
// rendering of pi's resolver (the 2026-06-24 divergence), and that resolver
// produces the bundle only when it HAS a gateway credential — with none there is
// nothing to send and nothing to suppress.
func TestAnthropicStreamSimpleGatewayHeaderOwnedAuthSendsNoEmptyGatewayBearer(t *testing.T) {
	t.Setenv(ai.AnthropicAuthTokenEnv, "")

	model := anthropicAuthModel()
	model.Provider = "cloudflare-ai-gateway"
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			Headers: ai.ProviderHeaders{"Authorization": strHeader("Bearer caller-token")},
		},
	}}
	headers, final := anthropicSimpleCapture(t, model, opts)
	if headers == nil {
		t.Fatalf("request never went out: stop %s, err %q", final.StopReason, final.ErrorMessage)
	}
	if got := headers.Get("authorization"); got != "Bearer caller-token" {
		t.Fatalf("authorization = %q, want the caller's header", got)
	}
	if _, sent := headers["Cf-Aig-Authorization"]; sent {
		t.Fatalf("cf-aig-authorization = %q, want the header absent when there is no gateway credential",
			headers.Get("cf-aig-authorization"))
	}
	// The suppression markers ride with the bundle; without a credential there is
	// no bundle, so an unrelated auth header must not be deleted either.
	if _, sent := headers["X-Api-Key"]; sent {
		t.Fatalf("x-api-key = %q, want no key emitted", headers.Get("x-api-key"))
	}
}

// TestAnthropicStreamSimpleMissingKeyPreemptsOtherSetupFailures gives anthropic
// the control-backed precedence case google and the openai adapters already
// have. 8b5899dce's payload for this port is precedence — upstream re-attached
// the assertion at the TOP of streamSimple (anthropic-messages.ts:854), so the
// auth failure must beat every other setup failure on that path. Without this,
// the gate could be moved below buildAnthropicParams and the whole suite would
// stay green.
//
// The control asserts the error the SAME request reports when a key IS supplied,
// so the precedence assertion cannot pass vacuously.
func TestAnthropicStreamSimpleMissingKeyPreemptsOtherSetupFailures(t *testing.T) {
	t.Setenv(ai.AnthropicAuthTokenEnv, "")

	// A tool that REQUIRES json-schema constrained sampling against a model whose
	// compat does not support strict tools: convertAnthropicTools fails, so
	// buildAnthropicParams returns an error well after the auth gate.
	strictTool := ai.Tool{
		Name: "needs-strict", Description: "strict tool",
		Parameters: ai.Object(ai.Prop("query", ai.String())),
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{
			Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingRequire,
		},
	}
	const strictErr = `Tool "needs-strict" requires JSON-schema constrained sampling, but strict tools are unsupported.`

	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{strictTool}}
	newModel := func() *ai.Model {
		m := anthropicAuthModel()
		m.BaseURL = "https://example.invalid"
		return m
	}

	// Control: with a key, the params failure is what surfaces.
	ctrlOpts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"},
	}}
	_, ctrl := drainSimple(StreamSimpleAnthropic(context.Background(), newModel(), ai.NormalizeContext(req), ctrlOpts))
	if ctrl.ErrorMessage != strictErr {
		t.Fatalf("control error = %q, want %q — the precedence case below would be vacuous", ctrl.ErrorMessage, strictErr)
	}

	// Without a key, the auth failure must preempt it.
	types, final := drainSimple(StreamSimpleAnthropic(context.Background(), newModel(), ai.NormalizeContext(req), &ai.SimpleStreamOptions{}))
	if len(types) != 1 || types[0] != ai.EventError {
		t.Fatalf("events = %v, want a single error event", types)
	}
	if final.StopReason != ai.StopError || final.ErrorMessage != anthropicNoKeyErr {
		t.Fatalf("final = stop %s, err %q, want stop error / %q", final.StopReason, final.ErrorMessage, anthropicNoKeyErr)
	}
}
