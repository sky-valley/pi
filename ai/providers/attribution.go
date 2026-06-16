package providers

import (
	"net/url"
	"os"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// Provider attribution headers, ported faithfully from pi
// core/provider-attribution.ts (at upstream f8a77f47, which adds the Vercel AI
// Gateway branch). pi merges these in sdk.ts streamFn via
// mergeProviderAttributionHeaders, which builds
//
//	{ ...getSessionHeaders(), ...getDefaultAttributionHeaders() }
//
// then Object.assigns auth.headers (= {...model.headers, ...providerHeaders,
// ...modelHeaders}) and finally options.headers (the consumer's). Later spreads
// win, so the effective precedence (low → high) is:
//
//	1. session-attribution (x-opencode-session, x-opencode-client)  — LOWEST
//	2. default-attribution (HTTP-Referer, X-OpenRouter-Title, ...)
//	3. auth.headers        (model.Headers + provider/model auth headers)
//	4. options.headers     (consumer-supplied opts.Headers)         — HIGHEST
//
// In the Go port (http.Header.Set = "last write wins") we reproduce this by
// emitting the attribution headers FIRST (session, then default), then
// model.Headers and the provider-specific headers, then opts.Headers. So both
// model.Headers and opts.Headers override the attribution defaults — see
// applyAttributionDefaults and the per-provider call sites.

const (
	attrOpenRouterHost          = "openrouter.ai"
	attrNvidiaNimHost           = "integrate.api.nvidia.com"
	attrCloudflareAPIHost       = "api.cloudflare.com"
	attrCloudflareAIGatewayHost = "gateway.ai.cloudflare.com"
	attrOpenCodeHost            = "opencode.ai"
	attrVercelGatewayHost       = "ai-gateway.vercel.sh"
)

// matchesAttributionHost reports whether baseURL's hostname equals expectedHost.
// Mirrors pi's matchesHost: parse failures (no URL) are treated as non-matches.
func matchesAttributionHost(baseURL, expectedHost string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return u.Hostname() == expectedHost
}

func isOpenRouterAttributionModel(model *ai.Model) bool {
	// pi uses baseUrl.includes(OPENROUTER_HOST) here (substring, not host match),
	// preserving legacy substring matching (provider-attribution.ts isOpenRouterModel).
	return model.Provider == "openrouter" || strings.Contains(model.BaseURL, attrOpenRouterHost)
}

func isNvidiaNimAttributionModel(model *ai.Model) bool {
	return model.Provider == "nvidia" || matchesAttributionHost(model.BaseURL, attrNvidiaNimHost)
}

func isCloudflareAttributionModel(model *ai.Model) bool {
	return model.Provider == "cloudflare-workers-ai" ||
		model.Provider == "cloudflare-ai-gateway" ||
		matchesAttributionHost(model.BaseURL, attrCloudflareAPIHost) ||
		matchesAttributionHost(model.BaseURL, attrCloudflareAIGatewayHost)
}

func isVercelGatewayAttributionModel(model *ai.Model) bool {
	return model.Provider == "vercel-ai-gateway" || matchesAttributionHost(model.BaseURL, attrVercelGatewayHost)
}

// isInstallTelemetryEnabled mirrors pi telemetry.ts isInstallTelemetryEnabled.
// pi reads PI_TELEMETRY (env overrides settings) and otherwise falls back to
// SettingsManager.getEnableInstallTelemetry(), which defaults to true. The Go
// port has no settings manager, so the faithful default is enabled; the
// PI_TELEMETRY env override is honored so the headers can be suppressed.
func isInstallTelemetryEnabled() bool {
	value, ok := os.LookupEnv("PI_TELEMETRY")
	if !ok {
		// No env override: settings default (getEnableInstallTelemetry() ?? true).
		return true
	}
	return isTruthyEnvFlag(value)
}

// isTruthyEnvFlag mirrors pi telemetry.ts isTruthyEnvFlag.
func isTruthyEnvFlag(value string) bool {
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	return value == "1" || lower == "true" || lower == "yes"
}

// getDefaultAttributionHeaders returns pi's default attribution headers for the
// model's provider/host, or nil. Ported from provider-attribution.ts
// getDefaultAttributionHeaders (gated on install telemetry).
func getDefaultAttributionHeaders(model *ai.Model) map[string]string {
	if !isInstallTelemetryEnabled() {
		return nil
	}
	switch {
	case isOpenRouterAttributionModel(model):
		return map[string]string{
			"HTTP-Referer":            "https://pi.dev",
			"X-OpenRouter-Title":      "pi",
			"X-OpenRouter-Categories": "cli-agent",
		}
	case isNvidiaNimAttributionModel(model):
		return map[string]string{
			"X-BILLING-INVOKE-ORIGIN": "Pi",
		}
	case isCloudflareAttributionModel(model):
		return map[string]string{
			"User-Agent": "pi-coding-agent",
		}
	case isVercelGatewayAttributionModel(model):
		// f8a77f47: Vercel AI Gateway branch.
		return map[string]string{
			"http-referer": "https://pi.dev",
			"x-title":      "pi",
		}
	}
	return nil
}

// getSessionAttributionHeaders returns the OpenCode session headers when a
// session id is present and the model targets OpenCode. Ported from
// provider-attribution.ts getSessionHeaders.
func getSessionAttributionHeaders(model *ai.Model, sessionID string) map[string]string {
	if sessionID == "" {
		return nil
	}
	if model.Provider != "opencode" &&
		model.Provider != "opencode-go" &&
		!matchesAttributionHost(model.BaseURL, attrOpenCodeHost) {
		return nil
	}
	return map[string]string{
		"x-opencode-session": sessionID,
		"x-opencode-client":  "pi",
	}
}

// applyAttributionDefaults sets pi's session + default attribution headers on r
// (via set) at the BOTTOM of the precedence stack. It mirrors the base of pi's
// mergeProviderAttributionHeaders: { ...getSessionHeaders(),
// ...getDefaultAttributionHeaders() } — session-attribution first (lowest), then
// default-attribution.
//
// Callers must invoke this BEFORE setting model.Headers, the provider-specific
// headers, and opts.Headers, so that those higher-precedence sources override
// the attribution defaults (matching pi, where auth.headers and options.headers
// are spread over the attribution base). The consumer opts.Headers (pi's
// options.headers — highest precedence) are applied separately by each caller
// after model.Headers and the provider-specific headers.
func applyAttributionDefaults(set func(k, v string), model *ai.Model, sessionID string) {
	for k, v := range getSessionAttributionHeaders(model, sessionID) {
		set(k, v)
	}
	for k, v := range getDefaultAttributionHeaders(model) {
		set(k, v)
	}
}
