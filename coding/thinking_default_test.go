package coding

import (
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai/providers"
)

// TestUnsetThinkingDefaultsToMediumOnReasoningModel verifies an unset thinking
// level starts at DEFAULT_THINKING_LEVEL ("medium") and is kept after clamping
// on a reasoning-capable model (pi defaults.ts:3 + sdk.ts:233-241).
func TestUnsetThinkingDefaultsToMediumOnReasoningModel(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "reasoner", Reasoning: true}},
	})
	defer reg.Unregister()
	model := reg.GetModel()

	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})
	if got := sess.Agent.State().ThinkingLevel; got != agent.ThinkMedium {
		t.Fatalf("unset thinking on a reasoning model = %q, want medium", got)
	}
}

// TestUnsetThinkingClampsToOffOnNonReasoningModel verifies the medium default
// clamps back to "off" on a model that does not support reasoning.
func TestUnsetThinkingClampsToOffOnNonReasoningModel(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "plain", Reasoning: false}},
	})
	defer reg.Unregister()
	model := reg.GetModel()

	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})
	if got := sess.Agent.State().ThinkingLevel; got != agent.ThinkOff {
		t.Fatalf("unset thinking on a non-reasoning model = %q, want off", got)
	}
}
