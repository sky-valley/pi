package coding

import (
	"strings"
	"testing"
)

// I12: model resolution ports pi's resolveCliModel (model-resolver.ts).

// A slash prefix that is NOT a known provider is part of the model id:
// OpenRouter-style ids resolve across providers.
func TestResolveModelOpenRouterSlashedID(t *testing.T) {
	r, err := ResolveModelPattern("ai21/jamba-large-1.7")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "openrouter" || r.Model.ID != "ai21/jamba-large-1.7" {
		t.Fatalf("expected openrouter/ai21/jamba-large-1.7, got %s/%s", r.Model.Provider, r.Model.ID)
	}
}

// A slash prefix that IS a known provider is preferred — but when nothing
// matches within that provider, the full input falls back to a raw model id
// across all models (pi: "openai/gpt-4o:extended" style openrouter ids).
func TestResolveModelProviderPrefixFallsBackToFullID(t *testing.T) {
	r, err := ResolveModelPattern("anthropic/claude-3.5-haiku")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "openrouter" || r.Model.ID != "anthropic/claude-3.5-haiku" {
		t.Fatalf("expected openrouter fallback for full id, got %s/%s", r.Model.Provider, r.Model.ID)
	}
}

func TestResolveModelCaseInsensitive(t *testing.T) {
	r, err := ResolveModelPattern("ANTHROPIC/CLAUDE-SONNET-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "anthropic" || r.Model.ID != "claude-sonnet-4-5" {
		t.Fatalf("case-insensitive resolution failed: %s/%s", r.Model.Provider, r.Model.ID)
	}
}

// A ":<level>" suffix parses off and surfaces alongside the model
// (parseModelPattern). Levels: off|minimal|low|medium|high|xhigh.
func TestResolveModelThinkingLevelSuffix(t *testing.T) {
	r, err := ResolveModelPattern("anthropic/claude-sonnet-4-5:high")
	if err != nil {
		t.Fatal(err)
	}
	if r.Model.ID != "claude-sonnet-4-5" || r.ThinkingLevel != "high" {
		t.Fatalf("suffix parse wrong: id=%s level=%q", r.Model.ID, r.ThinkingLevel)
	}
	// Bare-id pattern with suffix. ("claude-sonnet-4-5" is ambiguous across
	// providers in the catalog, so like pi the fuzzy matcher picks an alias —
	// only the model presence and parsed level are asserted here.)
	r, err = ResolveModelPattern("claude-sonnet-4-5:xhigh")
	if err != nil {
		t.Fatal(err)
	}
	if r.Model == nil || r.ThinkingLevel != "xhigh" {
		t.Fatalf("bare-id suffix parse wrong: model=%v level=%q", r.Model, r.ThinkingLevel)
	}
	// No suffix → empty level.
	r, err = ResolveModelPattern("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if r.ThinkingLevel != "" {
		t.Fatalf("unexpected level without suffix: %q", r.ThinkingLevel)
	}
}

// pi's exact error text (resolveCliModel).
func TestResolveModelUnknownErrorText(t *testing.T) {
	_, err := ResolveModelPattern("definitely-not-a-model-xyz")
	if err == nil {
		t.Fatal("expected error")
	}
	want := `Model "definitely-not-a-model-xyz" not found. Use --list-models to see available models.`
	if err.Error() != want {
		t.Fatalf("error text drift:\n got: %s\nwant: %s", err, want)
	}
}

// An unknown id under a KNOWN provider falls back to a synthetic custom-id
// model with a warning (pi buildFallbackModel).
func TestResolveModelCustomIDFallback(t *testing.T) {
	r, err := ResolveModelPattern("anthropic/my-custom-model-id")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "anthropic" || r.Model.ID != "my-custom-model-id" || r.Model.Name != "my-custom-model-id" {
		t.Fatalf("custom-id fallback wrong: %s/%s (%s)", r.Model.Provider, r.Model.ID, r.Model.Name)
	}
	if !strings.Contains(r.Warning, `Model "my-custom-model-id" not found for provider "anthropic". Using custom model id.`) {
		t.Fatalf("fallback warning drift: %q", r.Warning)
	}
	if r.ThinkingLevel != "" {
		t.Fatalf("fallback without suffix must not carry a level: %q", r.ThinkingLevel)
	}
}

// pi 9fd75b8a (#5560): a ":<level>" suffix on a custom id is stripped in the
// fallback path — it must NOT leak into the model id sent to the API — and is
// surfaced as the thinking level. The warning quotes the STRIPPED id.
func TestResolveModelCustomIDFallbackThinkingSuffix(t *testing.T) {
	r, err := ResolveModelPattern("anthropic/my-custom-model-id:high")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "anthropic" || r.Model.ID != "my-custom-model-id" {
		t.Fatalf("suffix leaked into custom id: %s/%s", r.Model.Provider, r.Model.ID)
	}
	if r.ThinkingLevel != "high" {
		t.Fatalf("fallback thinking level wrong: %q", r.ThinkingLevel)
	}
	if !strings.Contains(r.Warning, `Model "my-custom-model-id" not found for provider "anthropic". Using custom model id.`) {
		t.Fatalf("fallback warning must quote the stripped id: %q", r.Warning)
	}
}

// All valid thinking levels work in the fallback path (upstream test parity).
func TestResolveModelCustomIDFallbackAllLevels(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh"} {
		r, err := ResolveModelPattern("anthropic/my-custom-model-id:" + level)
		if err != nil {
			t.Fatal(err)
		}
		if r.Model.ID != "my-custom-model-id" {
			t.Fatalf("level %s: suffix leaked into custom id: %s", level, r.Model.ID)
		}
		if r.ThinkingLevel != level {
			t.Fatalf("level %s: fallback thinking level wrong: %q", level, r.ThinkingLevel)
		}
	}
}

// An invalid suffix is not a thinking level: it stays part of the custom id.
func TestResolveModelCustomIDFallbackInvalidSuffix(t *testing.T) {
	r, err := ResolveModelPattern("anthropic/my-custom-model-id:banana")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "anthropic" || r.Model.ID != "my-custom-model-id:banana" {
		t.Fatalf("invalid suffix must stay in the id: %s/%s", r.Model.Provider, r.Model.ID)
	}
	if r.ThinkingLevel != "" {
		t.Fatalf("invalid suffix must not surface a level: %q", r.ThinkingLevel)
	}
	if !strings.Contains(r.Warning, `Model "my-custom-model-id:banana" not found for provider "anthropic". Using custom model id.`) {
		t.Fatalf("fallback warning drift: %q", r.Warning)
	}
}
