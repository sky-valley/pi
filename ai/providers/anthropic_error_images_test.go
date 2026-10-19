package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicErrorImagesCaptureFile is what testdata/anthropic-error-images/
// capture.mjs recorded from pi-ai 1.3.0's anthropic-messages streamSimple (src
// at 5ab7fe74c records the same file; at 5ab7fe74c^ and at 34a8bb38b^ every row
// with an error result carrying images moves).
const anthropicErrorImagesCaptureFile = "testdata/anthropic-error-images/error-images-1.3.0.json"

// Anthropic rejects an error tool_result holding anything but text, so pi
// replaces each run of images in one with "(image attached after the tool
// results)" and sends the images after ALL tool results of the turn, in the
// same user message, under "Images from the error result of tool call <id>
// (<name>):" (upstream 34a8bb38b, 5ab7fe74c).
func TestAnthropicErrorToolResultImagesMatchPi(t *testing.T) {
	data, err := os.ReadFile(anthropicErrorImagesCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name           string            `json:"name"`
			Model          ai.Model          `json:"model"`
			Messages       []json.RawMessage `json:"messages"`
			CacheRetention string            `json:"cacheRetention"`
			Payload        json.RawMessage   `json:"payload"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", anthropicErrorImagesCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", anthropicErrorImagesCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			messages := decodeCapturedMessages(t, row.Name, row.Messages)
			var payload []byte
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey = "fake-key"
			opts.CacheRetention = ai.CacheRetention(row.CacheRetention)
			opts.OnPayload = func(p any, _ *ai.Model) (any, error) {
				payload, _ = json.Marshal(p)
				return nil, errors.New("payload captured")
			}
			model := row.Model
			final := StreamSimpleAnthropic(context.Background(), &model, ai.NormalizeContext(ai.Context{Messages: messages}), opts).Result()
			if payload == nil {
				t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
			}
			if !sameJSON(t, payload, row.Payload) {
				t.Errorf("params\n%s\npi\n%s", payload, compactJSON(row.Payload))
			}
		})
	}
}
