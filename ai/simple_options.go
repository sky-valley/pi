package ai

import "maps"

// Ported from pi packages/ai/src/api/simple-options.ts (upstream 09f10595).
// pi's buildBaseOptions sets the base maxTokens to
// clampMaxTokensToContext(model, context, options?.maxTokens ?? model.maxTokens)
// and the base samplingParams to resolveSamplingParams(model, options?.reasoning
// ?? "off", options?.samplingParams) (upstream 76dfb88f6). The Go port has no
// single buildBaseOptions — each provider's StreamSimple copies StreamOptions
// directly — so the providers call ClampMaxTokensToContext,
// SimpleMaxTokensDefault and ResolveSamplingParams to reproduce the same base.

const (
	contextSafetyTokens = 4096
	minMaxTokens        = 1
)

// ClampMaxTokensToContext caps maxTokens to what fits in the model's context
// window after the estimated context and a safety margin, mirroring pi's
// clampMaxTokensToContext. Models with no known context window only get the
// MIN_MAX_TOKENS floor applied.
func ClampMaxTokensToContext(model *Model, context TranscriptContext, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return max(minMaxTokens, maxTokens)
	}
	available := model.ContextWindow - estimateContextTokens(context.Messages).Tokens - contextSafetyTokens
	return min(maxTokens, max(minMaxTokens, available))
}

// SimpleMaxTokensDefault returns the maxTokens that pi's buildBaseOptions feeds
// into the clamp: the caller's explicit maxTokens when set, else model.maxTokens
// (pi: options?.maxTokens ?? model.maxTokens). opts.MaxTokens is treated as
// "set" only when non-nil, matching pi's `??`.
func SimpleMaxTokensDefault(model *Model, opts *SimpleStreamOptions) int {
	if opts != nil && opts.MaxTokens != nil {
		return *opts.MaxTokens
	}
	return model.MaxTokens
}

// ResolveSamplingParams is pi's resolveSamplingParams: the model's default
// sampling parameters, overridden per key by its SamplingParamsByThinkingLevel
// entry for the effective level — ClampThinkingLevel of level — overridden by
// the request's. Nil when none of the three is set.
func ResolveSamplingParams(model *Model, level ModelThinkingLevel, request map[string]any) map[string]any {
	byLevel := model.SamplingParamsByThinkingLevel[ClampThinkingLevel(model, level)]
	if model.SamplingParams == nil && byLevel == nil && request == nil {
		return nil
	}
	resolved := make(map[string]any, len(model.SamplingParams)+len(byLevel)+len(request))
	maps.Copy(resolved, model.SamplingParams)
	maps.Copy(resolved, byLevel)
	maps.Copy(resolved, request)
	return resolved
}

// SimpleSamplingParams returns the samplingParams pi's buildBaseOptions gives a
// simple stream: resolveSamplingParams(model, options?.reasoning ?? "off",
// options?.samplingParams).
func SimpleSamplingParams(model *Model, opts *SimpleStreamOptions) map[string]any {
	level, request := ModelThinkingLevel("off"), map[string]any(nil)
	if opts != nil {
		if opts.Reasoning != "" {
			level = ModelThinkingLevel(opts.Reasoning)
		}
		request = opts.SamplingParams
	}
	return ResolveSamplingParams(model, level, request)
}
