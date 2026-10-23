package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicThinkingOffCaptureFile is what testdata/anthropic-thinking-off/
// capture.mjs recorded from src at e3bc05e87, with the pi-ai 1.4.0 build's
// catalog data. At e3bc05e87^ it records the 1.4.0 build's file exactly (which
// is 1.3.0's), and e3bc05e87 moves only the managed-effort output_config, from
// "high" to "low" in all 26 rows that send one.
const anthropicThinkingOffCaptureFile = "testdata/anthropic-thinking-off/thinking-off-e3bc05e87.json"

// On a managed-effort model, "off" — thinkingEnabled false on a reasoning model
// whose thinkingLevelMap does not mark off unsupported — sends {type:
// "disabled"} with no effort markers, no output_config and no block_binding,
// and the reply records no providerThinkingLevel, which is read from the params
// built (upstream dc939913f). The catalog's claude-opus-5 and claude-haiku-5-5
// accept it; Opus 5.5 and Fable 5.1 keep off unsupported.
func TestAnthropicThinkingOffMatchesPi(t *testing.T) {
	data, err := os.ReadFile(anthropicThinkingOffCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	type captured struct {
		Payload               json.RawMessage `json:"payload"`
		ProviderThinkingLevel *string         `json:"providerThinkingLevel"`
	}
	var capture struct {
		StreamRows []struct {
			Name     string            `json:"name"`
			Model    ai.Model          `json:"model"`
			Messages []json.RawMessage `json:"messages"`
			Options  struct {
				Effort          string `json:"effort"`
				ThinkingEnabled *bool  `json:"thinkingEnabled"`
			} `json:"options"`
			captured
		} `json:"streamRows"`
		CatalogRows []struct {
			Name      string  `json:"name"`
			Provider  string  `json:"provider"`
			ID        string  `json:"id"`
			Reasoning *string `json:"reasoning"`
			captured
		} `json:"catalogRows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", anthropicThinkingOffCaptureFile, err)
	}
	if len(capture.StreamRows) == 0 || len(capture.CatalogRows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", anthropicThinkingOffCaptureFile)
	}
	var payload []byte
	onPayload := func(p any, _ *ai.Model) (any, error) {
		payload, _ = json.Marshal(p)
		return nil, errors.New("payload captured")
	}
	check := func(t *testing.T, final *ai.AssistantMessage, want captured) {
		t.Helper()
		if payload == nil {
			t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
		}
		if !sameJSON(t, payload, want.Payload) {
			t.Errorf("params\n%s\npi\n%s", payload, compactJSON(want.Payload))
		}
		wantLevel := ""
		if want.ProviderThinkingLevel != nil {
			wantLevel = *want.ProviderThinkingLevel
		}
		if final.ProviderThinkingLevel != wantLevel {
			t.Errorf("providerThinkingLevel %q, pi %q", final.ProviderThinkingLevel, wantLevel)
		}
	}
	for _, row := range capture.StreamRows {
		t.Run("stream/"+row.Name, func(t *testing.T) {
			payload = nil
			// A null thinkingEnabled is pi's undefined.
			opts := &AnthropicOptions{Effort: row.Options.Effort}
			if row.Options.ThinkingEnabled != nil {
				opts.ThinkingProvided, opts.ThinkingEnabled = true, *row.Options.ThinkingEnabled
			}
			opts.APIKey, opts.CacheRetention, opts.OnPayload = "test-key", ai.CacheNone, onPayload
			model := row.Model
			final := StreamAnthropic(context.Background(), &model, ai.NormalizeContext(ai.Context{Messages: decodeCapturedMessages(t, row.Name, row.Messages)}), opts).Result()
			check(t, final, row.captured)
		})
	}
	for _, row := range capture.CatalogRows {
		t.Run("catalog/"+row.Name, func(t *testing.T) {
			payload = nil
			model := ai.GetModel(row.Provider, row.ID)
			if model == nil {
				t.Fatalf("no %s/%s in the catalog", row.Provider, row.ID)
			}
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey, opts.CacheRetention, opts.OnPayload = "test-key", ai.CacheNone, onPayload
			if row.Reasoning != nil {
				opts.Reasoning = ai.ThinkingLevel(*row.Reasoning)
			}
			final := StreamSimpleAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("one", 1)}}), opts).Result()
			check(t, final, row.captured)
		})
	}
}
