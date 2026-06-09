package ai

import (
	"os"
	"path/filepath"
)

// apiKeyEnvVars returns the environment variable names that can provide an API
// key for a provider, in precedence order.
func apiKeyEnvVars(provider string) []string {
	switch provider {
	case "github-copilot":
		return []string{"COPILOT_GITHUB_TOKEN"}
	case "anthropic":
		// ANTHROPIC_OAUTH_TOKEN takes precedence over ANTHROPIC_API_KEY.
		return []string{"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	}
	envMap := map[string]string{
		"ant-ling":               "ANT_LING_API_KEY",
		"openai":                 "OPENAI_API_KEY",
		"azure-openai-responses": "AZURE_OPENAI_API_KEY",
		"nvidia":                 "NVIDIA_API_KEY",
		"deepseek":               "DEEPSEEK_API_KEY",
		"google":                 "GEMINI_API_KEY",
		"google-vertex":          "GOOGLE_CLOUD_API_KEY",
		"groq":                   "GROQ_API_KEY",
		"cerebras":               "CEREBRAS_API_KEY",
		"xai":                    "XAI_API_KEY",
		"openrouter":             "OPENROUTER_API_KEY",
		"vercel-ai-gateway":      "AI_GATEWAY_API_KEY",
		"zai":                    "ZAI_API_KEY",
		"zai-coding-cn":          "ZAI_CODING_CN_API_KEY",
		"mistral":                "MISTRAL_API_KEY",
		"minimax":                "MINIMAX_API_KEY",
		"minimax-cn":             "MINIMAX_CN_API_KEY",
		"moonshotai":             "MOONSHOT_API_KEY",
		"moonshotai-cn":          "MOONSHOT_API_KEY",
		"huggingface":            "HF_TOKEN",
		"fireworks":              "FIREWORKS_API_KEY",
		"together":               "TOGETHER_API_KEY",
		"opencode":               "OPENCODE_API_KEY",
		"opencode-go":            "OPENCODE_API_KEY",
		"kimi-coding":            "KIMI_API_KEY",
		"cloudflare-workers-ai":  "CLOUDFLARE_API_KEY",
		"cloudflare-ai-gateway":  "CLOUDFLARE_API_KEY",
		"xiaomi":                 "XIAOMI_API_KEY",
		"xiaomi-token-plan-cn":   "XIAOMI_TOKEN_PLAN_CN_API_KEY",
		"xiaomi-token-plan-ams":  "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
		"xiaomi-token-plan-sgp":  "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
	}
	if v, ok := envMap[provider]; ok {
		return []string{v}
	}
	return nil
}

// FindEnvKeys returns the configured environment variable names that provide an
// API key for a provider (excludes ambient credential sources like AWS/ADC).
func FindEnvKeys(provider string) []string {
	vars := apiKeyEnvVars(provider)
	if vars == nil {
		return nil
	}
	var found []string
	for _, v := range vars {
		if os.Getenv(v) != "" {
			found = append(found, v)
		}
	}
	return found
}

// GetEnvApiKey returns the API key for a provider from known environment
// variables. It does not return keys for OAuth-only providers.
func GetEnvApiKey(provider string) string {
	if keys := FindEnvKeys(provider); len(keys) > 0 {
		return os.Getenv(keys[0])
	}

	switch provider {
	case "google-vertex":
		if hasVertexADCCredentials() &&
			anyEnv("GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT") &&
			anyEnv("GOOGLE_CLOUD_LOCATION") {
			return "<authenticated>"
		}
	case "amazon-bedrock":
		if os.Getenv("AWS_PROFILE") != "" ||
			(os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != "") ||
			os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "" ||
			os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" ||
			os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" ||
			os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" {
			return "<authenticated>"
		}
	}
	return ""
}

func anyEnv(names ...string) bool {
	for _, n := range names {
		if os.Getenv(n) != "" {
			return true
		}
	}
	return false
}

func hasVertexADCCredentials() bool {
	if gac := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); gac != "" {
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
