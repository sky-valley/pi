package providers

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// An Anthropic stream that reports a model other than the requested one keeps
// the REQUESTED id on the assistant message and records the reported one as
// responseModel, so a relay that relabels the model cannot turn signed thinking
// into a cross-model replay (upstream 1283afd0d, pi#9188). Fallback pricing is
// still keyed on the reported model.
//
// TestAnthropicResponseModelKeepsSignedThinkingReplayable and
// TestAnthropicResponseModelFallbackCost transliterate the two cases 1283afd0d
// added to packages/ai/test/anthropic-sse-parsing.test.ts; the remaining cases
// are this port's edges. Every expected message is pi's, captured under node by
// testdata/response-model/capture-anthropic-response-model.mts, last at
// f284a2460 (the response timing, 36a686ee8) with the 1.3.0 build's
// node_modules and catalog data. Re-captured at every regen: each time the
// behavioural entries came back identical and only the catalog model moved.

const anthropicResponseModelCaptureFile = "testdata/response-model/anthropic-response-model-f284a2460.json"

// anthropicResponseModelCapture decodes one captured entry into plain JSON
// values.
func anthropicResponseModelCapture(t *testing.T, key string) any {
	t.Helper()
	var entry any
	decodeAnthropicResponseModelCapture(t, key, &entry)
	return entry
}

// decodeAnthropicResponseModelCapture decodes one captured entry into dst.
func decodeAnthropicResponseModelCapture(t *testing.T, key string, dst any) {
	t.Helper()
	data, err := os.ReadFile(anthropicResponseModelCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]json.RawMessage
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", anthropicResponseModelCaptureFile, err)
	}
	entry, ok := capture[key]
	if !ok {
		t.Fatalf("%s has no %q entry; rerun capture-anthropic-response-model.mts", anthropicResponseModelCaptureFile, key)
	}
	if err := json.Unmarshal(entry, dst); err != nil {
		t.Fatalf("%s entry %q: %v", anthropicResponseModelCaptureFile, key, err)
	}
}

// The suite's two content blocks.
const (
	responseModelSignedThinking = `{"type":"thinking","thinking":"reasoning","signature":"signature"}`
	responseModelDoneText       = `{"type":"text","text":"done"}`
)

// anthropicResponseModelSSE is the suite's createResponseModelSseResponse body,
// with one message_start per entry of models ("" omits the model field, as
// JSON.stringify drops an undefined one).
func anthropicResponseModelSSE(models []string, contentBlock string) string {
	var b strings.Builder
	event := func(name, data string) {
		b.WriteString("event: " + name + "\ndata: " + data + "\n\n")
	}
	for _, model := range models {
		message := map[string]any{
			"id":    "msg_response_model",
			"usage": map[string]any{"input_tokens": 100, "output_tokens": 0},
		}
		if model != "" {
			message["model"] = model
		}
		data, _ := json.Marshal(map[string]any{"type": "message_start", "message": message})
		event("message_start", string(data))
	}
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":`+contentBlock+`}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":20}}`)
	event("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// startAnthropicResponseModel starts one "Hello" request against a stub serving
// sse — the suite's `stream(model, normalizeContext(...), {client})` — and
// returns the context it sent alongside the stream.
func startAnthropicResponseModel(t *testing.T, model *ai.Model, sse string) (ai.TranscriptContext, *ai.AssistantMessageEventStream) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	served := *model
	served.BaseURL = server.URL
	transcript := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}})
	stream := StreamAnthropic(context.Background(), &served, transcript,
		&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}})
	return transcript, stream
}

// streamAnthropicResponseModel is startAnthropicResponseModel's final message.
func streamAnthropicResponseModel(t *testing.T, model *ai.Model, sse string) (ai.TranscriptContext, *ai.AssistantMessage) {
	t.Helper()
	transcript, stream := startAnthropicResponseModel(t, model, sse)
	msg := stream.Result()
	if msg == nil {
		t.Fatal("stream produced no message")
	}
	return transcript, msg
}

// responseModelJSON is a message as plain JSON values, with a streamed
// assistant message's wall-clock timestamp dropped and its monotonic
// durationMs recorded only as being a number, as the capture does.
func responseModelJSON(t *testing.T, message ai.Message) any {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if am, ok := asAssistantMsg(message); ok {
		delete(out, "timestamp")
		if _, ok := out["durationMs"].(float64); ok {
			out["durationMs"] = "number"
		}
		if am.Usage.CacheWrite1h != 0 {
			t.Errorf("usage.cacheWrite1h = %d, want 0", am.Usage.CacheWrite1h)
		}
	}
	return out
}

// assertResponseModelJSON compares JSON values against pi's, reporting every
// difference rather than stopping at the first.
//
// One pre-existing shape difference is set aside, and only when the value is
// zero: pi's Anthropic adapter writes `usage.cacheWrite1h: 0` on every
// message_start, while ai.Usage drops a zero CacheWrite1h (omitempty, the
// convention Usage.Reasoning documents). The in-memory value is 0 on both sides,
// and responseModelJSON asserts the Go side's.
func assertResponseModelJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	want = withoutZeroCacheWrite1h(want)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("%s differs from pi:\ngot  %s\nwant %s", label, g, w)
	}
}

// withoutZeroCacheWrite1h copies a decoded JSON value, dropping every
// `usage.cacheWrite1h` that is exactly 0.
func withoutZeroCacheWrite1h(v any) any {
	switch v := v.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutZeroCacheWrite1h(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = withoutZeroCacheWrite1h(item)
		}
		if usage, ok := out["usage"].(map[string]any); ok && usage["cacheWrite1h"] == float64(0) {
			trimmed := make(map[string]any, len(usage))
			for k, item := range usage {
				if k != "cacheWrite1h" {
					trimmed[k] = item
				}
			}
			out["usage"] = trimmed
		}
		return out
	}
	return v
}

// opusForResponseModel is the suite's getModel("anthropic", "claude-opus-5"),
// checked against the catalog entry the capture streamed with.
func opusForResponseModel(t *testing.T) *ai.Model {
	t.Helper()
	model := ai.GetModel("anthropic", "claude-opus-5")
	if model == nil {
		t.Fatal("catalog has no anthropic/claude-opus-5")
	}
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	assertResponseModelJSON(t, "catalog claude-opus-5", got, anthropicResponseModelCapture(t, "model"))
	return model
}

// pricedOpusForResponseModel is the suite's fallback-cost model: claude-opus-5
// whose compat is REPLACED by one priced fallback.
func pricedOpusForResponseModel(t *testing.T) *ai.Model {
	t.Helper()
	priced := *opusForResponseModel(t)
	priced.Compat = json.RawMessage(`{"allowedFallbackModels":[{"provider":"anthropic","model":"fallback-model","cost":{"input":3,"output":5,"cacheRead":0,"cacheWrite":0}}]}`)
	return &priced
}

// 'keeps signed thinking replayable when a proxy relabels the model'.
func TestAnthropicResponseModelKeepsSignedThinkingReplayable(t *testing.T) {
	model := opusForResponseModel(t)
	const responseModel = "kimi-for-coding"
	initial, first := streamAnthropicResponseModel(t, model,
		anthropicResponseModelSSE([]string{responseModel}, responseModelSignedThinking))

	if first.Model != model.ID {
		t.Errorf("model = %q, want the requested %q", first.Model, model.ID)
	}
	if first.ResponseModel != responseModel {
		t.Errorf("responseModel = %q, want %q", first.ResponseModel, responseModel)
	}
	capture := anthropicResponseModelCapture(t, "relabeled").(map[string]any)
	assertResponseModelJSON(t, "assistant message", responseModelJSON(t, first), capture["message"])

	history := append(append([]ai.Message{}, initial.Messages...), first)
	replay := func(target *ai.Model) []any {
		var out []any
		for _, m := range transformMessages(history, target, nil) {
			out = append(out, responseModelJSON(t, m))
		}
		return out
	}

	transformed := transformMessages(history, model, nil)
	var replayed *ai.AssistantMessage
	for _, m := range transformed {
		if am, ok := asAssistantMsg(m); ok {
			replayed = am
			break
		}
	}
	want := ai.ContentList{ai.ThinkingContent{Thinking: "reasoning", ThinkingSignature: "signature"}}
	if replayed == nil {
		t.Fatal("the replay dropped the assistant message")
	}
	if !reflect.DeepEqual(replayed.Content, want) {
		t.Errorf("replayed assistant content = %#v, want %#v", replayed.Content, want)
	}
	assertResponseModelJSON(t, "replay", replay(model), capture["replayed"])

	// The gate keys on the requested id, not the relabel: replaying into a model
	// whose id IS the relabel is a cross-model replay.
	servedID := *model
	servedID.ID = responseModel
	assertResponseModelJSON(t, "replay into the served id", replay(&servedID), capture["replayedIntoServedId"])
}

// 'uses a returned fallback model for cost attribution'.
func TestAnthropicResponseModelFallbackCost(t *testing.T) {
	model := pricedOpusForResponseModel(t)
	const fallbackModel = "fallback-model"
	_, result := streamAnthropicResponseModel(t, model,
		anthropicResponseModelSSE([]string{fallbackModel}, responseModelDoneText))

	if result.Model != model.ID {
		t.Errorf("model = %q, want the requested %q", result.Model, model.ID)
	}
	if result.ResponseModel != fallbackModel {
		t.Errorf("responseModel = %q, want %q", result.ResponseModel, fallbackModel)
	}
	// toBeCloseTo(x, 10): |got - want| < 10^-10 / 2.
	if math.Abs(result.Usage.Cost.Input-0.0003) >= 5e-11 {
		t.Errorf("cost.input = %v, want 0.0003", result.Usage.Cost.Input)
	}
	if math.Abs(result.Usage.Cost.Output-0.0001) >= 5e-11 {
		t.Errorf("cost.output = %v, want 0.0001", result.Usage.Cost.Output)
	}
	assertResponseModelJSON(t, "assistant message", responseModelJSON(t, result), anthropicResponseModelCapture(t, "fallbackCost"))
}

// Edges of the only-when-different rule across message_start events, each
// against pi's message for the same stream.
func TestAnthropicResponseModelEdges(t *testing.T) {
	cases := []struct {
		name, capture string
		models        []string
	}{
		// Served by the requested model: no responseModel.
		{name: "same model", capture: "sameModel", models: []string{"claude-opus-5"}},
		// No model reported: the requested one stays, and no responseModel.
		{name: "missing model", capture: "missingModel", models: []string{""}},
		// A second message_start naming the requested model leaves the first
		// relabel standing but reprices back to the requested rates.
		{name: "relabeled then requested", capture: "relabeledThenRequested", models: []string{"fallback-model", "claude-opus-5"}},
		// A later relabel is recorded and priced.
		{name: "requested then fallback", capture: "requestedThenFallback", models: []string{"claude-opus-5", "fallback-model"}},
		// A later message_start with no model clears the earlier relabel.
		{name: "fallback then missing", capture: "fallbackThenMissing", models: []string{"fallback-model", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, msg := streamAnthropicResponseModel(t, pricedOpusForResponseModel(t),
				anthropicResponseModelSSE(tc.models, responseModelDoneText))
			assertResponseModelJSON(t, "assistant message", responseModelJSON(t, msg), anthropicResponseModelCapture(t, tc.capture))
		})
	}
}

// anthropicResponseModelEvent is what one pushed event says about the model: its
// type, and the model and responseModel of the message it carries (partial,
// message or error) as they stood when it was pushed.
type anthropicResponseModelEvent struct {
	Type          ai.EventType `json:"type"`
	Model         string       `json:"model"`
	ResponseModel string       `json:"responseModel,omitempty"`
}

// The relabel reaches every event pushed after message_start, and survives a
// stream that fails: each case replays the exact SSE body pi streamed and
// compares every event, then the final message, against pi's.
func TestAnthropicResponseModelStreamEvents(t *testing.T) {
	cases := []struct {
		name, capture string
		stopReason    ai.StopReason
	}{
		// Every event after message_start carries responseModel on its partial.
		{name: "relabeled", capture: "relabeledEvents", stopReason: ai.StopStop},
		// An errored stream keeps the requested model and the relabel on the error
		// message: a fallback block after content, thrown mid-stream...
		{name: "mid-output fallback error", capture: "midOutputFallbackError", stopReason: ai.StopError},
		// ...and a refusal stop reason, thrown once the stream has completed.
		{name: "refusal after a relabel", capture: "stopReasonErrorRelabel", stopReason: ai.StopError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var capture struct {
				SSE     string                        `json:"sse"`
				Events  []anthropicResponseModelEvent `json:"events"`
				Message any                           `json:"message"`
			}
			decodeAnthropicResponseModelCapture(t, tc.capture, &capture)
			model := opusForResponseModel(t)
			_, stream := startAnthropicResponseModel(t, model, capture.SSE)

			var events []anthropicResponseModelEvent
			for ev := range stream.Events() {
				carried := ev.Partial
				switch ev.Type {
				case ai.EventDone:
					carried = ev.Message
				case ai.EventError:
					carried = ev.Error
				}
				if carried == nil {
					t.Fatalf("%s event carries no message", ev.Type)
				}
				events = append(events, anthropicResponseModelEvent{Type: ev.Type, Model: carried.Model, ResponseModel: carried.ResponseModel})
			}
			if !reflect.DeepEqual(events, capture.Events) {
				t.Errorf("events differ from pi:\ngot  %+v\nwant %+v", events, capture.Events)
			}

			msg := stream.Result()
			if msg == nil {
				t.Fatal("stream produced no message")
			}
			if msg.StopReason != tc.stopReason || msg.Model != model.ID || msg.ResponseModel != "kimi-for-coding" {
				t.Errorf("final stopReason/model/responseModel = %q/%q/%q, want %q/%q/%q",
					msg.StopReason, msg.Model, msg.ResponseModel, tc.stopReason, model.ID, "kimi-for-coding")
			}
			assertResponseModelJSON(t, "final message", responseModelJSON(t, msg), capture.Message)
		})
	}
}
