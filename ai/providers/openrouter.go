package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// OpenRouter's model list for a key (pi providers/openrouter.ts and
// providers/openrouter-api.ts, upstream ee409a85f).

const (
	openRouterBaseURL           = "https://openrouter.ai/api/v1"
	openRouterUserModelsTimeout = 15 * time.Second
)

// openRouterFetch is the stand-in for the globalThis.fetch pi's
// personalizeOpenRouterCatalog calls; tests replace it as pi's spy on fetch
// does.
var openRouterFetch ai.HTTPDoer = http.DefaultClient

// PersonalizeOpenRouterCatalog restricts an OpenRouter catalog to the chat
// models the key may use, with the key's limits and prices from
// GET /models/user. Image and classifier models are returned as they are. A
// missing limit keeps the catalog value. The request uses the provider's
// configured base URL, refresh.BaseURL, so regional endpoints such as
// https://us.openrouter.ai/api/v1 apply their region's filtering. When the
// endpoint rejects the key (401/403) or does not exist (404), for example with
// a proxy configured in models.json, the catalog is returned unchanged; so it
// is when the refresh has no key.
//
// The request is bounded by ctx and a 15-second timeout. The response is read
// as pi reads it: JSON.parse's SyntaxError for a body that is not JSON, and
// "Invalid OpenRouter model list response" for one without a data array.
// Listed values are JavaScript values: a limit counts when it is truthy, and a
// price is parseFloat of its string form.
func PersonalizeOpenRouterCatalog(ctx context.Context, catalog []*ai.Model, refresh ai.RefreshModelsContext) ([]*ai.Model, error) {
	var key string
	if credential := refresh.Credential; credential != nil {
		if credential.Type == ai.CredentialOAuth {
			key = credential.Access
		} else {
			key = credential.Key
		}
	}
	if key == "" {
		return catalog, nil
	}
	baseURL := refresh.BaseURL
	if baseURL == "" {
		baseURL = openRouterBaseURL
	}

	ctx, cancel := context.WithTimeout(ctx, openRouterUserModelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models/user", nil)
	if err != nil {
		return nil, fmt.Errorf("OpenRouter model list request: %w; check the provider's baseUrl", err)
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("authorization", "Bearer "+key)
	resp, err := openRouterFetch.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return catalog, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("OpenRouter model list request failed: %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	text := jstext.DecodeText(raw)
	if err := jstext.JSONSyntaxError(text); err != nil {
		return nil, err
	}
	body, err := jstext.Parse([]byte(text))
	if err != nil {
		// Unreachable short of encoding/json's nesting limit: JSON.parse
		// accepted the body.
		return nil, err
	}
	object, _ := body.(map[string]any)
	entries, isArray := object["data"].([]any)
	if !isArray {
		return nil, errors.New("Invalid OpenRouter model list response")
	}
	allowed := make(map[string]map[string]any, len(entries))
	for _, entry := range entries {
		if fields, ok := entry.(map[string]any); ok {
			if id, ok := fields["id"].(string); ok {
				allowed[id] = fields
			}
		}
	}

	out := make([]*ai.Model, 0, len(catalog))
	for _, model := range catalog {
		if !ai.IsModelType(model, ai.ModelTypeChat) {
			out = append(out, model)
			continue
		}
		entry, ok := allowed[model.ID]
		if !ok {
			continue
		}
		personalized := *model
		topProvider, _ := entry["top_provider"].(map[string]any)
		// `||`: OpenRouter reports a missing limit as absent, null or 0, like
		// the generator's fallback.
		if limit, ok := openRouterLimit(topProvider["context_length"], entry["context_length"]); ok {
			personalized.ContextWindow = limit
		}
		if limit, ok := openRouterLimit(topProvider["max_completion_tokens"]); ok {
			personalized.MaxTokens = limit
		}
		if pricing := entry["pricing"]; jstext.Truthy(pricing) {
			personalized.Cost = openRouterCost(pricing)
		}
		out = append(out, &personalized)
	}
	return out, nil
}

// openRouterLimit is the first truthy value of a `||` chain, as a token count.
func openRouterLimit(values ...any) (int, bool) {
	for _, value := range values {
		if jstext.Truthy(value) {
			n, _ := jstext.ToNumber(value)
			return int(n), true
		}
	}
	return 0, false
}

// openRouterField is `value?.[name]` on a JSON value.
func openRouterField(value any, name string) any {
	fields, _ := value.(map[string]any)
	return fields[name]
}

// openRouterCost converts OpenRouter's $/token prices (pricing, a JSON value)
// to pi's $/1M tokens. Missing, invalid and negative base prices are 0.
// Prompt-length overrides become request-wide tiers; a rate a tier does not
// list keeps the base price. Time-of-day overrides are skipped because
// ModelCost cannot express them: an override counts as one when it has any
// utc_start, utc_end or utc_days key, even a null one, and as a prompt-length
// override when it has a min_prompt_tokens key.
//
// pi keeps a null min_prompt_tokens as inputTokensAbove: null, which prices as
// 0 does; InputTokensAbove is an int, so it is 0 here.
func openRouterCost(pricing any) ai.ModelCost {
	base := ai.ModelCost{
		Input:      openRouterPerMillion(openRouterField(pricing, "prompt"), 0),
		Output:     openRouterPerMillion(openRouterField(pricing, "completion"), 0),
		CacheRead:  openRouterPerMillion(openRouterField(pricing, "input_cache_read"), 0),
		CacheWrite: openRouterPerMillion(openRouterField(pricing, "input_cache_write"), 0),
	}
	overrides, _ := openRouterField(pricing, "overrides").([]any)
	for _, value := range overrides {
		override, _ := value.(map[string]any)
		minPromptTokens, hasMin := override["min_prompt_tokens"]
		_, hasStart := override["utc_start"]
		_, hasEnd := override["utc_end"]
		_, hasDays := override["utc_days"]
		if !hasMin || hasStart || hasEnd || hasDays {
			continue
		}
		inputTokensAbove, _ := jstext.ToNumber(minPromptTokens)
		base.Tiers = append(base.Tiers, ai.ModelCostTier{
			InputTokensAbove: int(inputTokensAbove),
			Input:            openRouterPerMillion(override["prompt"], base.Input),
			Output:           openRouterPerMillion(override["completion"], base.Output),
			CacheRead:        openRouterPerMillion(override["input_cache_read"], base.CacheRead),
			CacheWrite:       openRouterPerMillion(override["input_cache_write"], base.CacheWrite),
		})
	}
	return base
}

// openRouterPerMillion converts one $/token price to $/1M tokens:
// Number((parseFloat(value ?? "") * 1_000_000).toFixed(6)). 0 means free.
// Missing, invalid and negative prices use the fallback: OpenRouter reports
// -1 for router models such as openrouter/auto, whose price depends on the
// model they pick.
func openRouterPerMillion(value any, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	text, ok := jstext.ToString(value)
	if !ok {
		return fallback
	}
	perToken, ok := parseFloatPrefix(text)
	if !ok || !(perToken >= 0) {
		return fallback
	}
	return jstext.StringToNumber(jstext.ToFixed(perToken*1_000_000, 6))
}
