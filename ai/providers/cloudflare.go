package providers

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// Cloudflare base URLs with {VAR} placeholders, ported from pi cloudflare.ts.
const (
	// cloudflareWorkersAIBaseURL is the Workers AI direct endpoint.
	cloudflareWorkersAIBaseURL = "https://api.cloudflare.com/client/v4/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai/v1"
	// cloudflareAIGatewayCompatBaseURL is the AI Gateway Unified API.
	// https://developers.cloudflare.com/ai-gateway/usage/unified-api/
	cloudflareAIGatewayCompatBaseURL = "https://gateway.ai.cloudflare.com/v1/{CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}/compat"
	// cloudflareAIGatewayOpenAIBaseURL is the AI Gateway → OpenAI passthrough.
	// Used until /compat supports /v1/responses.
	cloudflareAIGatewayOpenAIBaseURL = "https://gateway.ai.cloudflare.com/v1/{CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}/openai"
	// cloudflareAIGatewayAnthropicBaseURL is the AI Gateway → Anthropic passthrough.
	cloudflareAIGatewayAnthropicBaseURL = "https://gateway.ai.cloudflare.com/v1/{CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}/anthropic"
)

// isCloudflareProvider reports whether provider is one of pi's Cloudflare
// providers (cloudflare.ts isCloudflareProvider).
func isCloudflareProvider(provider ai.ProviderId) bool {
	return provider == "cloudflare-workers-ai" || provider == "cloudflare-ai-gateway"
}

// cloudflarePlaceholderRe matches {VAR} placeholders, mirroring pi's
// /\{([A-Z_][A-Z0-9_]*)\}/g.
var cloudflarePlaceholderRe = regexp.MustCompile(`\{([A-Z_][A-Z0-9_]*)\}`)

// resolveCloudflareBaseURL substitutes `{VAR}` placeholders in a Cloudflare
// baseUrl from the environment (cloudflare.ts resolveCloudflareBaseUrl). It
// errors with pi's exact message when a referenced variable is unset or empty.
// Provider-scoped env overrides win over the OS environment (pi 7f29e7a3).
func resolveCloudflareBaseURL(model *ai.Model, env map[string]string) (string, error) {
	url := model.BaseURL
	if !strings.Contains(url, "{") {
		return url, nil
	}
	var missing string
	resolved := cloudflarePlaceholderRe.ReplaceAllStringFunc(url, func(match string) string {
		name := match[1 : len(match)-1]
		value := getProviderEnvValue(name, env)
		if value == "" {
			if missing == "" {
				missing = name
			}
			return match
		}
		return value
	})
	if missing != "" {
		return "", fmt.Errorf("%s is required for provider %s but is not set.", missing, model.Provider)
	}
	return resolved, nil
}
