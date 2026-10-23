package providers

// Upstream 4e69b0c28 "feat(ai): preserve Anthropic per-turn thinking effort".
//
// pi migrated from `client.messages.create({...params, stream:true})` to
// `client.beta.messages.create(params, ...)`. Three wire surfaces move with it,
// and only ONE of them has an oracle in difftest/ (which compares request BODIES
// and never headers or URLs):
//
//   - the request URL gains `?beta=true`   — no oracle, pinned here
//   - `anthropic-beta` is derived from the body's `betas` and applied as a
//     PER-REQUEST header that beats every default header — no oracle, pinned here
//   - `betas` itself now travels in the params object `onPayload` observes,
//     so difftest does see it
//
// Every literal below is asserted against upstream sha 64eeb82a4
// (packages/ai/src/api/anthropic-messages.ts).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicCaptureRequest is anthropicCapture plus the request URI, which is
// where `?beta=true` lives. anthropicCapture cannot report it: it hands back
// only headers and the decoded body.
func anthropicCaptureRequest(t *testing.T, model *ai.Model, req ai.Context, opts *AnthropicOptions, sse string) (string, http.Header, map[string]any) {
	t.Helper()
	var gotURI string
	var gotHeaders http.Header
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
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
	return gotURI, gotHeaders, gotBody
}

func anthropicPlainModel() *ai.Model {
	return &ai.Model{
		ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096,
	}
}

func anthropicHelloContext() ai.Context {
	return ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
}

func apiKeyOptions(key string) *AnthropicOptions {
	return &AnthropicOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: key},
	}}
}

// bodyStrings reads a JSON array of strings out of a decoded body.
func bodyStrings(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		return nil
	}
	// A body captured through OnPayload still holds the Go-native []string the
	// request builder wrote; one read back off the wire holds decoded JSON.
	if native, ok := raw.([]string); ok {
		return native
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s is not an array: %#v", key, raw)
	}
	out := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s[%d] is not a string: %#v", key, i, item)
		}
		out[i] = s
	}
	return out
}

// anthropicCaptureParams records the params object as it looked when onPayload
// saw it — before the SDK's beta namespace lifts `betas` out of it. That object
// is the surface difftest compares, so it is where a betas assertion belongs;
// the wire body no longer carries the key at all.
func anthropicCaptureParams(t *testing.T, model *ai.Model, req ai.Context, opts *AnthropicOptions, sse string) (map[string]any, http.Header) {
	t.Helper()
	var params map[string]any
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		params, _ = body.(map[string]any)
		return nil, nil
	}
	_, headers, _ := anthropicCaptureRequest(t, model, req, opts, sse)
	return params, headers
}

// The beta namespace changes the request URL, not just the header. pi calls
// `client.beta.messages.create(...)`, and the SDK posts that to
// `/v1/messages?beta=true` (resources/beta/messages/messages.js). A port that
// keeps posting to a bare `/v1/messages` sends a request pi never sends, and no
// body-diff harness can see it.
func TestAnthropicBetaNamespaceRequestURL(t *testing.T) {
	uri, _, _ := anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), apiKeyOptions("test-key"), anthropicSSE)
	if uri != "/v1/messages?beta=true" {
		t.Fatalf("request URI = %q, want %q", uri, "/v1/messages?beta=true")
	}
}

// The betas list now travels in the params object (so onPayload and difftest
// see it) and the SDK lifts it out of the body into the header. Both halves are
// asserted: the body must carry `betas`, the wire body must NOT.
func TestAnthropicBetasTravelInParamsAndHeader(t *testing.T) {
	model := anthropicPlainModel()
	// supportsEagerToolInputStreaming:false + tools => fine-grained beta.
	model.Compat = json.RawMessage(`{"supportsEagerToolInputStreaming":false}`)
	req := anthropicHelloContext()
	req.Tools = []ai.Tool{{Name: "read", Description: "read", Parameters: ai.Object(ai.Prop("p", ai.String()))}}

	var payload map[string]any
	opts := apiKeyOptions("test-key")
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		payload, _ = body.(map[string]any)
		return nil, nil
	}
	_, headers, wire := anthropicCaptureRequest(t, model, req, opts, anthropicSSE)

	if got := bodyStrings(t, payload, "betas"); !reflect.DeepEqual(got, []string{fineGrainedToolStreamBeta}) {
		t.Fatalf("onPayload betas = %#v, want [%q]", got, fineGrainedToolStreamBeta)
	}
	if _, present := wire["betas"]; present {
		t.Fatalf("betas must be stripped from the marshalled body, got %#v", wire["betas"])
	}
	if got := headers.Get("anthropic-beta"); got != fineGrainedToolStreamBeta {
		t.Fatalf("anthropic-beta = %q, want %q", got, fineGrainedToolStreamBeta)
	}
}

// getBetaFeatures pushes "claude-code-20250219" and "oauth-2025-04-20" together
// inside `if (isOAuthToken)`. Neither may appear on an api-key request.
func TestAnthropicOAuthBetasGatedOnOAuthToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model func() *ai.Model
		opts  func() *AnthropicOptions
	}{
		{"api-key", anthropicPlainModel, func() *AnthropicOptions { return apiKeyOptions("sk-ant-api-plain") }},
		{"auth-token", anthropicPlainModel, func() *AnthropicOptions {
			o := apiKeyOptions("")
			o.Env = map[string]string{"ANTHROPIC_AUTH_TOKEN": "at-secret"}
			return o
		}},
		{"github-copilot", func() *ai.Model {
			m := anthropicPlainModel()
			m.Provider = "github-copilot"
			return m
		}, func() *AnthropicOptions { return apiKeyOptions("sk-ant-oat-looks-like-oauth") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, headers, body := anthropicCaptureRequest(t, tc.model(), anthropicHelloContext(), tc.opts(), anthropicSSE)
			beta := headers.Get("anthropic-beta")
			for _, forbidden := range []string{"claude-code-20250219", "oauth-2025-04-20"} {
				if strings.Contains(beta, forbidden) {
					t.Fatalf("%s request leaked %q in anthropic-beta: %q", tc.name, forbidden, beta)
				}
				for _, b := range bodyStrings(t, body, "betas") {
					if b == forbidden {
						t.Fatalf("%s request leaked %q in betas", tc.name, forbidden)
					}
				}
			}
		})
	}
}

// getBetaFeatures narrowed the interleaved-thinking gate: it now requires
// `model.reasoning && options?.thinkingEnabled === true` on top of the old
// `(interleavedThinking ?? true) && !forceAdaptiveThinking`. A non-reasoning
// model, or a reasoning model with thinking left off, must not ask for it.
func TestAnthropicInterleavedBetaGate(t *testing.T) {
	reasoning := func(on bool) *ai.Model {
		m := anthropicPlainModel()
		m.Reasoning = on
		return m
	}
	thinking := func(provided, enabled bool) *AnthropicOptions {
		o := apiKeyOptions("test-key")
		o.ThinkingProvided, o.ThinkingEnabled = provided, enabled
		o.ThinkingBudgetTokens = 2048
		return o
	}
	// `(options.interleavedThinking ?? true)`: the option only ever turns the
	// beta OFF, and its absence is not the same as an explicit false.
	withInterleaved := func(o *AnthropicOptions, v bool) *AnthropicOptions {
		o.InterleavedThinking = &v
		return o
	}
	for _, tc := range []struct {
		name string
		mdl  *ai.Model
		opts *AnthropicOptions
		want bool
	}{
		{"reasoning+thinking on", reasoning(true), thinking(true, true), true},
		{"reasoning, thinking unset", reasoning(true), thinking(false, false), false},
		{"reasoning, thinking explicitly off", reasoning(true), thinking(true, false), false},
		{"non-reasoning, thinking on", reasoning(false), thinking(true, true), false},
		{"interleavedThinking explicitly false", reasoning(true), withInterleaved(thinking(true, true), false), false},
		{"interleavedThinking explicitly true", reasoning(true), withInterleaved(thinking(true, true), true), true},
		{"forceAdaptiveThinking model", func() *ai.Model {
			m := reasoning(true)
			m.Compat = json.RawMessage(`{"forceAdaptiveThinking":true}`)
			return m
		}(), thinking(true, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, headers, _ := anthropicCaptureRequest(t, tc.mdl, anthropicHelloContext(), tc.opts, anthropicSSE)
			got := strings.Contains(headers.Get("anthropic-beta"), interleavedThinkingBeta)
			if got != tc.want {
				t.Fatalf("interleaved beta present = %v, want %v (header %q)", got, tc.want, headers.Get("anthropic-beta"))
			}
		})
	}
}

// An `anthropic-beta` header configured on the model or by the consumer now
// REPLACES the computed feature set (it is split, trimmed, deduped) instead of
// merely sitting alongside it in the default headers.
func TestAnthropicConfiguredBetaHeaderReplacesFeatures(t *testing.T) {
	model := anthropicPlainModel()
	model.Headers = ai.ProviderHeaders{"Anthropic-Beta": ai.HeaderValue(" alpha , beta ,, alpha ")}
	opts := apiKeyOptions("sk-ant-oat-secret") // OAuth betas would otherwise apply
	params, headers := anthropicCaptureParams(t, model, anthropicHelloContext(), opts, anthropicSSE)

	if got := bodyStrings(t, params, "betas"); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("betas = %#v, want [alpha beta]", got)
	}
	if got := headers.Get("anthropic-beta"); got != "alpha,beta" {
		t.Fatalf("anthropic-beta = %q, want %q", got, "alpha,beta")
	}
}

// A deletion marker (pi's `null`) short-circuits getBetaFeatures to [], and the
// marker also removes the default header: the request carries no anthropic-beta
// at all, not even the OAuth pair.
func TestAnthropicConfiguredBetaNullSuppressesEverything(t *testing.T) {
	model := anthropicPlainModel()
	model.Headers = ai.ProviderHeaders{"anthropic-beta": nil}
	_, headers, body := anthropicCaptureRequest(t, model, anthropicHelloContext(), apiKeyOptions("sk-ant-oat-secret"), anthropicSSE)

	if _, present := headers["Anthropic-Beta"]; present {
		t.Fatalf("anthropic-beta must be absent, got %q", headers.Get("anthropic-beta"))
	}
	if _, present := body["betas"]; present {
		t.Fatalf("betas must be absent from the wire body, got %#v", body["betas"])
	}
}

// The per-request header derived from `betas` wins over every default header,
// including one the CONSUMER set — the SDK passes it as a request header and
// buildHeaders lets those beat client defaults. Spelling it differently must not
// let the default win by slot order.
//
// The consumer's value is deliberately one that does NOT survive getBetaFeatures
// unchanged — it is padded and duplicated — so the normalized betas string and
// the raw default-header write differ, and writing the betas header before
// pi's object instead of after it is visible here.
func TestAnthropicBetasHeaderBeatsConsumerSpelling(t *testing.T) {
	model := anthropicPlainModel()
	model.Headers = ai.ProviderHeaders{"anthropic-beta": ai.HeaderValue("from-model")}
	opts := apiKeyOptions("test-key")
	opts.Headers = ai.ProviderHeaders{"ANTHROPIC-BETA": ai.HeaderValue(" from-consumer ,from-consumer")}
	_, headers, _ := anthropicCaptureRequest(t, model, anthropicHelloContext(), opts, anthropicSSE)
	if got := headers.Get("anthropic-beta"); got != "from-consumer" {
		t.Fatalf("anthropic-beta = %q, want %q (the normalized betas of the last source scanned)", got, "from-consumer")
	}
}

// getBetaFeatures pushes its six sources in a fixed order and the SDK joins the
// list with "," in that order, so the header is an ordered string rather than a
// set. Only a request that arranges ALL of them at once pins the OAuth pair's
// position, which is first, and the native tool-changes beta's, which is last
// (upstream 9e05370b2; inline-tools since b271b0a52).
func TestAnthropicBetaOrderAcrossEverySource(t *testing.T) {
	model := anthropicMidConvoModel()
	model.Compat = json.RawMessage(`{"supportsMidConvoEffort":true,"supportsEagerToolInputStreaming":false,
		"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true,
		"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8"}]}`)
	req := anthropicHelloContext()
	req.Tools = []ai.Tool{{Name: "read", Description: "read", Parameters: ai.Object(ai.Prop("p", ai.String()))}}
	opts := apiKeyOptions("sk-ant-oat-secret")
	opts.ThinkingProvided, opts.ThinkingEnabled = true, true

	params, headers := anthropicCaptureParams(t, model, req, opts, anthropicSSE)
	want := []string{
		"claude-code-20250219", "oauth-2025-04-20",
		fineGrainedToolStreamBeta,
		interleavedThinkingBeta,
		serverSideFallbackBeta,
		midConvoOutputConfigBeta, thinkingBindingBeta,
		"inline-tools-2026-09-15",
	}
	if got := bodyStrings(t, params, "betas"); !reflect.DeepEqual(got, want) {
		t.Fatalf("betas = %#v, want %#v", got, want)
	}
	if got := headers.Get("anthropic-beta"); got != strings.Join(want, ",") {
		t.Fatalf("anthropic-beta = %q, want %q", got, strings.Join(want, ","))
	}
}

// `betas: []` is NOT the same as no betas. The SDK's presence test is
// `betas?.toString() != null`, and `[].toString()` is "" — which is not null —
// so the request carries a PRESENT, EMPTY `anthropic-beta`, and because that is
// a per-request header it deletes the one the model's default headers
// configured. buildAnthropicParams never emits an empty list, so the case
// belongs to onPayload, the surface this slice exposes `betas` on.
func TestAnthropicEmptyBetasSendsPresentEmptyHeader(t *testing.T) {
	model := anthropicPlainModel()
	model.Headers = ai.ProviderHeaders{"anthropic-beta": ai.HeaderValue("from-model")}
	_, headers, _ := anthropicCaptureRequest(t, model, anthropicHelloContext(),
		anthropicOverrideBetas(apiKeyOptions("test-key"), []any{}), anthropicSSE)

	values, present := headers["Anthropic-Beta"]
	if !present {
		t.Fatalf("anthropic-beta must be present and empty, got no header at all")
	}
	if len(values) != 1 || values[0] != "" {
		t.Fatalf("anthropic-beta = %#v, want exactly one empty value", values)
	}
}

// The other half of the presence rule: `?.` short-circuits on null, so a null
// `betas` sends no header at all and leaves the configured default standing.
func TestAnthropicNullBetasLeavesConfiguredHeader(t *testing.T) {
	model := anthropicPlainModel()
	model.Headers = ai.ProviderHeaders{"anthropic-beta": ai.HeaderValue("from-model")}
	_, headers, _ := anthropicCaptureRequest(t, model, anthropicHelloContext(),
		anthropicOverrideBetas(apiKeyOptions("test-key"), nil), anthropicSSE)
	if got := headers.Get("anthropic-beta"); got != "from-model" {
		t.Fatalf("anthropic-beta = %q, want the configured default %q", got, "from-model")
	}
}

// anthropicOverrideBetas installs an onPayload hook that copies the params and
// replaces `betas`, the only way to reach a value buildAnthropicParams does not
// produce.
func anthropicOverrideBetas(opts *AnthropicOptions, betas any) *AnthropicOptions {
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		next := map[string]any{}
		for k, v := range body.(map[string]any) {
			next[k] = v
		}
		next["betas"] = betas
		return next, nil
	}
	return opts
}

// --- mid-conversation effort (compat.supportsMidConvoEffort) ---

func anthropicMidConvoModel() *ai.Model {
	m := anthropicPlainModel()
	m.Reasoning = true
	m.Compat = json.RawMessage(`{"supportsMidConvoEffort":true}`)
	return m
}

// A managed-effort model always sends adaptive thinking with drop_block
// binding, a literal `output_config.effort:"low"`, both new betas, and NEVER a
// temperature — regardless of what thinking options the caller passed.
func TestAnthropicMidConvoEffortRequestShape(t *testing.T) {
	opts := apiKeyOptions("test-key")
	temp := 0.7
	opts.Temperature = &temp
	opts.Effort = "low"
	params, headers := anthropicCaptureParams(t, anthropicMidConvoModel(), anthropicHelloContext(), opts, anthropicSSE)
	body := params

	wantThinking := map[string]any{
		"type":    "adaptive",
		"display": "summarized",
		"block_binding": map[string]any{
			"prefix_mismatch_behavior": "drop_block",
		},
	}
	if !reflect.DeepEqual(body["thinking"], wantThinking) {
		t.Fatalf("thinking = %#v, want %#v", body["thinking"], wantThinking)
	}
	// Literal "low", NOT options.effort: the per-turn effort rides in the
	// trailing system message instead (upstream 64eeb82a4 buildParams). The
	// top-level effort is part of the cached prompt so it stays fixed, and it is
	// "low" because an organization's effort cap rejects any level above it
	// (upstream e3bc05e87, #10063).
	if !reflect.DeepEqual(body["output_config"], map[string]any{"effort": "low"}) {
		t.Fatalf("output_config = %#v, want {effort: low}", body["output_config"])
	}
	if _, present := body["temperature"]; present {
		t.Fatalf("temperature must be suppressed on managed-effort models, got %#v", body["temperature"])
	}
	betas := bodyStrings(t, body, "betas")
	for _, want := range []string{"mid-conversation-output-config-2026-07-01", "thinking-binding-controls-2026-08-01"} {
		found := false
		for _, b := range betas {
			if b == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("betas %#v missing %q", betas, want)
		}
		if !strings.Contains(headers.Get("anthropic-beta"), want) {
			t.Fatalf("anthropic-beta %q missing %q", headers.Get("anthropic-beta"), want)
		}
	}
}

// Effort-only system messages: one before every assistant turn that recorded a
// provider-native level for THIS api+provider, and one at the very end carrying
// the active effort (`options.effort ?? "high"`).
func TestAnthropicMidConvoEffortInsertsSystemMessages(t *testing.T) {
	model := anthropicMidConvoModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("one", 1),
		&ai.AssistantMessage{
			Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
			ProviderThinkingLevel: "max",
			Content:               ai.ContentList{ai.TextContent{Text: "first"}},
		},
		ai.NewUserText("two", 2),
		&ai.AssistantMessage{
			// No recorded level: no system message may precede it.
			Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
			Content: ai.ContentList{ai.TextContent{Text: "second"}},
		},
		ai.NewUserText("three", 3),
	}}
	opts := apiKeyOptions("test-key")
	opts.Effort = "low"
	_, _, body := anthropicCaptureRequest(t, model, req, opts, anthropicSSE)

	msgs, _ := body["messages"].([]any)
	var shape []string
	for _, m := range msgs {
		mm := m.(map[string]any)
		role, _ := mm["role"].(string)
		if role != "system" {
			shape = append(shape, role)
			continue
		}
		oc, ok := mm["output_config"].(map[string]any)
		if !ok {
			t.Fatalf("system message without output_config: %#v", mm)
		}
		content, ok := mm["content"].([]any)
		if !ok || len(content) != 0 {
			t.Fatalf("effort system message must carry an empty content array, got %#v", mm["content"])
		}
		shape = append(shape, "system:"+oc["effort"].(string))
	}
	want := []string{"user", "system:max", "assistant", "user", "assistant", "user", "system:low"}
	if !reflect.DeepEqual(shape, want) {
		t.Fatalf("message shape = %#v, want %#v", shape, want)
	}
}

// The level is only honoured when the recorded turn came from the same api AND
// the same provider, and only for a value in the AnthropicEffort union.
func TestAnthropicMidConvoEffortIgnoresForeignLevels(t *testing.T) {
	model := anthropicMidConvoModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("one", 1),
		&ai.AssistantMessage{ // foreign provider
			Api: ai.APIAnthropicMessages, Provider: "openrouter", Model: "claude-test",
			ProviderThinkingLevel: "max", Content: ai.ContentList{ai.TextContent{Text: "a"}},
		},
		&ai.AssistantMessage{ // not an AnthropicEffort value
			Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
			ProviderThinkingLevel: "ultra", Content: ai.ContentList{ai.TextContent{Text: "b"}},
		},
		&ai.AssistantMessage{ // right provider and a valid level, but a foreign api
			Api: ai.APIOpenAICompletions, Provider: "anthropic", Model: "claude-test",
			ProviderThinkingLevel: "max", Content: ai.ContentList{ai.TextContent{Text: "c"}},
		},
	}}
	_, _, body := anthropicCaptureRequest(t, model, req, apiKeyOptions("test-key"), anthropicSSE)
	msgs, _ := body["messages"].([]any)
	systems := 0
	for _, m := range msgs {
		if m.(map[string]any)["role"] == "system" {
			systems++
		}
	}
	if systems != 1 {
		t.Fatalf("expected only the trailing active-effort system message, got %d", systems)
	}
}

// An unmanaged model keeps the old message list: no system messages, no
// output_config, and temperature still flows.
func TestAnthropicWithoutMidConvoEffortUnchanged(t *testing.T) {
	opts := apiKeyOptions("test-key")
	temp := 0.5
	opts.Temperature = &temp
	_, _, body := anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), opts, anthropicSSE)
	for _, m := range body["messages"].([]any) {
		if m.(map[string]any)["role"] == "system" {
			t.Fatalf("unmanaged model must not get effort system messages: %#v", body["messages"])
		}
	}
	if _, present := body["output_config"]; present {
		t.Fatalf("unmanaged model must not get output_config: %#v", body["output_config"])
	}
	if body["temperature"] != 0.5 {
		t.Fatalf("temperature = %#v, want 0.5", body["temperature"])
	}
}

// The response records the effort it was produced under, so the next turn can
// replay it. Absent entirely for unmanaged models.
func TestAnthropicProviderThinkingLevelOnOutput(t *testing.T) {
	opts := apiKeyOptions("test-key")
	opts.Effort = "xhigh"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()

	model := anthropicMidConvoModel()
	model.BaseURL = server.URL
	got := StreamAnthropic(context.Background(), model, ai.NormalizeContext(anthropicHelloContext()), opts).Result()
	if got.ProviderThinkingLevel != "xhigh" {
		t.Fatalf("providerThinkingLevel = %q, want %q", got.ProviderThinkingLevel, "xhigh")
	}

	// Default when the caller supplied no effort.
	defaulted := anthropicMidConvoModel()
	defaulted.BaseURL = server.URL
	got = StreamAnthropic(context.Background(), defaulted, ai.NormalizeContext(anthropicHelloContext()), apiKeyOptions("k")).Result()
	if got.ProviderThinkingLevel != "high" {
		t.Fatalf("defaulted providerThinkingLevel = %q, want %q", got.ProviderThinkingLevel, "high")
	}

	unmanaged := anthropicPlainModel()
	unmanaged.BaseURL = server.URL
	got = StreamAnthropic(context.Background(), unmanaged, ai.NormalizeContext(anthropicHelloContext()), apiKeyOptions("k")).Result()
	if got.ProviderThinkingLevel != "" {
		t.Fatalf("unmanaged providerThinkingLevel = %q, want empty", got.ProviderThinkingLevel)
	}
}

// --- fallback content blocks ---

const anthropicFallbackFirstSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"fallback","model":"claude-opus-4-8"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

const anthropicFallbackMidOutputSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"fallback","model":"claude-opus-4-8"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

func streamAnthropicAgainst(t *testing.T, model *ai.Model, sse string) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	return StreamAnthropic(context.Background(), model, ai.NormalizeContext(anthropicHelloContext()), apiKeyOptions("test-key")).Result()
}

// A leading `fallback` block is skipped, and the text after it still lands at
// content index 0 — the block is not materialized at all.
func TestAnthropicFallbackBlockSkippedBeforeContent(t *testing.T) {
	got := streamAnthropicAgainst(t, anthropicPlainModel(), anthropicFallbackFirstSSE)
	if got.StopReason != ai.StopStop {
		t.Fatalf("stopReason = %q (%s), want stop", got.StopReason, got.ErrorMessage)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content = %#v, want one text block", got.Content)
	}
	if text, ok := got.Content[0].(ai.TextContent); !ok || text.Text != "hi" {
		t.Fatalf("content[0] = %#v, want text 'hi'", got.Content[0])
	}
}

// A `fallback` block AFTER content means Anthropic swapped models mid-output,
// which pi refuses outright.
func TestAnthropicFallbackBlockMidOutputFailsStream(t *testing.T) {
	got := streamAnthropicAgainst(t, anthropicPlainModel(), anthropicFallbackMidOutputSSE)
	if got.StopReason != ai.StopError {
		t.Fatalf("stopReason = %q, want error", got.StopReason)
	}
	if got.ErrorMessage != "Anthropic performed an unsupported mid-output model fallback" {
		t.Fatalf("errorMessage = %q", got.ErrorMessage)
	}
}

// --- input transformations diagnostic ---

const anthropicInputTransformationsSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","input_transformations":[{"type":"thinking_dropped","path":"messages.1","reason":"prefix_mismatch"}],"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","input_transformations":[{"type":"thinking_dropped","path":"messages.3"}],"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// message_delta's list REPLACES message_start's (pi assigns, it does not
// concatenate), and absent optional fields are dropped rather than emitted empty.
func TestAnthropicInputTransformationsDiagnostic(t *testing.T) {
	got := streamAnthropicAgainst(t, anthropicPlainModel(), anthropicInputTransformationsSSE)
	if got.StopReason != ai.StopStop {
		t.Fatalf("stopReason = %q (%s)", got.StopReason, got.ErrorMessage)
	}
	if len(got.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want exactly one", got.Diagnostics)
	}
	d := got.Diagnostics[0]
	if d.Type != "anthropic_input_transformations" {
		t.Fatalf("diagnostic type = %q", d.Type)
	}
	// pi's details literal: {type, path, reason} in that order.
	assertDiagnosticDetailsJSON(t, d, `{"transformations":[{"type":"thinking_dropped","path":"messages.3"}]}`)
}

// assertDiagnosticDetailsJSON requires d's details to marshal to want, the
// JSON.stringify text of pi's details: its key order and its numbers.
func assertDiagnosticDetailsJSON(t *testing.T, d ai.Diagnostic, want string) {
	t.Helper()
	got, err := json.Marshal(d.Details)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	if string(got) != want {
		t.Fatalf("details = %s, want pi's %s", got, want)
	}
}

// An empty `input_transformations` array clears an earlier non-empty one and
// leaves no diagnostic.
func TestAnthropicInputTransformationsClearedByEmptyArray(t *testing.T) {
	sse := strings.Replace(anthropicInputTransformationsSSE,
		`"input_transformations":[{"type":"thinking_dropped","path":"messages.3"}],`,
		`"input_transformations":[],`, 1)
	got := streamAnthropicAgainst(t, anthropicPlainModel(), sse)
	if len(got.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", got.Diagnostics)
	}
}

// pi's guard is `Array.isArray(transformations)` and nothing else: EVERY array
// is accepted and replaces the list, and each entry contributes whatever
// `.type/.path/.reason` it happens to carry — a non-object entry contributing
// nothing at all. Rejecting a wrong-shaped array would not cost that array its
// entry, it would leave the previous list standing and un-retract it, which is
// the opposite of what pi does.
func TestAnthropicInputTransformationsAcceptAnyArray(t *testing.T) {
	sse := strings.Replace(anthropicInputTransformationsSSE,
		`"input_transformations":[{"type":"thinking_dropped","path":"messages.3"}],`,
		`"input_transformations":[{"type":5,"reason":null},7,{"path":{"nested":true}}],`, 1)
	got := streamAnthropicAgainst(t, anthropicPlainModel(), sse)
	if len(got.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want exactly one (the message_delta list replaces message_start's)", got.Diagnostics)
	}
	assertDiagnosticDetailsJSON(t, got.Diagnostics[0], `{"transformations":[{"type":5},{},{"path":{"nested":true}}]}`)
}

// A non-array value is the only thing the guard rejects, and rejecting it leaves
// the previous list standing (pi never assigns).
func TestAnthropicInputTransformationsNonArrayKeepsPreviousList(t *testing.T) {
	sse := strings.Replace(anthropicInputTransformationsSSE,
		`"input_transformations":[{"type":"thinking_dropped","path":"messages.3"}],`,
		`"input_transformations":{"type":"thinking_dropped"},`, 1)
	got := streamAnthropicAgainst(t, anthropicPlainModel(), sse)
	if len(got.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want message_start's list intact", got.Diagnostics)
	}
	assertDiagnosticDetailsJSON(t, got.Diagnostics[0], `{"transformations":[{"type":"thinking_dropped","path":"messages.1","reason":"prefix_mismatch"}]}`)
}

// pi appends the diagnostic at the very END of the success path: the aborted,
// still-pending and errored-stop-reason throws all fire before
// appendAssistantMessageDiagnostic, so a refused turn carries transformations
// but no diagnostic.
func TestAnthropicInputTransformationsNotAppendedOnFailedTurn(t *testing.T) {
	sse := strings.Replace(anthropicInputTransformationsSSE,
		`"delta":{"stop_reason":"end_turn"}`, `"delta":{"stop_reason":"refusal"}`, 1)
	got := streamAnthropicAgainst(t, anthropicPlainModel(), sse)
	if got.StopReason != ai.StopError {
		t.Fatalf("stopReason = %q, want error", got.StopReason)
	}
	if len(got.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none on a turn that never reached the append", got.Diagnostics)
	}
}

// No transformations at all: no diagnostic.
func TestAnthropicNoInputTransformationsNoDiagnostic(t *testing.T) {
	got := streamAnthropicAgainst(t, anthropicPlainModel(), anthropicSSE)
	for _, d := range got.Diagnostics {
		if d.Type == "anthropic_input_transformations" {
			t.Fatalf("unexpected diagnostic %#v", d)
		}
	}
}

// --- onPayload ---

// pi re-forces `stream: true` after an onPayload override: `params = {...next,
// stream: true}`. A hook that drops or falsifies the flag cannot turn the
// request non-streaming.
func TestAnthropicOnPayloadCannotDisableStreaming(t *testing.T) {
	opts := apiKeyOptions("test-key")
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		next := map[string]any{}
		for k, v := range body.(map[string]any) {
			next[k] = v
		}
		next["stream"] = false
		return next, nil
	}
	_, _, body := anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), opts, anthropicSSE)
	if body["stream"] != true {
		t.Fatalf("stream = %#v, want true (re-forced after onPayload)", body["stream"])
	}
}

// The params the request is built from are a COPY of what the hook returned:
// pi's `{...nextParams, stream: true}` never writes through to the object the
// hook still owns. A hook that hands back the very map it was given must not see
// `stream` re-forced inside it, and a hook that returns a nil map gets pi's
// `{...null, stream: true}` — a body of just the re-forced flag — rather than a
// write to a nil map.
func TestAnthropicOnPayloadResultIsNotWrittenThrough(t *testing.T) {
	opts := apiKeyOptions("test-key")
	var returned map[string]any
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		returned = map[string]any{}
		for k, v := range body.(map[string]any) {
			returned[k] = v
		}
		returned["stream"] = false
		return returned, nil
	}
	_, _, body := anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), opts, anthropicSSE)
	if body["stream"] != true {
		t.Fatalf("wire stream = %#v, want true", body["stream"])
	}
	if returned["stream"] != false {
		t.Fatalf("the hook's own map was mutated: stream = %#v, want the false it set", returned["stream"])
	}

	nilOpts := apiKeyOptions("test-key")
	nilOpts.OnPayload = func(any, *ai.Model) (any, error) {
		var none map[string]any
		return none, nil
	}
	_, _, body = anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), nilOpts, anthropicSSE)
	if !reflect.DeepEqual(body, map[string]any{"stream": true}) {
		t.Fatalf("body = %#v, want just the re-forced stream flag", body)
	}
}

// The beta namespace rewrites a deprecated top-level `output_format` into
// `output_config.format` on the way into the request, merging it into whatever
// output_config already carries (transformOutputFormat, @anthropic-ai/sdk
// resources/beta/messages/messages.js). pi emits neither key, so only a hook
// reaches this.
func TestAnthropicOutputFormatMovesIntoOutputConfig(t *testing.T) {
	opts := apiKeyOptions("test-key")
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		next := map[string]any{}
		for k, v := range body.(map[string]any) {
			next[k] = v
		}
		next["output_format"] = map[string]any{"type": "json_schema"}
		next["output_config"] = map[string]any{"effort": "low"}
		return next, nil
	}
	_, _, body := anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), opts, anthropicSSE)
	if _, present := body["output_format"]; present {
		t.Fatalf("output_format must not reach the wire, got %#v", body["output_format"])
	}
	want := map[string]any{"effort": "low", "format": map[string]any{"type": "json_schema"}}
	if !reflect.DeepEqual(body["output_config"], want) {
		t.Fatalf("output_config = %#v, want %#v", body["output_config"], want)
	}
}

// Supplying both spellings is refused rather than silently preferring one, and
// the throw surfaces as a failed stream.
func TestAnthropicOutputFormatConflictFailsStream(t *testing.T) {
	opts := apiKeyOptions("test-key")
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		next := map[string]any{}
		for k, v := range body.(map[string]any) {
			next[k] = v
		}
		next["output_format"] = map[string]any{"type": "json_schema"}
		next["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema"}}
		return next, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused params object must not reach the network")
	}))
	defer server.Close()
	model := anthropicPlainModel()
	model.BaseURL = server.URL
	got := StreamAnthropic(context.Background(), model, ai.NormalizeContext(anthropicHelloContext()), opts).Result()
	want := "Both output_format and output_config.format were provided. " +
		"Please use only output_config.format (output_format is deprecated)."
	if got.StopReason != ai.StopError || got.ErrorMessage != want {
		t.Fatalf("stopReason = %q, errorMessage = %q, want error + %q", got.StopReason, got.ErrorMessage, want)
	}
}

// A hook that rewrites `betas` still steers the header, because the SDK reads
// the header off the params object it is handed.
func TestAnthropicOnPayloadBetasSteerHeader(t *testing.T) {
	opts := apiKeyOptions("test-key")
	opts.OnPayload = func(body any, _ *ai.Model) (any, error) {
		next := map[string]any{}
		for k, v := range body.(map[string]any) {
			next[k] = v
		}
		next["betas"] = []any{"hook-beta"}
		return next, nil
	}
	_, headers, body := anthropicCaptureRequest(t, anthropicPlainModel(), anthropicHelloContext(), opts, anthropicSSE)
	if got := headers.Get("anthropic-beta"); got != "hook-beta" {
		t.Fatalf("anthropic-beta = %q, want %q", got, "hook-beta")
	}
	if _, present := body["betas"]; present {
		t.Fatalf("betas must not reach the wire body")
	}
}

// --- boy scout: an empty transcript must send `messages: []`, not `null` ---

// pi seeds convertMessages with `const params: MessageParam[] = []`, so a
// transcript that converts to nothing still marshals as an empty ARRAY. Go's nil
// slice marshals as `null`, which is a different request body — Anthropic
// rejects it, and no test reached the case because every other fixture has at
// least one surviving message. Pre-existing; surfaced by rewriting this
// function's return for managed effort.
func TestAnthropicEmptyTranscriptSendsEmptyMessagesArray(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model func() *ai.Model
	}{
		{"unmanaged", anthropicPlainModel},
		{"managed effort", anthropicMidConvoModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A whitespace-only user turn converts to zero blocks and is dropped,
			// leaving nothing behind it.
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("   ", 1)}}
			_, _, body := anthropicCaptureRequest(t, tc.model(), req, apiKeyOptions("test-key"), anthropicSSE)
			msgs, ok := body["messages"].([]any)
			if !ok {
				t.Fatalf("messages = %#v, want a JSON array", body["messages"])
			}
			// The managed-effort model still gets its trailing active-effort message.
			want := 0
			if tc.name == "managed effort" {
				want = 1
			}
			if len(msgs) != want {
				t.Fatalf("messages = %#v, want %d entries", msgs, want)
			}
		})
	}
}
