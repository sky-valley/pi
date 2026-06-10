package coding

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/sky-valley/pi/ai"
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
	"amazon-bedrock":         "us.anthropic.claude-opus-4-6-v1",
	"ant-ling":               "Ring-2.6-1T",
	"anthropic":              "claude-opus-4-8",
	"openai":                 "gpt-5.4",
	"azure-openai-responses": "gpt-5.4",
	"openai-codex":           "gpt-5.5",
	"nvidia":                 "nvidia/nemotron-3-super-120b-a12b",
	"deepseek":               "deepseek-v4-pro",
	"google":                 "gemini-3.1-pro-preview",
	"google-vertex":          "gemini-3.1-pro-preview",
	"github-copilot":         "gpt-5.4",
	"openrouter":             "moonshotai/kimi-k2.6",
	"vercel-ai-gateway":      "zai/glm-5.1",
	"xai":                    "grok-4.20-0309-reasoning",
	"groq":                   "openai/gpt-oss-120b",
	"cerebras":               "zai-glm-4.7",
	"zai":                    "glm-5.1",
	"zai-coding-cn":          "glm-5.1",
	"mistral":                "devstral-medium-latest",
	"minimax":                "MiniMax-M2.7",
	"minimax-cn":             "MiniMax-M2.7",
	"moonshotai":             "kimi-k2.6",
	"moonshotai-cn":          "kimi-k2.6",
	"huggingface":            "moonshotai/Kimi-K2.6",
	"fireworks":              "accounts/fireworks/models/kimi-k2p6",
	"together":               "moonshotai/Kimi-K2.6",
	"opencode":               "kimi-k2.6",
	"opencode-go":            "kimi-k2.6",
	"kimi-coding":            "kimi-for-coding",
	"cloudflare-workers-ai":  "@cf/moonshotai/kimi-k2.6",
	"cloudflare-ai-gateway":  "workers-ai/@cf/moonshotai/kimi-k2.6",
	"xiaomi":                 "mimo-v2.5-pro",
	"xiaomi-token-plan-cn":   "mimo-v2.5-pro",
	"xiaomi-token-plan-ams":  "mimo-v2.5-pro",
	"xiaomi-token-plan-sgp":  "mimo-v2.5-pro",
}

// validThinkingLevels mirrors pi's VALID_THINKING_LEVELS (cli/args.ts:57).
var validThinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// ResolvedModel is the result of resolving a model spec.
type ResolvedModel struct {
	Model *ai.Model
	// ThinkingLevel is the level parsed from a ":<level>" suffix in the spec
	// (pi parseModelPattern), or "" when the spec carried none.
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

// ResolveModelPattern ports pi's resolveCliModel (model-resolver.ts): a
// slash-prefix is treated as a provider ONLY when it matches a known provider;
// otherwise the whole string is matched as a model id across providers
// (OpenRouter-style ids contain slashes). Matching is case-insensitive and a
// ":<thinkingLevel>" suffix (off|minimal|low|medium|high|xhigh) is parsed off
// and returned alongside. An unknown model under a known provider falls back
// to a synthetic custom-id model with a warning (pi buildFallbackModel).
func ResolveModelPattern(spec string) (ResolvedModel, error) {
	if spec == "" {
		spec = DefaultModelSpec
	}
	availableModels := allCatalogModels()
	if len(availableModels) == 0 {
		return ResolvedModel{}, fmt.Errorf("No models available. Check your installation or add models to models.json.")
	}

	// Canonical provider lookup (case-insensitive).
	providerByLower := map[string]string{}
	for _, m := range availableModels {
		providerByLower[strings.ToLower(string(m.Provider))] = string(m.Provider)
	}

	pattern := spec
	provider := ""
	inferredProvider := false

	// Interpret "provider/model" only when the prefix before the FIRST slash is
	// a known provider; otherwise the slash belongs to the model id.
	if slash := strings.Index(spec, "/"); slash != -1 {
		if canonical, ok := providerByLower[strings.ToLower(spec[:slash])]; ok {
			provider = canonical
			pattern = spec[slash+1:]
			inferredProvider = true
		}
	}

	// No provider inferred: try exact matches on the raw input first (handles
	// model ids that naturally contain slashes).
	if provider == "" {
		if m := exactModelMatch(spec, availableModels); m != nil {
			return ResolvedModel{Model: m}, nil
		}
	}

	candidates := availableModels
	if provider != "" {
		candidates = nil
		for _, m := range availableModels {
			if string(m.Provider) == provider {
				candidates = append(candidates, m)
			}
		}
	}
	model, level, warning := parseModelPattern(pattern, candidates, false)
	if model != nil {
		return ResolvedModel{Model: model, ThinkingLevel: level, Warning: warning}, nil
	}

	// Provider inferred from the slash but nothing matched within it: fall back
	// to matching the full input as a raw model id across all models
	// (e.g. "openai/gpt-4o:extended" is an openrouter model id).
	if inferredProvider {
		if m := exactModelMatch(spec, availableModels); m != nil {
			return ResolvedModel{Model: m}, nil
		}
		if m, lvl, warn := parseModelPattern(spec, availableModels, false); m != nil {
			return ResolvedModel{Model: m, ThinkingLevel: lvl, Warning: warn}, nil
		}
	}

	if provider != "" {
		if fb := buildFallbackModel(provider, pattern, availableModels); fb != nil {
			fbWarning := fmt.Sprintf("Model %q not found for provider %q. Using custom model id.", pattern, provider)
			if warning != "" {
				fbWarning = warning + " " + fbWarning
			}
			return ResolvedModel{Model: fb, Warning: fbWarning}, nil
		}
	}

	display := spec
	if provider != "" {
		display = provider + "/" + pattern
	}
	return ResolvedModel{Warning: warning},
		fmt.Errorf("Model \"%s\" not found. Use --list-models to see available models.", display)
}

// allCatalogModels lists every registered model in deterministic order
// (providers and ids sorted; pi's registry order is models.json order).
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

// exactModelMatch finds a model whose id or "provider/id" equals the reference
// case-insensitively (resolveCliModel's pre-provider exact check).
func exactModelMatch(reference string, models []*ai.Model) *ai.Model {
	lower := strings.ToLower(reference)
	for _, m := range models {
		if strings.ToLower(m.ID) == lower || strings.ToLower(string(m.Provider)+"/"+m.ID) == lower {
			return m
		}
	}
	return nil
}

// parseModelPattern ports pi's parseModelPattern: try the full pattern as a
// model; otherwise split on the LAST colon — a valid thinking-level suffix
// recurses on the prefix and surfaces the level; an invalid suffix either
// recurses with a warning (scope mode) or fails (strict mode, used for CLI
// --model parsing, allowInvalidLevelFallback=false).
func parseModelPattern(pattern string, models []*ai.Model, allowInvalidLevelFallback bool) (*ai.Model, string, string) {
	if m := tryMatchModel(pattern, models); m != nil {
		return m, "", ""
	}
	lastColon := strings.LastIndex(pattern, ":")
	if lastColon == -1 {
		return nil, "", ""
	}
	prefix := pattern[:lastColon]
	suffix := pattern[lastColon+1:]
	if validThinkingLevels[suffix] {
		m, _, warning := parseModelPattern(prefix, models, allowInvalidLevelFallback)
		if m != nil {
			level := suffix
			if warning != "" {
				level = "" // pi: only use the level if the inner recursion was clean
			}
			return m, level, warning
		}
		return m, "", warning
	}
	if !allowInvalidLevelFallback {
		// Strict mode: treat the suffix as part of the model id and fail rather
		// than accidentally resolving to a different model.
		return nil, "", ""
	}
	m, _, _ := parseModelPattern(prefix, models, allowInvalidLevelFallback)
	if m != nil {
		return m, "", fmt.Sprintf("Invalid thinking level %q in pattern %q. Using default instead.", suffix, pattern)
	}
	return nil, "", ""
}

// tryMatchModel ports pi's tryMatchModel: exact reference match first, then
// case-insensitive substring matching on id/name, preferring aliases (un-dated
// ids) and otherwise the latest dated version.
func tryMatchModel(pattern string, models []*ai.Model) *ai.Model {
	if m := findExactModelReferenceMatch(pattern, models); m != nil {
		return m
	}
	lower := strings.ToLower(pattern)
	var matches []*ai.Model
	for _, m := range models {
		if strings.Contains(strings.ToLower(m.ID), lower) || strings.Contains(strings.ToLower(m.Name), lower) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	var aliases, dated []*ai.Model
	for _, m := range matches {
		if isModelAlias(m.ID) {
			aliases = append(aliases, m)
		} else {
			dated = append(dated, m)
		}
	}
	pick := func(ms []*ai.Model) *ai.Model {
		// pi sorts descending by id (b.localeCompare(a)) and takes the first.
		sort.Slice(ms, func(i, j int) bool { return ms[i].ID > ms[j].ID })
		return ms[0]
	}
	if len(aliases) > 0 {
		return pick(aliases)
	}
	return pick(dated)
}

// findExactModelReferenceMatch ports pi's findExactModelReferenceMatch:
// a canonical "provider/id" match, then a provider+id split match, then a bare
// id match — each only when unambiguous (a unique match).
func findExactModelReferenceMatch(reference string, models []*ai.Model) *ai.Model {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return nil
	}
	lower := strings.ToLower(trimmed)

	var canonical []*ai.Model
	for _, m := range models {
		if strings.ToLower(string(m.Provider)+"/"+m.ID) == lower {
			canonical = append(canonical, m)
		}
	}
	if len(canonical) == 1 {
		return canonical[0]
	}
	if len(canonical) > 1 {
		return nil
	}

	if slash := strings.Index(trimmed, "/"); slash != -1 {
		provider := strings.TrimSpace(trimmed[:slash])
		modelID := strings.TrimSpace(trimmed[slash+1:])
		if provider != "" && modelID != "" {
			var byProvider []*ai.Model
			for _, m := range models {
				if strings.EqualFold(string(m.Provider), provider) && strings.EqualFold(m.ID, modelID) {
					byProvider = append(byProvider, m)
				}
			}
			if len(byProvider) == 1 {
				return byProvider[0]
			}
			if len(byProvider) > 1 {
				return nil
			}
		}
	}

	var byID []*ai.Model
	for _, m := range models {
		if strings.ToLower(m.ID) == lower {
			byID = append(byID, m)
		}
	}
	if len(byID) == 1 {
		return byID[0]
	}
	return nil
}

var modelDatePattern = regexp.MustCompile(`-\d{8}$`)

// isModelAlias ports pi's isAlias: -latest ids and ids without a -YYYYMMDD
// date suffix are aliases.
func isModelAlias(id string) bool {
	if strings.HasSuffix(id, "-latest") {
		return true
	}
	return !modelDatePattern.MatchString(id)
}

// buildFallbackModel ports pi's buildFallbackModel: clone the provider's
// default (or first) model with the requested custom id.
func buildFallbackModel(provider, modelID string, models []*ai.Model) *ai.Model {
	var providerModels []*ai.Model
	for _, m := range models {
		if string(m.Provider) == provider {
			providerModels = append(providerModels, m)
		}
	}
	if len(providerModels) == 0 {
		return nil
	}
	base := providerModels[0]
	if defaultID, ok := defaultModelPerProvider[provider]; ok {
		for _, m := range providerModels {
			if m.ID == defaultID {
				base = m
				break
			}
		}
	}
	clone := *base
	clone.ID = modelID
	clone.Name = modelID
	return &clone
}
