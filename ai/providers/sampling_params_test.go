package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Mirrors pi's packages/ai/test/sampling-options.test.ts (upstream c01f687e5;
// the per-thinking-level table of 76dfb88f6 is TestSamplingParamsMatchPi's):
// arbitrary sampling parameters ride into the request body of the
// OpenAI-compatible adapters, the model's defaults with the request's merged
// over them per key and applied after the named request fields, while other
// APIs ignore them. The adapters do the merge, so a direct Stream gets the
// model's defaults as StreamSimple does (pi #9506).

// capturingHook is pi's onPayload-throws capture: it records the request body
// and fails the stream before any network call.
func capturingHook(captured *map[string]any) func(any, *ai.Model) (any, error) {
	return func(payload any, _ *ai.Model) (any, error) {
		params, ok := payload.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("payload is %T, want map[string]any", payload)
		}
		*captured = params
		return nil, fmt.Errorf("payload captured")
	}
}

// capturedStreamPayload runs pi's stream() — ai.Stream, dispatched on the
// model's API — far enough to build the request body and returns it.
func capturedStreamPayload(t *testing.T, model *ai.Model, opts ai.StreamOptions) map[string]any {
	t.Helper()
	var captured map[string]any
	opts.APIKey = "fake-key"
	opts.OnPayload = capturingHook(&captured)
	final := ai.Complete(t.Context(), model, baseReq(), &opts)
	if captured == nil {
		t.Fatalf("payload was never built (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	return captured
}

// capturedSimplePayload is capturedStreamPayload for a StreamSimple entry point.
func capturedSimplePayload(
	t *testing.T,
	stream ai.StreamSimpleFunction,
	model *ai.Model,
	opts ai.SimpleStreamOptions,
) map[string]any {
	t.Helper()
	var captured map[string]any
	opts.APIKey = "fake-key"
	opts.OnPayload = capturingHook(&captured)
	final := stream(t.Context(), model, ai.NormalizeContext(baseReq()), &opts).Result()
	if captured == nil {
		t.Fatalf("payload was never built (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	return captured
}

func samplingModel(overrides func(*ai.Model)) *ai.Model {
	return openAIModel(func(m *ai.Model) {
		m.ID = "custom-model"
		m.Provider = "custom-provider"
		m.BaseURL = "http://127.0.0.1:9/v1"
		if overrides != nil {
			overrides(m)
		}
	})
}

func TestSamplingParamsRequestLevel(t *testing.T) {
	body := capturedStreamPayload(t, samplingModel(nil), ai.StreamOptions{
		SamplingParams: map[string]any{"top_p": 0.95, "top_k": 0, "min_p": 0},
	})
	for key, want := range map[string]any{"top_p": 0.95, "top_k": 0, "min_p": 0} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
}

func TestSamplingParamsAbsentByDefault(t *testing.T) {
	body := capturedStreamPayload(t, samplingModel(nil), ai.StreamOptions{})
	if has(body, "temperature") || has(body, "top_p") {
		t.Fatalf("no sampling config must leave the body untouched, got temperature=%v top_p=%v",
			body["temperature"], body["top_p"])
	}
}

// Model defaults apply to a direct Stream, not only StreamSimple, with the
// request's keys taking precedence. pi runs the same case for
// azure-openai-responses, whose adapter is Scope queue row 4.
func TestSamplingParamsModelDefaultsUnderRequest(t *testing.T) {
	for _, api := range []ai.Api{ai.APIOpenAICompletions, ai.APIOpenAIResponses} {
		t.Run(string(api), func(t *testing.T) {
			model := samplingModel(func(m *ai.Model) {
				m.Api = api
				m.SamplingParams = map[string]any{"top_p": 0.95, "min_p": 0.05}
			})
			body := capturedStreamPayload(t, model, ai.StreamOptions{
				SamplingParams: map[string]any{"top_p": 0.5},
			})
			if body["top_p"] != 0.5 {
				t.Errorf("request key must win: top_p = %v, want 0.5", body["top_p"])
			}
			if body["min_p"] != 0.05 {
				t.Errorf("model default must apply: min_p = %v, want 0.05", body["min_p"])
			}
			// Object.assign copies onto the body; the model's own map is untouched.
			if model.SamplingParams["top_p"] != 0.95 {
				t.Errorf("model defaults mutated: top_p = %v, want 0.95", model.SamplingParams["top_p"])
			}
		})
	}
}

// Model defaults alone reach a direct Stream's body: pi #9506's own case, a
// complete() call that passes no samplingParams. pi's test always sends a
// request key, which would leave a model merge guarded on the request's map
// green.
func TestSamplingParamsModelLevel(t *testing.T) {
	for _, api := range []ai.Api{ai.APIOpenAICompletions, ai.APIOpenAIResponses} {
		t.Run(string(api), func(t *testing.T) {
			model := samplingModel(func(m *ai.Model) {
				m.Api = api
				m.SamplingParams = map[string]any{"temperature": 1, "top_p": 0.95}
			})
			body := capturedStreamPayload(t, model, ai.StreamOptions{})
			if body["temperature"] != 1 || body["top_p"] != 0.95 {
				t.Fatalf("model sampling params not applied: temperature=%v top_p=%v", body["temperature"], body["top_p"])
			}
		})
	}
}

// StreamSimple passes the request's keys through and the adapter merges them
// over the model's defaults. pi tests completions with a bare model; the
// responses entry and the clashing model default are the port's rows.
func TestSamplingParamsThroughStreamSimple(t *testing.T) {
	for _, tc := range []struct {
		api    ai.Api
		stream ai.StreamSimpleFunction
	}{
		{ai.APIOpenAICompletions, StreamSimpleOpenAICompletions},
		{ai.APIOpenAIResponses, StreamSimpleOpenAIResponses},
	} {
		t.Run(string(tc.api), func(t *testing.T) {
			model := samplingModel(func(m *ai.Model) {
				m.Api = tc.api
				m.SamplingParams = map[string]any{"top_p": 0.95, "min_p": 0.05}
			})
			body := capturedSimplePayload(t, tc.stream, model, ai.SimpleStreamOptions{
				StreamOptions: ai.StreamOptions{SamplingParams: map[string]any{"top_p": 0.5}},
			})
			if body["top_p"] != 0.5 {
				t.Errorf("request key must win: top_p = %v, want 0.5", body["top_p"])
			}
			if body["min_p"] != 0.05 {
				t.Errorf("model default must apply: min_p = %v, want 0.05", body["min_p"])
			}
		})
	}
}

// Sampling params are applied last, so a key named like a request field
// overrides it. pi tests completions; responses assigns last too.
func TestSamplingParamsOverrideNamedFields(t *testing.T) {
	for _, api := range []ai.Api{ai.APIOpenAICompletions, ai.APIOpenAIResponses} {
		t.Run(string(api), func(t *testing.T) {
			zero := 0.0
			model := samplingModel(func(m *ai.Model) { m.Api = api })
			body := capturedStreamPayload(t, model, ai.StreamOptions{
				Temperature:    &zero,
				SamplingParams: map[string]any{"temperature": 1},
			})
			if body["temperature"] != 1 {
				t.Fatalf("sampling params are applied last: temperature = %v, want 1", body["temperature"])
			}
		})
	}
}

func TestSamplingParamsIgnoredByAnthropic(t *testing.T) {
	model := samplingModel(func(m *ai.Model) { m.Api = ai.APIAnthropicMessages })
	body := capturedStreamPayload(t, model, ai.StreamOptions{
		SamplingParams: map[string]any{"top_p": 0.9, "top_k": 40},
	})
	if has(body, "top_p") || has(body, "top_k") {
		t.Fatalf("anthropic must ignore sampling params, got top_p=%v top_k=%v", body["top_p"], body["top_k"])
	}
}

// samplingParamsCaptureFile is what testdata/sampling-params/capture.mjs
// recorded from pi-ai 1.3.0: the openai-completions and openai-responses
// request bodies stream() and streamSimple() build from a model's sampling
// parameters, its per-thinking-level table and the request's.
const samplingParamsCaptureFile = "testdata/sampling-params/sampling-params-1.3.0.json"

// The sampling parameters a request carries are the model's, overridden by the
// model's samplingParamsByThinkingLevel entry for the effective level
// (ClampThinkingLevel of the reasoning effort, "off" when there is none, and
// "medium" for a summary-only Responses request), overridden by the request's
// — and they override the named request fields (upstream 76dfb88f6).
func TestSamplingParamsMatchPi(t *testing.T) {
	data, err := os.ReadFile(samplingParamsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name    string          `json:"name"`
			API     string          `json:"api"`
			Simple  bool            `json:"simple"`
			Model   json.RawMessage `json:"model"`
			Options struct {
				SamplingParams   map[string]any `json:"samplingParams"`
				Temperature      *float64       `json:"temperature"`
				Reasoning        string         `json:"reasoning"`
				ReasoningEffort  string         `json:"reasoningEffort"`
				ReasoningSummary string         `json:"reasoningSummary"`
			} `json:"options"`
			Payload string `json:"payload"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", samplingParamsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", samplingParamsCaptureFile)
	}
	errCaptured := errors.New("captured")
	for _, row := range capture.Rows {
		var model ai.Model
		if err := json.Unmarshal(row.Model, &model); err != nil {
			t.Fatalf("%s: model: %v", row.Name, err)
		}
		var payload any
		base := ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "fake-key", OnPayload: func(p any, _ *ai.Model) (any, error) {
				payload = p
				return nil, errCaptured
			}},
			Temperature:    row.Options.Temperature,
			SamplingParams: row.Options.SamplingParams,
		}
		req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 0)}})
		var final *ai.AssistantMessage
		switch {
		case row.Simple:
			opts := &ai.SimpleStreamOptions{StreamOptions: base, Reasoning: ai.ThinkingLevel(row.Options.Reasoning)}
			if row.API == "openai-completions" {
				final = StreamSimpleOpenAICompletions(context.Background(), &model, req, opts).Result()
			} else {
				final = StreamSimpleOpenAIResponses(context.Background(), &model, req, opts).Result()
			}
		case row.API == "openai-completions":
			final = StreamOpenAICompletions(context.Background(), &model, req, &OpenAIOptions{StreamOptions: base, ReasoningEffort: row.Options.ReasoningEffort}).Result()
		default:
			final = StreamOpenAIResponses(context.Background(), &model, req, &OpenAIResponsesOptions{StreamOptions: base, ReasoningEffort: row.Options.ReasoningEffort, ReasoningSummary: row.Options.ReasoningSummary}).Result()
		}
		if payload == nil {
			t.Errorf("%s: no payload (%s %q)", row.Name, final.StopReason, final.ErrorMessage)
			continue
		}
		got, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		_ = json.Unmarshal(got, &gotValue)
		if err := json.Unmarshal([]byte(row.Payload), &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("%s: body\n%s\npi\n%s", row.Name, got, row.Payload)
		}
	}
}
