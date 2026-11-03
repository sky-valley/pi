package ai

import (
	"maps"
	"math"
)

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
	// contextSafetyRatio: OpenRouter counts the input with its own tokenizer
	// and rejects input + max_tokens above the window before routing; it
	// counted prose up to 12% above the provider's count (#10497, upstream
	// 0a4fad548).
	contextSafetyRatio = 0.15
	// minOutputWithRatioMargin: the 15% margin is kept only while it leaves at
	// least this much output. Otherwise (a nearly full large window, or any
	// small window such as a 32k local model) only the fixed margin is kept,
	// and output is capped at this size.
	minOutputWithRatioMargin = 32768
	minMaxTokens             = 1
)

// ClampMaxTokensToContext caps maxTokens to what fits in the model's context
// window, leaving room for the input being counted higher than estimated,
// mirroring pi's clampMaxTokensToContext. Models with no known context window
// only get the MIN_MAX_TOKENS floor applied.
func ClampMaxTokensToContext(model *Model, context TranscriptContext, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return max(minMaxTokens, maxTokens)
	}
	input := estimateContextTokens(context.Messages).Tokens
	// pi's Math.ceil(input * 0.15), in the same float64 arithmetic.
	margin := max(contextSafetyTokens, int(math.Ceil(float64(input)*contextSafetyRatio)))
	room := model.ContextWindow - input - margin
	if room < minOutputWithRatioMargin {
		room = min(minOutputWithRatioMargin, model.ContextWindow-input-contextSafetyTokens)
	}
	return min(maxTokens, max(minMaxTokens, room))
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
