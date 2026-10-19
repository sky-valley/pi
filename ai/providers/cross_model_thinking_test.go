package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// crossModelThinkingCaptureFile is what testdata/cross-model-thinking/
// capture.mjs recorded from pi-ai 1.3.0 (src at 911309429 records the same
// file; at 911309429^ every row with another model's thinking moves).
const crossModelThinkingCaptureFile = "testdata/cross-model-thinking/cross-model-thinking-1.3.0.json"

type crossModelThinkingCapture struct {
	TransformRows []struct {
		Name     string            `json:"name"`
		Model    ai.Model          `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Result   json.RawMessage   `json:"result"`
	} `json:"transformRows"`
	RequestRows []struct {
		Name     string            `json:"name"`
		Model    ai.Model          `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Payload  json.RawMessage   `json:"payload"`
	} `json:"requestRows"`
}

func loadCrossModelThinkingCapture(t *testing.T) crossModelThinkingCapture {
	t.Helper()
	data, err := os.ReadFile(crossModelThinkingCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture crossModelThinkingCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", crossModelThinkingCaptureFile, err)
	}
	if len(capture.TransformRows) == 0 || len(capture.RequestRows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", crossModelThinkingCaptureFile)
	}
	return capture
}

// Thinking from another model — another provider, api or model id — is
// dropped, whatever it holds: readable, signed, redacted or empty. Sending
// readable thinking as text made the new model treat it as part of its visible
// reply. A model's own thinking is kept as before (upstream 911309429).
func TestTransformDropsCrossModelThinkingLikePi(t *testing.T) {
	for _, row := range loadCrossModelThinkingCapture(t).TransformRows {
		t.Run(row.Name, func(t *testing.T) {
			model := row.Model
			result := transformMessages(decodeCapturedMessages(t, row.Name, row.Messages), &model, nil)
			for i, m := range result {
				if tr, ok := m.(ai.ToolResultMessage); ok && len(tr.Content) == 1 {
					if text, ok := tr.Content[0].(ai.TextContent); ok && text.Text == "No result provided" {
						tr.Timestamp = 0
						result[i] = tr
					}
				}
			}
			got, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !sameJSON(t, got, row.Result) {
				t.Errorf("transformMessages\n%s\npi\n%s", got, compactJSON(row.Result))
			}
		})
	}
}

// Every adapter that runs transformMessages replays a Claude turn to its own
// model without the turn's thinking.
func TestCrossModelThinkingRequestsMatchPi(t *testing.T) {
	for _, row := range loadCrossModelThinkingCapture(t).RequestRows {
		t.Run(row.Name, func(t *testing.T) {
			var payload []byte
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey, opts.CacheRetention = "test-key", ai.CacheNone
			opts.OnPayload = func(p any, _ *ai.Model) (any, error) {
				payload, _ = json.Marshal(p)
				return nil, errors.New("payload captured")
			}
			model := row.Model
			final := ai.StreamSimple(context.Background(), &model, ai.Context{Messages: decodeCapturedMessages(t, row.Name, row.Messages)}, opts).Result()
			if payload == nil {
				t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
			}
			want := row.Payload
			if model.Api == ai.APIGoogleGenerativeAI {
				// pi's onPayload sees @google/genai's call params ({model,
				// contents, config}), the port's the REST body it builds itself;
				// only contents is the same artifact on both sides
				// (difftest/README.md, Known limits).
				payload, want = jsonMember(t, payload, "contents"), jsonMember(t, want, "contents")
			}
			if !sameJSON(t, payload, want) {
				t.Errorf("params\n%s\npi\n%s", payload, compactJSON(want))
			}
		})
	}
}

// jsonMember returns one member of a JSON object.
func jsonMember(t *testing.T, raw []byte, key string) []byte {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return members[key]
}
