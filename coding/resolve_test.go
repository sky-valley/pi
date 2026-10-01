package coding

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// I12: model resolution ports pi's resolveCliModel (model-resolver.ts).

// A slash prefix that is NOT a known provider is part of the model id:
// OpenRouter-style ids resolve across providers.
func TestResolveModelOpenRouterSlashedID(t *testing.T) {
	// npm 0.84.3 dropped the previous fixture (openrouter/ai21/jamba-large-1.7);
	// re-point on catalog churn. Pick an id that is unique CASE-INSENSITIVELY
	// across every provider — resolution folds case, so huggingface's
	// "meta-llama/Llama-3.1-8B-Instruct" would win an id that only looks unique
	// when compared byte-for-byte. This one is openrouter's alone and has survived
	// every build the port has pinned (0.83.0, 0.84.1, 0.84.2, 0.84.3);
	// "meta-llama" is not a pi provider id, which is what the case is about.
	r, err := ResolveModelPattern("meta-llama/llama-4-scout")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "openrouter" || r.Model.ID != "meta-llama/llama-4-scout" {
		t.Fatalf("expected openrouter/meta-llama/llama-4-scout, got %s/%s", r.Model.Provider, r.Model.ID)
	}
}

// A slash prefix that IS a known provider is preferred — but when nothing
// matches within that provider, the full input falls back to a raw model id
// across all models (pi: "openai/gpt-4o:extended" style openrouter ids).
func TestResolveModelProviderPrefixFallsBackToFullID(t *testing.T) {
	// "anthropic" is a known provider but has no "claude-opus-4.8-fast" model, so
	// the full input falls back to a raw model id across all providers, landing on
	// the sole copy (pi's registry .find() lands on the same one).
	//
	// The HOST of that copy is catalog churn, not behaviour: npm 0.80.7 dropped
	// the original fixture (vercel-ai-gateway/anthropic/claude-3.5-haiku), and
	// 0.85.1 dropped openrouter's copy of this one, moving it to
	// vercel-ai-gateway. So the expected provider is derived from the catalog and
	// the sole-copy precondition is asserted rather than assumed — that
	// precondition is what makes the fallback deterministic, and it is the thing
	// worth pinning.
	const id = "anthropic/claude-opus-4.8-fast"
	var hosts []string
	for _, provider := range ai.GetProviders() {
		if ai.GetModel(provider, id) != nil {
			hosts = append(hosts, provider)
		}
	}
	sort.Strings(hosts)
	if len(hosts) != 1 {
		t.Fatalf("fixture %q is hosted by %v, want exactly one provider; re-point it", id, hosts)
	}

	r, err := ResolveModelPattern(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != hosts[0] || r.Model.ID != id {
		t.Fatalf("expected %s/%s fallback for full id, got %s/%s", hosts[0], id, r.Model.Provider, r.Model.ID)
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

// pi 1fc80f4f (#5552): a requested thinking level on a custom-id fallback must
// set reasoning:true even when the provider's template model is non-reasoning,
// so the level is honored. mistral's fallback template is non-reasoning, so the
// flip is observable here.
func TestResolveModelCustomIDFallbackThinkingSuffixSetsReasoning(t *testing.T) {
	r, err := ResolveModelPattern("mistral/my-custom-model-id:high")
	if err != nil {
		t.Fatal(err)
	}
	if r.Model.ID != "my-custom-model-id" || r.ThinkingLevel != "high" {
		t.Fatalf("suffix parse wrong: id=%s level=%q", r.Model.ID, r.ThinkingLevel)
	}
	if !r.Model.Reasoning {
		t.Fatalf("requested thinking level must set reasoning:true on the fallback model")
	}
}

// The :off level is not a request to think: reasoning must stay false on a
// non-reasoning fallback template (pi gates on requestedThinking !== "off").
func TestResolveModelCustomIDFallbackThinkingOffKeepsReasoningFalse(t *testing.T) {
	r, err := ResolveModelPattern("mistral/my-custom-model-id:off")
	if err != nil {
		t.Fatal(err)
	}
	if r.Model.ID != "my-custom-model-id" || r.ThinkingLevel != "off" {
		t.Fatalf("suffix parse wrong: id=%s level=%q", r.Model.ID, r.ThinkingLevel)
	}
	if r.Model.Reasoning {
		t.Fatalf(":off must not enable reasoning on a non-reasoning fallback template")
	}
}

// All valid thinking levels work in the fallback path (upstream test parity).
// The list is pi's VALID_THINKING_LEVELS in full — seven levels, "max"
// included (cli/args.ts, and model-resolver.test.ts loops the same seven).
func TestResolveModelCustomIDFallbackAllLevels(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
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

// A ":max" suffix on a REAL catalog model resolves to that model at level
// "max", rather than being glued onto the id as a custom-model fallback. The
// all-levels test above only exercises the custom-id path, so it cannot catch a
// level missing from validThinkingLevels: an unrecognised suffix looks the same
// as a custom id there. This is the path upstream's model-resolver.test.ts
// covers and the one a user hits with `--model anthropic/claude-opus-4-8:max`.
func TestResolveModelRealModelMaxThinkingLevel(t *testing.T) {
	const id = "claude-opus-4-8"
	if ai.GetModel("anthropic", id) == nil {
		t.Skipf("catalog has no anthropic/%s; re-point this fixture", id)
	}
	r, err := ResolveModelPattern("anthropic/" + id + ":max")
	if err != nil {
		t.Fatal(err)
	}
	if r.Model.ID != id {
		t.Errorf("model id = %q, want %q (the :max suffix must be parsed off, not glued on)", r.Model.ID, id)
	}
	if r.ThinkingLevel != "max" {
		t.Errorf("thinking level = %q, want max", r.ThinkingLevel)
	}
	if r.Warning != "" {
		t.Errorf("unexpected warning %q — the model resolved, so nothing is custom", r.Warning)
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

// testdata/defaultmodels/capture.mjs reads pi's defaultModelPerProvider: from
// the published build when the pin is a release, from the source at the pin
// (its --src mode) between releases. The port's table must equal it: every
// provider, every id. Entries were missed twice before (baseten in c1019d920,
// the qwen-token-plan entries), because model-resolver.ts hunks sit in commits
// whose other hunks are host-only, and a missed re-point to an id the catalog
// still has passes TestDefaultModelsExistInCatalog. Re-capture at every
// re-pin, and point file at the capture for the new pin.
func TestDefaultModelPerProviderMatchesPi(t *testing.T) {
	const file = "testdata/defaultmodels/default-models-5806068c2.json"
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/defaultmodels/capture.mjs)", file, err)
	}
	var capture struct {
		Defaults map[string]string `json:"defaultModelPerProvider"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if len(capture.Defaults) == 0 {
		t.Fatalf("%s holds no defaults", file)
	}
	for provider, want := range capture.Defaults {
		if got, ok := defaultModelPerProvider[provider]; !ok || got != want {
			t.Errorf("default model for %q = %q, pi %q", provider, got, want)
		}
	}
	for provider, got := range defaultModelPerProvider {
		if _, ok := capture.Defaults[provider]; !ok {
			t.Errorf("default model for %q = %q, but pi has no entry for it", provider, got)
		}
	}
}

// Without their defaultModelPerProvider entries, a custom model id under the
// qwen-token-plan providers falls through to providerModels[0] — MiniMax-M2.5
// after the sort — and clones its contextWindow/maxTokens (196608/32768)
// instead of qwen3.7-max's (1000000/131072), which changes the emitted
// max_tokens and the context clamp. Lock the consequence, not just the entry.
func TestDefaultModelPerProviderQwenTokenPlanLimits(t *testing.T) {
	for _, provider := range []string{"qwen-token-plan", "qwen-token-plan-cn"} {
		t.Run(provider, func(t *testing.T) {
			tmpl := ai.GetModel(provider, defaultModelPerProvider[provider])
			if tmpl == nil {
				t.Fatalf("%s/%s missing from catalog", provider, defaultModelPerProvider[provider])
			}
			if tmpl.ContextWindow != 1000000 || tmpl.MaxTokens != 131072 {
				t.Fatalf("%s template limits = %d/%d, want 1000000/131072",
					provider, tmpl.ContextWindow, tmpl.MaxTokens)
			}
		})
	}
}

// Mirrors upstream model-resolver.test.ts "built-in chat providers have
// defaults in their generated catalogs" (e429d90b8, reshaped by a328aa89a):
// every catalog provider with chat models must have a default that resolves to
// one of them, so a regen can never orphan a default silently, and a provider
// with none (typesafe, which serves only classifiers) must have no chat
// default. Like upstream it iterates the catalog's providers, not the table's
// entries.
func TestDefaultModelsExistInCatalog(t *testing.T) {
	for _, provider := range ai.GetProviders() {
		defaultID, ok := defaultModelPerProvider[provider]
		if len(ai.GetModels(provider)) == 0 {
			if ok {
				t.Errorf("%s has no chat models and should have no chat default, has %q", provider, defaultID)
			}
			continue
		}
		if !ok {
			t.Errorf("catalog provider %q has no defaultModelPerProvider entry", provider)
			continue
		}
		if ai.GetModel(provider, defaultID) == nil {
			t.Errorf("%s default %s should exist in its generated catalog", provider, defaultID)
		}
	}
}

// pi a01baaae re-pointed defaultModelPerProvider's xai entry to grok-4.5 at
// 0.80.10; pi 70e878d4c advanced it to grok-4.6 and 1a584a7a5 to grok-4.7.
// Pin it through the custom-id fallback: the synthetic model must be templated
// from grok-4.7.
func TestResolveModelXaiFallbackDefault(t *testing.T) {
	r, err := ResolveModelPattern("xai/my-custom-grok")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Model.Provider) != "xai" || r.Model.ID != "my-custom-grok" {
		t.Fatalf("xai fallback wrong: %s/%s", r.Model.Provider, r.Model.ID)
	}
	// The template is the grok-4.7 catalog entry (clone carries its limits).
	tmpl, err := ResolveModel("xai/grok-4.7")
	if err != nil {
		t.Fatalf("grok-4.7 must exist in the catalog: %v", err)
	}
	if r.Model.ContextWindow != tmpl.ContextWindow || r.Model.MaxTokens != tmpl.MaxTokens {
		t.Fatalf("fallback not templated from grok-4.7: cw=%d/%d mt=%d/%d",
			r.Model.ContextWindow, tmpl.ContextWindow, r.Model.MaxTokens, tmpl.MaxTokens)
	}

	// At 0.87.1 the catalog's grok-4.6 and grok-4.7 differ only in id and name,
	// both of which the clone overwrites, so the check above cannot tell the two
	// templates apart. Give them distinct limits and the choice is observable.
	models := []*ai.Model{
		{ID: "grok-4.6", Provider: "xai", ContextWindow: 460_000, MaxTokens: 46_000},
		{ID: "grok-4.7", Provider: "xai", ContextWindow: 470_000, MaxTokens: 47_000},
	}
	got := buildFallbackModel("xai", "my-custom-grok", models)
	if got == nil || got.ID != "my-custom-grok" || got.ContextWindow != 470_000 || got.MaxTokens != 47_000 {
		t.Fatalf("fallback must be templated from grok-4.7 (cw=470000 mt=47000), got %+v", got)
	}
}
