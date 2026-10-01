package ai

import (
	"os"
	"path/filepath"
)

// AnthropicAuthTokenEnv holds a bearer token sent as Authorization: Bearer
// rather than x-api-key. It participates in env discovery/status, but
// GetEnvApiKey skips it because it is not an API key (pi env-api-keys.ts
// ANTHROPIC_AUTH_TOKEN_ENV, upstream 24e5cc04). The anthropic provider reads it
// directly and, when present, authenticates via the Authorization header.
const AnthropicAuthTokenEnv = "ANTHROPIC_AUTH_TOKEN"

// The workload identity federation variables @anthropic-ai/sdk documents (pi
// env-api-keys.ts ANTHROPIC_FEDERATION_RULE_ID_ENV et al., upstream a9424cd43).
// The anthropic provider resolves them last, after every key and
// ANTHROPIC_AUTH_TOKEN, and hands them to the adapter in env: the first three
// are required, the service account and workspace ids ride along when set.
// They are provider config rather than credentials, so they take no part in
// env-key discovery.
const (
	AnthropicFederationRuleIDEnv  = "ANTHROPIC_FEDERATION_RULE_ID"
	AnthropicOrganizationIDEnv    = "ANTHROPIC_ORGANIZATION_ID"
	AnthropicServiceAccountIDEnv  = "ANTHROPIC_SERVICE_ACCOUNT_ID"
	AnthropicIdentityTokenFileEnv = "ANTHROPIC_IDENTITY_TOKEN_FILE"
	AnthropicWorkspaceIDEnv       = "ANTHROPIC_WORKSPACE_ID"
)

// apiKeyEnvVars returns the environment variable names that can provide an API
// key for a provider, in precedence order.
func apiKeyEnvVars(provider string) []string {
	switch provider {
	case "github-copilot":
		return []string{"COPILOT_GITHUB_TOKEN"}
	case "anthropic":
		// ANTHROPIC_AUTH_TOKEN participates in discovery/status but is sent as
		// Authorization: Bearer (see GetEnvApiKey); ANTHROPIC_OAUTH_TOKEN takes
		// precedence over ANTHROPIC_API_KEY.
		return []string{AnthropicAuthTokenEnv, "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	}
	envMap := map[string]string{
		"ant-ling":           "ANT_LING_API_KEY",
		"qwen-token-plan":    "QWEN_TOKEN_PLAN_API_KEY",
		"qwen-token-plan-cn": "QWEN_TOKEN_PLAN_CN_API_KEY",
		// pi c03d78bdc: the Individual plan reuses the base plan's env var
		// (envApiKeyAuth(..., ["QWEN_TOKEN_PLAN_API_KEY"])), not a new one.
		"qwen-token-plan-individual": "QWEN_TOKEN_PLAN_API_KEY",
		"openai":                     "OPENAI_API_KEY",
		"azure-openai-responses":     "AZURE_OPENAI_API_KEY",
		"nvidia":                     "NVIDIA_API_KEY",
		"deepseek":                   "DEEPSEEK_API_KEY",
		"google":                     "GEMINI_API_KEY",
		"google-vertex":              "GOOGLE_CLOUD_API_KEY",
		"groq":                       "GROQ_API_KEY",
		"cerebras":                   "CEREBRAS_API_KEY",
		"xai":                        "XAI_API_KEY",
		"typesafe":                   "TYPESAFE_API_KEY", // pi a328aa89a; the classifier provider itself is queued
		"radius":                     "RADIUS_API_KEY",
		"openrouter":                 "OPENROUTER_API_KEY",
		"vercel-ai-gateway":          "AI_GATEWAY_API_KEY",
		"zai":                        "ZAI_API_KEY",
		"zai-coding-cn":              "ZAI_CODING_CN_API_KEY",
		"mistral":                    "MISTRAL_API_KEY",
		"minimax":                    "MINIMAX_API_KEY",
		"minimax-cn":                 "MINIMAX_CN_API_KEY",
		"moonshotai":                 "MOONSHOT_API_KEY",
		"moonshotai-cn":              "MOONSHOT_API_KEY",
		"huggingface":                "HF_TOKEN",
		"fireworks":                  "FIREWORKS_API_KEY",
		"together":                   "TOGETHER_API_KEY",
		"baseten":                    "BASETEN_API_KEY",
		"opencode":                   "OPENCODE_API_KEY",
		"opencode-go":                "OPENCODE_API_KEY",
		"kimi-coding":                "KIMI_API_KEY",
		"meta":                       "META_API_KEY",
		"cloudflare-workers-ai":      "CLOUDFLARE_API_KEY",
		"cloudflare-ai-gateway":      "CLOUDFLARE_API_KEY",
		"xiaomi":                     "XIAOMI_API_KEY",
		"xiaomi-token-plan-cn":       "XIAOMI_TOKEN_PLAN_CN_API_KEY",
		"xiaomi-token-plan-ams":      "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
		"xiaomi-token-plan-sgp":      "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
	}
	if v, ok := envMap[provider]; ok {
		return []string{v}
	}
	return nil
}

// FindEnvKeys returns the configured environment variable names that provide an
// API key for a provider (excludes ambient credential sources like AWS/ADC).
//
// env carries per-stream scoped overrides that win over the OS environment
// (pi 8eeaa2bc threads ProviderEnv through findEnvKeys); pass nil to read only
// the OS environment.
func FindEnvKeys(provider string, env map[string]string) []string {
	vars := apiKeyEnvVars(provider)
	if vars == nil {
		return nil
	}
	var found []string
	for _, v := range vars {
		if ProviderEnvValue(v, env) != "" {
			found = append(found, v)
		}
	}
	return found
}

// ambientAuthMarker is a sentinel API key signalling "authenticated ambiently"
// (e.g. AWS SDK default credential chain, Vertex ADC) — a provider is usable
// without an explicit key. It must never be sent downstream as a real key; the
// compat dispatch in withEnvAPIKey filters it (pi 850c210b, compat.ts
// AMBIENT_AUTH_MARKER).
const ambientAuthMarker = "<authenticated>"

// GetEnvApiKey returns the API key for a provider from known environment
// variables. It does not return keys for OAuth-only providers.
//
// env carries per-stream scoped overrides that win over the OS environment
// (pi 8eeaa2bc threads ProviderEnv through getEnvApiKey); pass nil to read only
// the OS environment.
func GetEnvApiKey(provider string, env map[string]string) string {
	if keys := FindEnvKeys(provider, env); len(keys) > 0 {
		// ANTHROPIC_AUTH_TOKEN is a bearer token, not an API key: skip it so the
		// key slot falls to ANTHROPIC_OAUTH_TOKEN/ANTHROPIC_API_KEY (pi getEnvApiKey,
		// upstream 24e5cc04). The provider consults the auth token separately.
		apiKeyEnv := keys[0]
		if provider == "anthropic" {
			apiKeyEnv = ""
			for _, k := range keys {
				if k != AnthropicAuthTokenEnv {
					apiKeyEnv = k
					break
				}
			}
		}
		if apiKeyEnv != "" {
			return ProviderEnvValue(apiKeyEnv, env)
		}
	}

	switch provider {
	case "google-vertex":
		if hasVertexADCCredentials(env) &&
			anyEnv(env, "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT") &&
			anyEnv(env, "GOOGLE_CLOUD_LOCATION") {
			return ambientAuthMarker
		}
	case "amazon-bedrock":
		if ProviderEnvValue("AWS_PROFILE", env) != "" ||
			(ProviderEnvValue("AWS_ACCESS_KEY_ID", env) != "" && ProviderEnvValue("AWS_SECRET_ACCESS_KEY", env) != "") ||
			ProviderEnvValue("AWS_BEARER_TOKEN_BEDROCK", env) != "" ||
			ProviderEnvValue("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", env) != "" ||
			ProviderEnvValue("AWS_CONTAINER_CREDENTIALS_FULL_URI", env) != "" ||
			ProviderEnvValue("AWS_WEB_IDENTITY_TOKEN_FILE", env) != "" {
			return ambientAuthMarker
		}
	}
	return ""
}

func anyEnv(env map[string]string, names ...string) bool {
	for _, n := range names {
		if ProviderEnvValue(n, env) != "" {
			return true
		}
	}
	return false
}

func hasVertexADCCredentials(env map[string]string) bool {
	if gac := ProviderEnvValue("GOOGLE_APPLICATION_CREDENTIALS", env); gac != "" {
		_, err := os.Stat(gac)
		return err == nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"))
	return err == nil
}
