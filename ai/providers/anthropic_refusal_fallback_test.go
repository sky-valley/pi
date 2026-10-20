package providers

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Server-side refusal fallback (upstream eb1f87fa9, 4809c2abc, ed867e909).
//
// ed867e909 withdrew the caller-facing option entirely: there is no
// `refusalFallbacks` on SimpleStreamOptions or AnthropicOptions any more, and no
// "default" literal. Everything below is driven by the model's catalog compat —
// `allowedFallbackModels` decides the request's `fallbacks` field, the beta
// header, and the pricing a response a fallback served is billed at.
//
// Most tests here build a model with explicit compat so each rule can be probed
// in isolation. The embedded catalog carried NO allowedFallbackModels until the
// 0.84.3 regen, which is the release that first ships them —
// TestAnthropicCatalogFallbacksAreLive pins that activation against the real
// catalog so the feature cannot silently go dormant again on a later regen.

// anthropicSSEWithModel is the shared fixture plus the `model` field real
// Anthropic responses always carry on message_start.
const anthropicSSEWithModel = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicSSESameModel is the same stream served by the requested model, i.e. a
// response no server-side fallback stood in for.
const anthropicSSESameModel = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicSSERepricedBack delivers message_start twice: once naming a fallback
// that served the response, then again naming the requested model. pi assigns
// usageModel in BOTH arms of its ternary, so the second one has to reprice back
// rather than leave the fallback's rates standing.
const anthropicSSERepricedBack = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicFallbackStub is a stub Anthropic endpoint replaying one SSE stream,
// along with the request it was called with. Read headers and body only once the
// stream has finished.
type anthropicFallbackStub struct {
	url     string
	headers http.Header
	body    map[string]any
}

// startAnthropicFallbackStub serves sse from an endpoint torn down with the test.
func startAnthropicFallbackStub(t *testing.T, sse string) *anthropicFallbackStub {
	t.Helper()
	stub := &anthropicFallbackStub{body: map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.headers = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &stub.body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

// anthropicFallbackModel is a claude-opus-5 pointed at the stub, priced at
// 100/200 per Mtok, carrying whatever compat blob the case needs.
func anthropicFallbackModel(url, compat string) *ai.Model {
	m := &ai.Model{
		ID: "claude-opus-5", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096, BaseURL: url,
		Cost: ai.ModelCost{Input: 100, Output: 200},
	}
	if compat != "" {
		m.Compat = json.RawMessage(compat)
	}
	return m
}

// runAnthropicFallbackRequest sends one plain request through the stub.
func runAnthropicFallbackRequest(t *testing.T, model *ai.Model) {
	t.Helper()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "k"
	StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()
}

// TestAnthropicFallbacksOnWire pins the request half: the `fallbacks` field is
// projected from the catalog's allowedFallbackModels down to `{model}` only —
// pi's explicit `.map(f => ({ model: f.model }))` — so neither the provider id
// nor the local pricing reaches Anthropic, and the order the catalog lists is
// the order Anthropic receives.
//
// The third entry names ANOTHER provider, which pins the asymmetry ed867e909
// introduced: the provider match it added guards the COST lookup only
// (TestAnthropicFallbackCosting's "another provider" case), while buildParams
// still maps every permitted entry onto the wire regardless of provider. Reusing
// the new provider match here — the natural tidy-up now that the field exists —
// must fail this test.
func TestAnthropicFallbacksOnWire(t *testing.T) {
	stub := startAnthropicFallbackStub(t, anthropicSSEWithModel)
	model := anthropicFallbackModel(stub.url, `{"allowedFallbackModels":[
		{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}},
		{"provider":"anthropic","model":"claude-sonnet-9","cost":{"input":1,"output":2,"cacheRead":0.1,"cacheWrite":1.25}},
		{"provider":"vertex","model":"claude-haiku-9","cost":{"input":0.8,"output":4,"cacheRead":0.08,"cacheWrite":1}}
	]}`)
	runAnthropicFallbackRequest(t, model)

	fallbacks, ok := stub.body["fallbacks"].([]any)
	if !ok {
		t.Fatalf("fallbacks missing or not an array: %#v", stub.body["fallbacks"])
	}
	want := []string{"claude-opus-4-8", "claude-sonnet-9", "claude-haiku-9"}
	if len(fallbacks) != len(want) {
		t.Fatalf("want %d fallback entries in catalog order, got %v", len(want), fallbacks)
	}
	for i, id := range want {
		entry, _ := fallbacks[i].(map[string]any)
		if len(entry) != 1 || entry["model"] != id {
			t.Fatalf("fallbacks[%d] must carry only {model: %s}, got %v", i, id, entry)
		}
	}

	// The beta rides on the same catalog list. Since upstream 4e69b0c28 the
	// interleaved-thinking beta needs a reasoning model with thinking explicitly
	// on, and this model is neither, so the fallback beta stands alone; the
	// request also carries no tools, so the fine-grained beta is out of play.
	// TestAnthropicFallbackBetaOrder pins the full three-beta string.
	if got := stub.headers.Get("anthropic-beta"); got != serverSideFallbackBeta {
		t.Fatalf("anthropic-beta = %q, want %q", got, serverSideFallbackBeta)
	}
}

// TestAnthropicFallbackBetaOrder pins the fallback beta's position in the FULL
// beta list, which no other test reaches: pi pushes fine-grained tool streaming,
// then interleaved thinking, then the server-side fallback (getBetaFeatures,
// upstream 4e69b0c28). The first needs both a tool in the context and a model
// that declares no eager tool-input streaming; the second — since 4e69b0c28 —
// needs a reasoning model asked to think, so this is the one test that arranges
// all three at once.
func TestAnthropicFallbackBetaOrder(t *testing.T) {
	stub := startAnthropicFallbackStub(t, anthropicSSEWithModel)
	model := anthropicFallbackModel(stub.url, `{"supportsEagerToolInputStreaming":false,"allowedFallbackModels":[
		{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}
	]}`)
	model.Reasoning = true
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools:    []ai.Tool{{Name: "read", Description: "read a file", Parameters: ai.Object(ai.Prop("p", ai.String()))}},
	}
	opts := &ai.SimpleStreamOptions{Reasoning: ai.ThinkingMedium}
	opts.APIKey = "k"
	StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()

	wantBeta := strings.Join([]string{fineGrainedToolStreamBeta, interleavedThinkingBeta, serverSideFallbackBeta}, ",")
	if got := stub.headers.Get("anthropic-beta"); got != wantBeta {
		t.Fatalf("anthropic-beta = %q, want %q", got, wantBeta)
	}
}

// TestAnthropicNoFallbacksWithoutCatalogTargets pins the other side of the gate:
// a model whose compat lists no permitted targets must omit `fallbacks` entirely
// — Anthropic rejects the field for such models — and must not ask for the beta.
// Since ed867e909 there is no option that could turn either on.
func TestAnthropicNoFallbacksWithoutCatalogTargets(t *testing.T) {
	cases := []struct {
		name   string
		compat string
	}{
		{name: "no compat at all"},
		{name: "compat without the key", compat: `{"supportsTemperature":true}`},
		{name: "empty target list", compat: `{"allowedFallbackModels":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := startAnthropicFallbackStub(t, anthropicSSEWithModel)
			runAnthropicFallbackRequest(t, anthropicFallbackModel(stub.url, tc.compat))

			if _, has := stub.body["fallbacks"]; has {
				t.Fatalf("fallbacks must be omitted with no permitted targets: %v", stub.body["fallbacks"])
			}
			if got := stub.headers.Get("anthropic-beta"); strings.Contains(got, serverSideFallbackBeta) {
				t.Fatalf("fallback beta must not be sent with no permitted targets: %q", got)
			}
		})
	}
}

// message_start reports the model Anthropic actually served, which is how a
// server-side fallback becomes visible on the returned message: as
// responseModel, while model keeps the requested id so the message still replays
// as the requested model's own (upstream 1283afd0d). This is independent of the
// catalog: pi records any served model that differs, priced or not.
func TestAnthropicCapturesServedModel(t *testing.T) {
	stub := startAnthropicFallbackStub(t, anthropicSSEWithModel)
	model := anthropicFallbackModel(stub.url, "")
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "k"
	msg := StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts).Result()

	if msg.Model != model.ID {
		t.Fatalf("want the requested model %s, got %q", model.ID, msg.Model)
	}
	if msg.ResponseModel != "claude-opus-4-8" {
		t.Fatalf("want the served model claude-opus-4-8 as responseModel, got %q", msg.ResponseModel)
	}
}

// Cost accounting for server-side refusal fallbacks (upstream 4809c2abc,
// ed867e909).

// anthropicFallbackCosts runs one stream and reports the cost recorded at
// message_start (carried on the first text_start event's partial) alongside the
// final cost. Both are needed: the closing message_delta recomputes the figure,
// so a message_start-only regression is invisible in the final message.
func anthropicFallbackCosts(t *testing.T, model *ai.Model) (start, final ai.CostBreakdown) {
	t.Helper()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "k"
	stream := StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(req), opts)
	for ev := range stream.Events() {
		if ev.Type == ai.EventTextStart && ev.Partial != nil {
			start = ev.Partial.Usage.Cost
		}
	}
	msg := stream.Result()
	if msg == nil {
		t.Fatal("stream produced no message")
	}
	return start, msg.Usage.Cost
}

func assertCost(t *testing.T, label string, got, want ai.CostBreakdown) {
	t.Helper()
	const eps = 1e-12
	if math.Abs(got.Input-want.Input) > eps || math.Abs(got.Output-want.Output) > eps ||
		math.Abs(got.CacheRead-want.CacheRead) > eps || math.Abs(got.CacheWrite-want.CacheWrite) > eps ||
		math.Abs(got.Total-want.Total) > eps {
		t.Fatalf("%s cost = %+v, want %+v", label, got, want)
	}
}

// Every case below requests claude-opus-5, priced at 100/200 per Mtok, and is
// served a stream reporting 10 input tokens and 1 output token on message_start,
// then 2 output tokens on the closing message_delta.
func TestAnthropicFallbackCosting(t *testing.T) {
	// The catalog pricing for the fallback, and the two rate cards a stream can
	// end up billed at.
	const pricedCompat = `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}]}`
	requestedStart := ai.CostBreakdown{Input: 1e-3, Output: 2e-4, Total: 1.2e-3}
	requestedFinal := ai.CostBreakdown{Input: 1e-3, Output: 4e-4, Total: 1.4e-3}
	fallbackStart := ai.CostBreakdown{Input: 5e-5, Output: 2.5e-5, Total: 7.5e-5}
	fallbackFinal := ai.CostBreakdown{Input: 5e-5, Output: 5e-5, Total: 1e-4}

	tests := []struct {
		name         string
		sse          string
		compat       string
		start, final ai.CostBreakdown
	}{
		{
			// A response a fallback served is costed at the fallback's rates, taken
			// from the catalog, at both cost sites.
			name:  "the catalog prices the served model",
			sse:   anthropicSSEWithModel,
			start: fallbackStart, final: fallbackFinal,
			compat: pricedCompat,
		},
		{
			// pi guards the whole lookup on the served model differing from the
			// requested one, so a self-referential catalog entry cannot reprice
			// ordinary traffic.
			name:   "a served model equal to the requested one keeps its own pricing",
			sse:    anthropicSSESameModel,
			compat: `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-5","cost":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0}}]}`,
			start:  requestedStart, final: requestedFinal,
		},
		{
			// pi gates the swap on a pricing being FOUND, not on the served model
			// differing: an unlisted one leaves the requested model's rates in place
			// rather than blanking them.
			name:  "a served model the catalog does not list keeps the requested pricing",
			sse:   anthropicSSEWithModel,
			start: requestedStart, final: requestedFinal,
		},
		{
			// ed867e909 added `fallback.provider === model.provider` to the find, so
			// another provider's entry for the same model id must not price this
			// response.
			name:   "a catalog entry for another provider does not price the response",
			sse:    anthropicSSEWithModel,
			compat: `{"allowedFallbackModels":[{"provider":"vertex","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}]}`,
			start:  requestedStart, final: requestedFinal,
		},
		{
			// A fallback priced at all zeroes IS a found pricing though — every JS
			// object is truthy — so it swaps, to a zero-cost response.
			name:   "a fallback priced at zero still swaps",
			sse:    anthropicSSEWithModel,
			compat: `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}]}`,
			start:  ai.CostBreakdown{}, final: ai.CostBreakdown{},
		},
		{
			// pi's ternary assigns usageModel in both arms, so a message_start naming
			// the requested model again drops the fallback's rates.
			name:   "a second message_start reprices back to the requested model",
			sse:    anthropicSSERepricedBack,
			compat: pricedCompat,
			start:  requestedStart, final: requestedFinal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := startAnthropicFallbackStub(t, tc.sse)
			start, final := anthropicFallbackCosts(t, anthropicFallbackModel(stub.url, tc.compat))
			assertCost(t, "message_start", start, tc.start)
			assertCost(t, "final", final, tc.final)
		})
	}
}

// TestAnthropicUsageModelLookup pins the edges of pi's
// `find(f => f.provider === model.provider && f.model === served)?.cost` that a
// full stream cannot reach cheaply: both halves of the match, and the fact that
// `find` stops at the FIRST hit — an entry carrying no usable pricing yields
// `undefined` rather than scanning on to a later duplicate that has some.
func TestAnthropicUsageModelLookup(t *testing.T) {
	priced := `{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}`
	cases := []struct {
		name     string
		provider string
		compat   string
		wantCost *ai.ModelCost
	}{
		{
			name: "priced entry swaps", provider: "anthropic",
			compat:   `{"allowedFallbackModels":[` + priced + `]}`,
			wantCost: &ai.ModelCost{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
		},
		{
			name: "provider must match too", provider: "github-copilot",
			compat: `{"allowedFallbackModels":[` + priced + `]}`,
		},
		{
			// pi's `cost` is required by the type, but the catalog reaches Go as
			// untyped JSON and the runtime read is still `?.cost`: an entry without
			// it is `undefined`, which fails the truthiness gate.
			name: "an entry with no cost does not swap", provider: "anthropic",
			compat: `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8"}]}`,
		},
		{
			name: "an entry with a null cost does not swap", provider: "anthropic",
			compat: `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":null}]}`,
		},
		{
			// find returns the first hit; `?.cost` on it is undefined and the search
			// is over, so the priced duplicate behind it is never consulted.
			name: "the first match decides", provider: "anthropic",
			compat: `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8"},` + priced + `]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{
				ID: "claude-opus-5", Api: ai.APIAnthropicMessages, Provider: tc.provider,
				Cost: ai.ModelCost{Input: 100, Output: 200}, Compat: json.RawMessage(tc.compat),
			}
			got := anthropicUsageModel(model, "claude-opus-4-8")
			if tc.wantCost == nil {
				if got != model {
					t.Fatalf("want the requested model kept (same pointer), got %+v", *got)
				}
				return
			}
			if got == model {
				t.Fatal("want a repriced copy, got the caller's model back")
			}
			if got.ID != "claude-opus-4-8" || !reflect.DeepEqual(got.Cost, *tc.wantCost) {
				t.Fatalf("want id claude-opus-4-8 priced %+v, got id %q priced %+v", *tc.wantCost, got.ID, got.Cost)
			}
		})
	}
}

// TestAnthropicUsageModelLeavesCatalogModelAlone pins the two structural
// properties of pi's `fallbackCost ? { ...model, id, cost } : model`: the no-swap
// arm hands back the caller's own *ai.Model (pi's `: model`), and the swap arm is
// a copy — the shared catalog entry is never written to, whatever a stream
// reports.
func TestAnthropicUsageModelLeavesCatalogModelAlone(t *testing.T) {
	model := &ai.Model{
		ID: "claude-opus-5", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Cost:   ai.ModelCost{Input: 100, Output: 200},
		Compat: json.RawMessage(`{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}]}`),
	}
	before := *model

	if got := anthropicUsageModel(model, "claude-opus-5"); got != model {
		t.Fatal("a response served by the requested model must reuse the caller's *ai.Model")
	}
	if got := anthropicUsageModel(model, "claude-haiku-9"); got != model {
		t.Fatal("a served model the catalog does not price must reuse the caller's *ai.Model")
	}

	priced := anthropicUsageModel(model, "claude-opus-4-8")
	if priced == model {
		t.Fatal("a repriced response must not alias the shared catalog model")
	}
	priced.ID, priced.Cost = "mutated", ai.ModelCost{Input: -1}
	if model.ID != before.ID || !reflect.DeepEqual(model.Cost, before.Cost) {
		t.Fatalf("the shared catalog model was mutated: id %q cost %+v", model.ID, model.Cost)
	}
}

// TestAnthropicCatalogFallbacksAreLive pins the 0.84.3 activation: until that
// regen the embedded catalog carried zero allowedFallbackModels, so every rule
// above was exercised only against hand-built compat and the whole feature was
// dormant in practice. This test drives the REAL catalog compat onto the wire
// rather than re-asserting the JSON — a regen that drops the field, reshapes the
// entries, or breaks the decode fails here even though every other test in this
// file passes.
//
// The 0.85.1 regen dropped claude-opus-5's chain (it was the sole fallback to
// claude-opus-4-8); claude-fable-5 keeps both of its entries. The exact set is
// deliberately hard-coded: the point is to notice when a regen changes it.
func TestAnthropicCatalogFallbacksAreLive(t *testing.T) {
	ai.LoadBuiltinModels()
	models := ai.BuiltinModels(nil).GetModels("anthropic")

	// Exactly the models the 0.85.1 catalog gives fallbacks to, and the order of
	// each one's list, which is the order Anthropic receives.
	want := map[string][]string{
		"claude-fable-5": {"claude-opus-4-8", "claude-opus-5"},
	}

	got := map[string]*ai.Model{}
	for _, m := range models {
		if len(m.Compat) == 0 || !strings.Contains(string(m.Compat), "allowedFallbackModels") {
			continue
		}
		got[m.ID] = m
	}
	if len(got) != len(want) {
		ids := make([]string, 0, len(got))
		for id := range got {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Fatalf("catalog carries allowedFallbackModels for %v, want exactly %d models (%v)", ids, len(want), want)
	}

	for id, wantChain := range want {
		model := got[id]
		if model == nil {
			t.Fatalf("catalog model anthropic/%s carries no allowedFallbackModels", id)
		}

		// Drive the real catalog compat through a stub and read the wire.
		stub := startAnthropicFallbackStub(t, anthropicSSEWithModel)
		onWire := *model
		onWire.BaseURL = stub.url
		runAnthropicFallbackRequest(t, &onWire)

		fallbacks, ok := stub.body["fallbacks"].([]any)
		if !ok {
			t.Fatalf("%s: no fallbacks on the wire, body=%v", id, stub.body)
		}
		if len(fallbacks) != len(wantChain) {
			t.Fatalf("%s: got %d fallbacks, want %d: %v", id, len(fallbacks), len(wantChain), fallbacks)
		}
		for i, wantModel := range wantChain {
			entry, ok := fallbacks[i].(map[string]any)
			if !ok {
				t.Fatalf("%s: fallback %d is not an object: %v", id, i, fallbacks[i])
			}
			// Projected down to {model} only — no provider, no local pricing.
			if len(entry) != 1 || entry["model"] != wantModel {
				t.Fatalf("%s: fallback %d = %v, want exactly {\"model\":%q}", id, i, entry, wantModel)
			}
		}
	}
}
