package providers

import (
	"math"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// OpenRouter's billed cost, ported from pi utils/openrouter-cost.ts (upstream
// cae8d69e5, #9980). OpenRouter adds these fields to streamed usage although
// neither its OpenAPI schema nor the SDKs type them:
//
//	cost          amount charged to the user's OpenRouter account
//	is_byok       whether OpenRouter used the user's upstream provider key
//	cost_details  upstream_inference_cost: the inference billed through that
//	              upstream account, when available; its prompt and completion
//	              parts; server_tool_cost (already inside cost)

// isOpenRouterModel is pi's isOpenRouterModel: the built-in provider, or any
// provider whose base URL contains OpenRouter's host.
func isOpenRouterModel(model *ai.Model) bool {
	return model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai")
}

// validOpenRouterCost is pi's isValidCost: a finite, non-negative number.
func validOpenRouterCost(value any) (float64, bool) {
	f, ok := value.(float64)
	return f, ok && !math.IsInf(f, 0) && !math.IsNaN(f) && f >= 0
}

// applyOpenRouterReportedCost is pi's applyOpenRouterReportedCost: it replaces
// the catalog total with OpenRouter's billed cost when reported, the usage
// object as JSON.parse read it (ai.DecodeOrderedValue's shapes), holds
// complete, valid accounting. Without a key the charge is the cost; with one
// it is the cost plus the upstream inference cost, or its two parts when
// OpenRouter omits that total (null). Anything else leaves the usage alone.
func applyOpenRouterReportedCost(usage *ai.Usage, reported any) {
	cost, ok := validOpenRouterCost(jsGet(reported, "cost"))
	if !ok {
		return
	}
	switch jsGet(reported, "is_byok") {
	case false:
		usage.Cost.Total = cost
		usage.Cost.TotalFromProvider = ai.OptBoolTrue
	case true:
		details := jsGet(reported, "cost_details")
		if !jsTruthy(details) {
			return
		}
		upstream := jsGet(details, "upstream_inference_cost")
		if upstream == nil {
			prompt, promptOK := validOpenRouterCost(jsGet(details, "upstream_inference_prompt_cost"))
			completions, completionsOK := validOpenRouterCost(jsGet(details, "upstream_inference_completions_cost"))
			if !promptOK || !completionsOK {
				return
			}
			upstream = prompt + completions
		}
		upstreamCost, ok := validOpenRouterCost(upstream)
		if !ok {
			return
		}
		usage.Cost.Total = cost + upstreamCost
		usage.Cost.TotalFromProvider = ai.OptBoolTrue
	}
}
