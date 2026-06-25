package ai

// Ported from pi packages/ai/src/api/simple-options.ts (upstream 09f10595).
// pi's buildBaseOptions sets the base maxTokens to
// clampMaxTokensToContext(model, context, options?.maxTokens ?? model.maxTokens).
// The Go port has no single buildBaseOptions — each provider's StreamSimple
// copies StreamOptions directly — so the providers call ClampMaxTokensToContext
// and SimpleMaxTokensDefault to reproduce the same maxTokens.

const (
	contextSafetyTokens = 4096
	minMaxTokens        = 1
)

// ClampMaxTokensToContext caps maxTokens to what fits in the model's context
// window after the estimated context and a safety margin, mirroring pi's
// clampMaxTokensToContext. Models with no known context window only get the
// MIN_MAX_TOKENS floor applied.
func ClampMaxTokensToContext(model *Model, context Context, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return max(minMaxTokens, maxTokens)
	}
	available := model.ContextWindow - estimateContextTokens(context).Tokens - contextSafetyTokens
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
