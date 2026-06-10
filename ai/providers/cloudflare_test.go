package providers

import (
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestIsCloudflareProvider(t *testing.T) {
	for _, p := range []ai.Provider{"cloudflare-workers-ai", "cloudflare-ai-gateway"} {
		if !isCloudflareProvider(p) {
			t.Fatalf("expected %q to be a cloudflare provider", p)
		}
	}
	for _, p := range []ai.Provider{"openai", "anthropic", "cloudflare", ""} {
		if isCloudflareProvider(p) {
			t.Fatalf("expected %q to NOT be a cloudflare provider", p)
		}
	}
}

func TestResolveCloudflareBaseURLSubstitutesEnv(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct-123")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "gw-456")

	model := &ai.Model{Provider: "cloudflare-ai-gateway", BaseURL: cloudflareAIGatewayCompatBaseURL}
	got, err := resolveCloudflareBaseURL(model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://gateway.ai.cloudflare.com/v1/acct-123/gw-456/compat"
	if got != want {
		t.Fatalf("resolved = %q, want %q", got, want)
	}

	model = &ai.Model{Provider: "cloudflare-workers-ai", BaseURL: cloudflareWorkersAIBaseURL}
	got, err = resolveCloudflareBaseURL(model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want = "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1"
	if got != want {
		t.Fatalf("resolved = %q, want %q", got, want)
	}
}

func TestResolveCloudflareBaseURLMissingEnvErrors(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct-123")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "") // unset/empty

	model := &ai.Model{Provider: "cloudflare-ai-gateway", BaseURL: cloudflareAIGatewayOpenAIBaseURL}
	_, err := resolveCloudflareBaseURL(model)
	if err == nil {
		t.Fatal("expected error for missing CLOUDFLARE_GATEWAY_ID")
	}
	// Exact pi message: `${name} is required for provider ${model.provider} but is not set.`
	want := "CLOUDFLARE_GATEWAY_ID is required for provider cloudflare-ai-gateway but is not set."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestResolveCloudflareBaseURLPassthroughWithoutPlaceholders(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	model := &ai.Model{Provider: "openai", BaseURL: "https://api.openai.com/v1"}
	got, err := resolveCloudflareBaseURL(model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://api.openai.com/v1" {
		t.Fatalf("passthrough mangled URL: %q", got)
	}
}
