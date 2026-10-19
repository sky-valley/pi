package providers

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// 6374c2baf (#10818): MiMo Chat Completions only supports a thinking on/off
// toggle, so the generator gives every Xiaomi reasoning model
// supportsReasoningEffort false and an off/high thinkingLevelMap; the 1.3.0
// regen carries it. Transliterated from xiaomi-models.test.ts "models %s
// reasoning as an off/on toggle", through ai.StreamSimple as the suite goes
// through compat.ts streamSimple, with pi's own expected values.
func TestXiaomiReasoningIsAnOnOffToggle(t *testing.T) {
	for _, provider := range []string{"xiaomi", "xiaomi-token-plan-cn", "xiaomi-token-plan-ams", "xiaomi-token-plan-sgp"} {
		t.Run(provider, func(t *testing.T) {
			var models []*ai.Model
			for _, model := range ai.GetModels(provider) {
				if model.Reasoning {
					models = append(models, model)
				}
			}
			if len(models) == 0 {
				t.Fatalf("%s has no reasoning model", provider)
			}
			for _, model := range models {
				if got := ai.GetSupportedThinkingLevels(model); !slices.Equal(got, []ai.ModelThinkingLevel{"off", "high"}) {
					t.Errorf("%s: GetSupportedThinkingLevels = %v, want [off high]", model.ID, got)
				}
				enabled := captureXiaomiPayload(t, model, "high")
				if got := enabled["thinking"]; !reflect.DeepEqual(got, map[string]any{"type": "enabled"}) {
					t.Errorf("%s high: thinking = %v, want {type: enabled}", model.ID, got)
				}
				if _, ok := enabled["reasoning_effort"]; ok {
					t.Errorf("%s high: reasoning_effort = %v, want it absent", model.ID, enabled["reasoning_effort"])
				}
				disabled := captureXiaomiPayload(t, model, "")
				if got := disabled["thinking"]; !reflect.DeepEqual(got, map[string]any{"type": "disabled"}) {
					t.Errorf("%s unset: thinking = %v, want {type: disabled}", model.ID, got)
				}
				if _, ok := disabled["reasoning_effort"]; ok {
					t.Errorf("%s unset: reasoning_effort = %v, want it absent", model.ID, disabled["reasoning_effort"])
				}
			}
		})
	}
}

func captureXiaomiPayload(t *testing.T, model *ai.Model, reasoning ai.ThinkingLevel) map[string]any {
	t.Helper()
	var captured any
	opts := &ai.SimpleStreamOptions{Reasoning: reasoning}
	opts.APIKey = "test-xiaomi-key"
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		captured = payload
		return nil, errors.New("payload captured")
	}
	final := ai.StreamSimple(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("test", 0)}}, opts).Result()
	body, _ := captured.(map[string]any)
	if body == nil {
		t.Fatalf("%s: no payload captured (stream ended %s: %q)", model.ID, final.StopReason, final.ErrorMessage)
	}
	return body
}
