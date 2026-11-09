package ai

import (
	"encoding/json"
	"math"
)

// Port of pi's packages/ai/src/cache-warming.ts (upstream c9552c7d8): the
// prompt cache warming policy shared by pi's hosts — when a refresh is due,
// whether a request can be replayed, and whether a refresh pays for itself.
// Hosts own the scheduling; this file owns the numbers. pi's own scheduler,
// coding-agent's CacheWarmer, is Scope queue row 17.

// CacheWarmingAction is whether a refresh is sent (CacheWarmingWarm) or
// warming ends until the next real request (CacheWarmingStop).
type CacheWarmingAction string

const (
	CacheWarmingWarm CacheWarmingAction = "warm"
	CacheWarmingStop CacheWarmingAction = "stop"
)

// CacheWarmingPhase is CacheWarmingStreaming while the agent run that sent the
// request is still active, CacheWarmingIdle after it settled.
type CacheWarmingPhase string

const (
	CacheWarmingStreaming CacheWarmingPhase = "streaming"
	CacheWarmingIdle      CacheWarmingPhase = "idle"
)

// CacheWarmingDecision holds the inputs and outcome of one warm-or-stop
// decision. Its fields marshal in pi's key order.
type CacheWarmingDecision struct {
	Phase CacheWarmingPhase `json:"phase"`
	// WarmCost is the price of this refresh: a cache read of the prompt plus
	// one output token.
	WarmCost float64 `json:"warmCost"`
	// MissCost is the extra price of the next real request if the cache entry
	// is lost.
	MissCost float64 `json:"missCost"`
	// ContinuationProbability is the estimated chance that a real request
	// arrives before the entry expires.
	ContinuationProbability float64 `json:"continuationProbability"`
	// ExpectedSavings is ContinuationProbability * MissCost - WarmCost.
	ExpectedSavings float64 `json:"expectedSavings"`
	// EconomicsAvailable is false when the prompt size or the model's prices
	// are unknown.
	EconomicsAvailable bool `json:"economicsAvailable"`
	// Action is CacheWarmingWarm when ExpectedSavings is at least
	// CacheWarmingMinimumExpectedSavings.
	Action CacheWarmingAction `json:"action"`
}

const (
	// CacheWarmingMaxAgeMs: streaming warming never continues past this long
	// after the real request that started it.
	CacheWarmingMaxAgeMs = 60 * 60_000
	// CacheWarmingMaxIdleAgeMs: idle warming uses a shorter horizon because
	// continuation estimates become less reliable with age.
	CacheWarmingMaxIdleAgeMs = 30 * 60_000
	// CacheWarmingMinimumExpectedSavings: a refresh is sent only when it is
	// expected to save at least this many dollars.
	CacheWarmingMinimumExpectedSavings = 0.05
	// CacheWarmingIdleContinuationProbability is the chance that a real
	// request arrives before the cache entry expires while the agent sits
	// idle. pi measured it from its own usage; per-session estimates were not
	// better than this constant.
	CacheWarmingIdleContinuationProbability = 0.15
)

// CacheWarmingDelayMs returns when to refresh a cache entry that lives ttlMs:
// at 90% of the TTL while preserving at least ten seconds of margin (pi
// getCacheWarmingDelayMs). ok is false when the TTL leaves no such margin.
// Milliseconds stay float64, as pi's numbers are, because a TTL from
// PromptCacheTTLMs need not be whole.
func CacheWarmingDelayMs(ttlMs float64) (delayMs float64, ok bool) {
	if ttlMs <= 10_000 {
		return 0, false
	}
	return math.Max(1, math.Floor(math.Min(ttlMs*0.9, ttlMs-10_000))), true
}

// PromptCacheTTLMs returns the lifetime of the prompt cache entry a request
// writes, from the model's PromptCache tier for the retention the request used
// (pi getPromptCacheTtlMs): opts.CacheRetention, else long when
// PI_CACHE_RETENTION is "long", else short. ok is false when the model has no
// lifetime for that tier or caching is off. A nil opts is pi's undefined
// options.
func PromptCacheTTLMs(model *Model, opts *StreamOptions) (ttlMs float64, ok bool) {
	var retention CacheRetention
	var env map[string]string
	if opts != nil {
		retention, env = opts.CacheRetention, opts.Env
	}
	if retention == "" {
		retention = CacheShort
		if ProviderEnvValue("PI_CACHE_RETENTION", env) == "long" {
			retention = CacheLong
		}
	}
	if model.PromptCache == nil {
		return 0, false
	}
	var seconds *float64
	switch retention {
	case CacheShort:
		seconds = model.PromptCache.Short
	case CacheLong:
		seconds = model.PromptCache.Long
	}
	if seconds == nil {
		return 0, false
	}
	return *seconds * 1000, true
}

// CacheWarmingReplayable reports whether replaying the request with a
// one-token output cap leaves its cache entry untouched (pi
// isCacheWarmingReplayable). Anthropic's budget-based thinking (Claude models
// without adaptive thinking) derives budget_tokens from max_tokens; the replay
// would get a different budget, which Anthropic keys the message cache on, and
// the model could still think for thousands of tokens. A nil opts is pi's
// undefined options.
func CacheWarmingReplayable(model *Model, opts *SimpleStreamOptions) bool {
	if opts == nil || opts.Reasoning == "" || model.Api != APIAnthropicMessages {
		return true
	}
	return compatFlagTrue(model.Compat, "forceAdaptiveThinking")
}

// compatFlagTrue reports whether the model's compat object has key set to the
// JSON value true, as pi's `compat?.[key] === true` does: the key is matched
// exactly (encoding/json's struct decoding would also match other casings), a
// repeated key's last value wins as in JSON.parse, and anything that is not an
// object reads as no compat.
func compatFlagTrue(compat json.RawMessage, key string) bool {
	var fields map[string]any
	if json.Unmarshal(compat, &fields) != nil {
		return false
	}
	return fields[key] == true
}

// CacheWarmingRequestOptions returns the options of a refresh: the request's
// own, capped at one output token, without retries, and without replacing the
// conversation's provider continuation state, since the refresh repeats the
// last request (pi getCacheWarmingRequestOptions). pi also swaps in the
// warmer's abort signal; in Go the caller streams the refresh under its own
// context instead.
func CacheWarmingRequestOptions(opts StreamOptions) StreamOptions {
	maxTokens := 1
	opts.MaxTokens = &maxTokens
	opts.DisableContinuation = true
	opts.MaxRetries = 0
	return opts
}

// EvaluateCacheWarming decides whether refreshing the cache entry of a
// promptTokens-token request pays for itself in phase (pi
// evaluateCacheWarming). The prices come from CalculateCost, so a pricing tier
// applies as it would to the requests themselves.
func EvaluateCacheWarming(model *Model, promptTokens int, phase CacheWarmingPhase) CacheWarmingDecision {
	cacheHitCost := cacheWarmingPrice(model, Usage{CacheRead: promptTokens})
	miss := Usage{Input: promptTokens}
	if model.Cost.CacheWrite > 0 {
		miss = Usage{CacheWrite: promptTokens}
	}
	cacheMissCost := cacheWarmingPrice(model, miss)
	warmCost := cacheWarmingPrice(model, Usage{CacheRead: promptTokens, Output: 1})
	missCost := math.Max(0, cacheMissCost-cacheHitCost)
	continuationProbability := 1.0
	if phase == CacheWarmingIdle {
		continuationProbability = CacheWarmingIdleContinuationProbability
	}
	economicsAvailable := promptTokens > 0 && (cacheHitCost > 0 || cacheMissCost > 0)
	// The conversion rounds the product before the subtraction, as V8 does;
	// without it the compiler may fuse the two into one FMA (see CalculateCost).
	expectedSavings := float64(continuationProbability*missCost) - warmCost
	action := CacheWarmingStop
	if expectedSavings >= CacheWarmingMinimumExpectedSavings {
		action = CacheWarmingWarm
	}
	return CacheWarmingDecision{
		Phase:                   phase,
		WarmCost:                warmCost,
		MissCost:                missCost,
		ContinuationProbability: continuationProbability,
		ExpectedSavings:         expectedSavings,
		EconomicsAvailable:      economicsAvailable,
		Action:                  action,
	}
}

// cacheWarmingPrice is the catalog price of usage's tokens (pi's price
// helper).
func cacheWarmingPrice(model *Model, usage Usage) float64 {
	return CalculateCost(model, &usage).Total
}
