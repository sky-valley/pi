package coding

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// DefaultModelSpec is the model used when none is specified. Deliberate
// divergence from pi: pi's default model comes from settings.json /
// findInitialModel (first available provider default); the Go port has no
// settings manager, so an empty spec resolves to this fixed default.
const DefaultModelSpec = "anthropic/claude-sonnet-4-5"

// defaultModelPerProvider ports pi's defaultModelPerProvider
// (model-resolver.ts): the per-provider default model id used by
// buildFallbackModel when synthesizing a custom-id model.
var defaultModelPerProvider = map[string]string{
	"amazon-bedrock":             "us.anthropic.claude-opus-4-6-v1",
	"ant-ling":                   "Ring-2.6-1T",
	"anthropic":                  "claude-opus-4-8",
	"openai":                     "gpt-5.5",
	"azure":                      "gpt-5.4",
	"openai-codex":               "gpt-6.1-sol",
	"radius":                     "balanced",
	"nvidia":                     "nvidia/nemotron-3.5-super-vl-120b-a12b",
	"deepseek":                   "deepseek-v4-pro",
	"google":                     "gemini-3.1-pro-preview",
	"google-vertex":              "gemini-3.1-pro-preview",
	"github-copilot":             "gpt-5.4",
	"openrouter":                 "moonshotai/kimi-k2.6",
	"vercel-ai-gateway":          "zai/glm-5.1",
	"xai":                        "grok-4.7",
	"groq":                       "openai/gpt-oss-120b",
	"cerebras":                   "gpt-oss-120b",
	"zai":                        "glm-5.3",
	"zai-coding-cn":              "glm-5.3",
	"mistral":                    "devstral-medium-latest",
	"minimax":                    "MiniMax-M2.7",
	"minimax-cn":                 "MiniMax-M2.7",
	"moonshotai":                 "kimi-k2.6",
	"moonshotai-cn":              "kimi-k2.6",
	"huggingface":                "moonshotai/Kimi-K2.6",
	"fireworks":                  "accounts/fireworks/models/kimi-k3",
	"together":                   "moonshotai/Kimi-K3",
	"baseten":                    "zai-org/GLM-5.2",
	"opencode":                   "kimi-k2.6",
	"opencode-go":                "kimi-k3",
	"kimi-coding":                "kimi-for-coding",
	"meta":                       "muse-spark-1.3",
	"cloudflare-workers-ai":      "@cf/moonshotai/kimi-k2.6",
	"cloudflare-ai-gateway":      "workers-ai/@cf/moonshotai/kimi-k2.6",
	"qwen-token-plan":            "qwen3.7-max",
	"qwen-token-plan-cn":         "qwen3.7-max",
	"qwen-token-plan-individual": "qwen3.8-max",
	"xiaomi":                     "mimo-v2.6-pro",
	"xiaomi-token-plan-cn":       "mimo-v2.6-pro",
	"xiaomi-token-plan-ams":      "mimo-v2.6-pro",
	"xiaomi-token-plan-sgp":      "mimo-v2.6-pro",
}

// validThinkingLevels mirrors pi's VALID_THINKING_LEVELS (cli/args.ts). "max"
// arrived upstream with fbdd46389; the port took that commit's ai-side half
// (ai.ThinkingMax) and missed this list, so every ":max" spec fell through to
// the custom-id fallback with the suffix still glued to the model id.
var validThinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// ResolvedModel is the result of resolving a model spec.
type ResolvedModel struct {
	Model *ai.Model
	// ThinkingLevel is the level parsed from a ":<level>" suffix in the spec,
	// or "" when the spec carried none.
	ThinkingLevel string
	// Warning carries pi's non-fatal resolution warnings (e.g. the custom-id
	// fallback for an unknown model under a known provider).
	Warning string
}

// ResolveModel resolves a model spec to a Model from the catalog (an empty
// spec resolves to DefaultModelSpec). Kept for source compatibility; the
// parsed thinking level and warnings are available via ResolveModelPattern.
func ResolveModel(spec string) (*ai.Model, error) {
	r, err := ResolveModelPattern(spec)
	return r.Model, err
}

// ResolveModelPattern ports pi's resolveCliModel (model-resolver.ts) for a
// --model spec with no --provider, --thinking or --api-key: the spec is a bare
// model id or provider/id, optionally followed by ":<thinking>", resolved
// against every catalog model through one provider selection policy
// (upstream d7f7e0dfe; see resolveModelReference). Credentials configured in
// the environment only break ties between providers offering the same model,
// so a unique match resolves without any. An unknown model under a known
// provider falls back to a custom-id model with a warning; an ambiguous one
// fails, naming the candidates.
func ResolveModelPattern(spec string) (ResolvedModel, error) {
	if spec == "" {
		spec = DefaultModelSpec
	}
	return resolveCliModel(cliModelOptions{model: spec, models: allCatalogModels(), hasConfiguredAuth: configuredAuthCheck()})
}

// configuredAuthCheck is pi's modelRuntime.hasConfiguredAuth for the built-in
// runtime: whether a provider's auth resolves from what is configured. Each
// provider is checked at most once, and only when a tie needs it.
func configuredAuthCheck() func(provider string) bool {
	var models ai.Models
	checked := map[string]bool{}
	return func(provider string) bool {
		if usable, ok := checked[provider]; ok {
			return usable
		}
		if models == nil {
			models = ai.BuiltinModels()
		}
		check, err := models.CheckAuth(context.Background(), provider)
		checked[provider] = err == nil && check != nil
		return checked[provider]
	}
}

// cliModelOptions are resolveCliModel's options: the --provider, --model and
// --thinking flags, whether --api-key was given, the models to resolve
// against (every model, not only those with credentials, so --api-key can set
// a provider up) and the runtime's hasConfiguredAuth.
type cliModelOptions struct {
	provider          string
	model             string
	thinking          string
	inlineAPIKey      bool
	models            []*ai.Model
	hasConfiguredAuth func(provider string) bool
}

// resolveCliModel is pi's resolveCliModel. Its errors are pi's CLI messages,
// each of which says what to do next.
func resolveCliModel(opts cliModelOptions) (ResolvedModel, error) {
	if opts.model == "" {
		return ResolvedModel{}, nil
	}
	if len(opts.models) == 0 {
		return ResolvedModel{}, errors.New("No models available. Check your installation or add models to models.json.")
	}
	provider := ""
	if opts.provider != "" {
		lower := jstext.ToLower(opts.provider)
		for _, m := range opts.models {
			if jstext.ToLower(string(m.Provider)) == lower {
				provider = string(m.Provider)
				break
			}
		}
		if provider == "" {
			return ResolvedModel{}, fmt.Errorf("Unknown provider \"%s\". Use --list-models to see available providers/models.", opts.provider)
		}
	}

	// An inline key goes to whichever provider is selected, so stored
	// credentials must not choose it.
	usable := opts.hasConfiguredAuth
	if opts.inlineAPIKey || usable == nil {
		usable = func(string) bool { return false }
	}
	result := resolveModelReference(modelReferenceRequest{text: opts.model, provider: provider, thinkingSuffix: thinkingSuffixStrict, fuzzy: true}, opts.models, usable)
	switch result.status {
	case referenceResolved:
		return ResolvedModel{Model: result.model, ThinkingLevel: result.thinkingLevel, Warning: result.warning}, nil
	case referenceAmbiguous:
		matches := make([]string, len(result.candidates))
		for i, m := range result.candidates {
			matches[i] = string(m.Provider) + "/" + m.ID
		}
		collator := collate.New(language.Und)
		sort.SliceStable(matches, func(i, j int) bool { return collator.CompareString(matches[i], matches[j]) < 0 })
		authHint := "More than one matching provider is authenticated."
		switch {
		case opts.inlineAPIKey:
			authHint = "--api-key does not choose a provider."
		case result.usableCount == 0:
			authHint = "No matching provider is authenticated."
		}
		return ResolvedModel{}, fmt.Errorf("Model \"%s\" is ambiguous across providers: %s. %s Use --provider or provider/model.", opts.model, strings.Join(matches, ", "), authHint)
	}
	target := result.target
	if target == nil {
		return ResolvedModel{}, fmt.Errorf("Model \"%s\" not found. Use --list-models to see available models.", opts.model)
	}
	// The thinking suffix applies only when --thinking is not given; otherwise
	// it stays part of the custom model id.
	// e.g. "zai-org/GLM-5.1-FP8:high" → id "zai-org/GLM-5.1-FP8", level "high".
	modelID, fallbackThinking := target.modelID, target.thinkingLevel
	if opts.thinking != "" {
		modelID, fallbackThinking = target.modelText, ""
	}
	fallback := buildFallbackModel(target.providerModels, modelID)
	requestedThinking := opts.thinking
	if requestedThinking == "" {
		requestedThinking = fallbackThinking
	}
	if requestedThinking != "" && requestedThinking != "off" {
		fallback.Reasoning = true
	}
	return ResolvedModel{
		Model:         fallback,
		ThinkingLevel: fallbackThinking,
		Warning:       fmt.Sprintf("Model \"%s\" not found for provider \"%s\". Using custom model id.", modelID, target.provider),
	}, nil
}

// allCatalogModels lists every registered model in deterministic order
// (providers and ids sorted; pi's registry order is models.json order, which
// resolveModelReference never lets decide).
func allCatalogModels() []*ai.Model {
	providers := ai.GetProviders()
	sort.Strings(providers)
	var out []*ai.Model
	for _, p := range providers {
		models := ai.GetModels(p)
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		out = append(out, models...)
	}
	return out
}

// Model reference resolution (pi model-resolver.ts, upstream d7f7e0dfe).
// Candidates are collected before a provider is chosen, so catalog order never
// decides the provider and an ambiguous match is reported instead of falling
// through to a weaker one.

// thinkingSuffixMode is how a reference treats a trailing ":<suffix>" (pi
// ModelReferenceRequest.thinkingSuffix). It is only considered when the full
// text matches no model, since ids such as OpenRouter's
// "qwen/qwen3-coder:exacto" contain colons.
type thinkingSuffixMode int

const (
	// thinkingSuffixNone keeps the suffix part of the id.
	thinkingSuffixNone thinkingSuffixMode = iota
	// thinkingSuffixStrict parses a valid thinking level; any other suffix
	// stays part of the id.
	thinkingSuffixStrict
	// thinkingSuffixLenient is strict, but drops an invalid suffix with a
	// warning.
	thinkingSuffixLenient
)

// modelReferenceRequest is pi's ModelReferenceRequest.
type modelReferenceRequest struct {
	// text is a bare model id or provider/id, optionally followed by
	// ":<thinking>".
	text string
	// provider, when set, limits the candidates to that provider.
	provider       string
	thinkingSuffix thinkingSuffixMode
	// fuzzy matches id and name substrings when no exact match exists.
	fuzzy bool
}

type modelReferenceStatus int

const (
	referenceNotFound modelReferenceStatus = iota
	referenceResolved
	referenceAmbiguous
)

func (s modelReferenceStatus) String() string {
	switch s {
	case referenceResolved:
		return "resolved"
	case referenceAmbiguous:
		return "ambiguous"
	}
	return "not-found"
}

// modelReferenceResult is pi's ModelReferenceResult: a resolved model, the
// candidates of an ambiguous reference with how many are usable, or nothing,
// with a target when a provider is explicit or named by a provider/ prefix.
// thinkingLevel and warning go with a resolved or ambiguous result.
type modelReferenceResult struct {
	status        modelReferenceStatus
	model         *ai.Model
	candidates    []*ai.Model
	usableCount   int
	thinkingLevel string
	warning       string
	target        *modelReferenceTarget
}

// modelReferenceTarget is pi's ModelReferenceTarget, for building a custom
// model id: the provider and its models (never empty, the anchor first), the
// text after the provider prefix as typed, and that text without a valid
// thinking suffix.
type modelReferenceTarget struct {
	provider       string
	providerModels []*ai.Model
	modelText      string
	modelID        string
	thinkingLevel  string
}

// splitThinkingSuffix splits a valid ":<thinking>" suffix off text.
func splitThinkingSuffix(text string) (base, thinkingLevel string, ok bool) {
	colon := strings.LastIndex(text, ":")
	if colon == -1 || !validThinkingLevels[text[colon+1:]] {
		return "", "", false
	}
	return text[:colon], text[colon+1:], true
}

// splitProviderPrefix splits a provider/ prefix naming one of models'
// providers off text, returning that provider's own spelling.
func splitProviderPrefix(text string, models []*ai.Model) (provider, modelText string, ok bool) {
	slash := strings.Index(text, "/")
	if slash == -1 {
		return "", "", false
	}
	prefix := jstext.ToLower(jstext.Trim(text[:slash]))
	modelText = jstext.Trim(text[slash+1:])
	if prefix == "" || modelText == "" {
		return "", "", false
	}
	for _, m := range models {
		if jstext.ToLower(string(m.Provider)) == prefix {
			return string(m.Provider), modelText, true
		}
	}
	return "", "", false
}

// resolveModelReference is pi's resolveModelReference. usable only breaks
// ties between providers, so a unique match resolves even without
// credentials (--api-key first-time setup).
func resolveModelReference(request modelReferenceRequest, models []*ai.Model, usable func(provider string) bool) modelReferenceResult {
	text := jstext.Trim(request.text)
	if text == "" {
		return modelReferenceResult{}
	}
	constraint := jstext.ToLower(request.provider)
	pool := models
	if constraint != "" {
		pool = filterModels(models, func(m *ai.Model) bool { return jstext.ToLower(string(m.Provider)) == constraint })
	}
	result := resolveText(text, request, pool, usable)
	if result.status != referenceNotFound {
		return result
	}

	splitProvider, splitText, split := splitProviderPrefix(text, pool)
	var anchor *ai.Model
	switch {
	case constraint != "":
		if len(pool) > 0 {
			anchor = pool[0]
		}
	case split:
		anchor = findModel(pool, func(m *ai.Model) bool { return string(m.Provider) == splitProvider })
	}
	if anchor == nil {
		return result
	}
	modelText := text
	if split {
		modelText = splitText
	}
	target := &modelReferenceTarget{provider: string(anchor.Provider), providerModels: []*ai.Model{anchor}, modelText: modelText, modelID: modelText}
	for _, m := range pool {
		if m.Provider == anchor.Provider && m != anchor {
			target.providerModels = append(target.providerModels, m)
		}
	}
	if request.thinkingSuffix != thinkingSuffixNone {
		if base, level, ok := splitThinkingSuffix(modelText); ok {
			target.modelID, target.thinkingLevel = base, level
		}
	}
	return modelReferenceResult{target: target}
}

// findExactModelReferenceMatch is pi's findExactModelReferenceMatch: an
// exact provider/id or bare id lookup, without thinking suffixes or fuzzy
// matching, that resolves only when unambiguous.
func findExactModelReferenceMatch(reference string, models []*ai.Model) *ai.Model {
	result := resolveModelReference(modelReferenceRequest{text: reference, thinkingSuffix: thinkingSuffixNone}, models, func(string) bool { return true })
	if result.status != referenceResolved {
		return nil
	}
	return result.model
}

// resolveText is pi's resolveText: an exact reference, then a fuzzy match,
// then the same with a thinking suffix split off.
func resolveText(text string, request modelReferenceRequest, pool []*ai.Model, usable func(string) bool) modelReferenceResult {
	if reference, ok := selectReference(text, pool, request.fuzzy, usable); ok {
		return reference
	}
	if request.fuzzy {
		if fuzzy, ok := selectFuzzy(fuzzyMatches(text, pool), usable); ok {
			return fuzzy
		}
	}
	if request.thinkingSuffix == thinkingSuffixNone {
		return modelReferenceResult{}
	}
	if base, level, ok := splitThinkingSuffix(text); ok {
		inner := resolveText(base, request, pool, usable)
		if inner.status == referenceNotFound {
			return inner
		}
		// The level applies only when the inner text resolved cleanly.
		inner.thinkingLevel = level
		if inner.warning != "" {
			inner.thinkingLevel = ""
		}
		return inner
	}
	// Keep an invalid suffix as part of the id and fail, rather than resolve a
	// different model.
	if request.thinkingSuffix == thinkingSuffixStrict {
		return modelReferenceResult{}
	}
	colon := strings.LastIndex(text, ":")
	if colon == -1 {
		return modelReferenceResult{}
	}
	inner := resolveText(text[:colon], request, pool, usable)
	if inner.status == referenceNotFound {
		return inner
	}
	inner.thinkingLevel = ""
	inner.warning = `Invalid thinking level "` + text[colon+1:] + `" in pattern "` + text + `". Using default instead.`
	return inner
}

// selectReference is pi's selectReference. provider/rest is only a hint: the
// whole text may also be a model id on another provider, such as gateway id
// "zai/glm-5". The named provider wins when usable, since that is what the
// syntax usually means; otherwise a sole usable raw match wins, since the
// named provider could not serve the request. An unusable named match is
// still preferred over unusable raw matches, so --api-key can set up the
// named provider.
func selectReference(text string, pool []*ai.Model, fuzzy bool, usable func(string) bool) (modelReferenceResult, bool) {
	lowerText := jstext.ToLower(text)
	raw := filterModels(pool, func(m *ai.Model) bool { return jstext.ToLower(m.ID) == lowerText })
	var named *ai.Model
	if provider, modelText, ok := splitProviderPrefix(text, pool); ok {
		providerModels := filterModels(pool, func(m *ai.Model) bool { return string(m.Provider) == provider })
		lowerModelText := jstext.ToLower(modelText)
		// The provider's own literal id (OpenRouter's "openrouter/free") beats
		// its fuzzy matches.
		named = findModel(providerModels, func(m *ai.Model) bool { return jstext.ToLower(m.ID) == lowerModelText })
		if named == nil {
			named = findModel(raw, func(m *ai.Model) bool { return string(m.Provider) == provider })
		}
		if named == nil && fuzzy {
			named = bestFuzzyMatch(fuzzyMatches(modelText, providerModels))
		}
	}

	others := filterModels(raw, func(m *ai.Model) bool { return m != named })
	candidates := others
	if named != nil {
		candidates = append([]*ai.Model{named}, others...)
	}
	switch {
	case len(candidates) == 0:
		return modelReferenceResult{}, false
	case providerCount(candidates) == 1:
		return modelReferenceResult{status: referenceResolved, model: candidates[0]}, true
	case named != nil && usable(string(named.Provider)):
		return modelReferenceResult{status: referenceResolved, model: named}, true
	}
	usableOthers := filterModels(others, func(m *ai.Model) bool { return usable(string(m.Provider)) })
	switch {
	case len(usableOthers) == 1:
		return modelReferenceResult{status: referenceResolved, model: usableOthers[0]}, true
	case len(usableOthers) == 0 && named != nil:
		return modelReferenceResult{status: referenceResolved, model: named}, true
	}
	return modelReferenceResult{status: referenceAmbiguous, candidates: candidates, usableCount: len(usableOthers)}, true
}

// fuzzyMatches are the models whose id or name contains text, ignoring case.
func fuzzyMatches(text string, models []*ai.Model) []*ai.Model {
	lowerText := jstext.ToLower(text)
	return filterModels(models, func(m *ai.Model) bool {
		return strings.Contains(jstext.ToLower(m.ID), lowerText) || strings.Contains(jstext.ToLower(m.Name), lowerText)
	})
}

// selectFuzzy is pi's selectFuzzy: matches on several providers narrow to the
// usable ones when there are any, then the best match wins unless the same id
// is offered by several providers, which would otherwise be decided by catalog
// order.
func selectFuzzy(matches []*ai.Model, usable func(string) bool) (modelReferenceResult, bool) {
	if len(matches) == 0 {
		return modelReferenceResult{}, false
	}
	var usableMatches []*ai.Model
	if providerCount(matches) > 1 {
		usableMatches = filterModels(matches, func(m *ai.Model) bool { return usable(string(m.Provider)) })
	}
	pool := matches
	if len(usableMatches) > 0 {
		pool = usableMatches
	}
	best := bestFuzzyMatch(pool)
	if best == nil {
		return modelReferenceResult{}, false
	}
	tied := filterModels(pool, func(m *ai.Model) bool { return m.ID == best.ID })
	if providerCount(tied) == 1 {
		return modelReferenceResult{status: referenceResolved, model: best}, true
	}
	usableCount := 0
	if len(usableMatches) > 0 {
		usableCount = len(tied)
	}
	return modelReferenceResult{status: referenceAmbiguous, candidates: tied, usableCount: usableCount}, true
}

// bestFuzzyMatch prefers aliases (claude-sonnet-4-5) over dated versions
// (claude-sonnet-4-5-20250929), then the latest: pi sorts by
// b.id.localeCompare(a.id), which a root-locale collator approximates.
func bestFuzzyMatch(matches []*ai.Model) *ai.Model {
	pool := filterModels(matches, func(m *ai.Model) bool { return isModelAlias(m.ID) })
	if len(pool) == 0 {
		pool = slices.Clone(matches)
	}
	if len(pool) == 0 {
		return nil
	}
	collator := collate.New(language.Und)
	sort.SliceStable(pool, func(i, j int) bool { return collator.CompareString(pool[j].ID, pool[i].ID) < 0 })
	return pool[0]
}

var modelDatePattern = regexp.MustCompile(`-\d{8}$`)

// isModelAlias ports pi's isAlias: -latest ids and ids without a -YYYYMMDD
// date suffix are aliases.
func isModelAlias(id string) bool {
	return strings.HasSuffix(id, "-latest") || !modelDatePattern.MatchString(id)
}

// buildFallbackModel ports pi's buildFallbackModel: a copy of the provider's
// default model, or its first, with the requested custom id. providerModels
// is never empty.
func buildFallbackModel(providerModels []*ai.Model, modelID string) *ai.Model {
	base := providerModels[0]
	if defaultID, ok := defaultModelPerProvider[string(base.Provider)]; ok {
		if m := findModel(providerModels, func(m *ai.Model) bool { return m.ID == defaultID }); m != nil {
			base = m
		}
	}
	clone := *base
	clone.ID = modelID
	clone.Name = modelID
	return &clone
}

func filterModels(models []*ai.Model, keep func(*ai.Model) bool) []*ai.Model {
	var out []*ai.Model
	for _, m := range models {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

func findModel(models []*ai.Model, match func(*ai.Model) bool) *ai.Model {
	if i := slices.IndexFunc(models, match); i != -1 {
		return models[i]
	}
	return nil
}

// providerCount is how many distinct providers offer models.
func providerCount(models []*ai.Model) int {
	providers := map[ai.ProviderId]bool{}
	for _, m := range models {
		providers[m.Provider] = true
	}
	return len(providers)
}
