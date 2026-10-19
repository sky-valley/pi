package ai

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// pi 8eeaa2bc threads per-stream scoped ProviderEnv through getEnvApiKey /
// findEnvKeys: a non-empty scoped value wins over the OS environment, an empty
// scoped value falls through, and nil reads only the OS environment.
func TestGetEnvApiKeyScopedEnv(t *testing.T) {
	// Neutralize the ambient OS key so the scoped override is the only source.
	t.Setenv("OPENAI_API_KEY", "")

	if got := GetEnvApiKey("openai", map[string]string{"OPENAI_API_KEY": "scoped"}); got != "scoped" {
		t.Errorf("scoped env should provide the key: got %q, want %q", got, "scoped")
	}

	// Scoped non-empty wins over a set OS value.
	t.Setenv("OPENAI_API_KEY", "os-value")
	if got := GetEnvApiKey("openai", map[string]string{"OPENAI_API_KEY": "scoped"}); got != "scoped" {
		t.Errorf("scoped should win over OS: got %q, want %q", got, "scoped")
	}

	// Empty scoped value falls through to the OS value.
	if got := GetEnvApiKey("openai", map[string]string{"OPENAI_API_KEY": ""}); got != "os-value" {
		t.Errorf("empty scoped should fall through to OS: got %q, want %q", got, "os-value")
	}

	// Nil scoped env reads only the OS environment (prior behavior preserved).
	if got := GetEnvApiKey("openai", nil); got != "os-value" {
		t.Errorf("nil env should read OS: got %q, want %q", got, "os-value")
	}

	// Absent everywhere -> no key.
	t.Setenv("OPENAI_API_KEY", "")
	if got := GetEnvApiKey("openai", nil); got != "" {
		t.Errorf("absent key should be empty: got %q", got)
	}
}

// withEnvAPIKey / withEnvAPIKeySimple must consult the scoped env on the stream
// options when no explicit API key is set, so a host can scope credentials per
// request without touching the process environment (pi 8eeaa2bc).
func TestWithEnvAPIKeyUsesScopedEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	model := &Model{Provider: "openai"}

	got := withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Env: map[string]string{"OPENAI_API_KEY": "scoped"}}})
	if got.APIKey != "scoped" {
		t.Errorf("withEnvAPIKey should pull key from scoped env: got %q", got.APIKey)
	}

	gotSimple := withEnvAPIKeySimple(model, &SimpleStreamOptions{
		StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Env: map[string]string{"OPENAI_API_KEY": "scoped"}}},
	})
	if gotSimple.APIKey != "scoped" {
		t.Errorf("withEnvAPIKeySimple should pull key from scoped env: got %q", gotSimple.APIKey)
	}

	// An explicit API key still wins over the scoped env.
	explicit := withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: "explicit", Env: map[string]string{"OPENAI_API_KEY": "scoped"}}})
	if explicit.APIKey != "explicit" {
		t.Errorf("explicit API key should win: got %q", explicit.APIKey)
	}
}

// pi 24e5cc04: on the compat path, ANTHROPIC_AUTH_TOKEN must NOT resolve into
// APIKey (it is a bearer credential the provider applies as an Authorization
// header). withEnvAPIKey leaves APIKey empty when the token is active, so it
// beats an env ANTHROPIC_API_KEY yet still yields to an explicit request key.
func TestWithEnvAPIKeyLeavesAnthropicAuthTokenForProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	model := &Model{Provider: "anthropic"}

	// Auth token present (even alongside an api key): APIKey stays empty so the
	// provider emits the bearer, matching pi's AUTH_TOKEN-beats-env-api-key order.
	got := withEnvAPIKey(model, &StreamOptions{
		ProviderRequestOptions: ProviderRequestOptions{Env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_API_KEY": "sk-env",
		}},
	})
	if got.APIKey != "" {
		t.Errorf("auth token must not resolve into APIKey: got %q", got.APIKey)
	}

	// No auth token: the env api key resolves normally.
	got = withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Env: map[string]string{"ANTHROPIC_API_KEY": "sk-env"}}})
	if got.APIKey != "sk-env" {
		t.Errorf("env api key should resolve when no auth token: got %q", got.APIKey)
	}

	// An explicit request key wins over the auth token.
	got = withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: "explicit", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}}})
	if got.APIKey != "explicit" {
		t.Errorf("explicit key should win over the auth token: got %q", got.APIKey)
	}
}

// The ambient-auth marker (GetEnvApiKey returns "<authenticated>" when a
// provider is usable via ambient credentials with no explicit key — e.g. AWS
// SDK default chain) must never be injected as a real API key by the compat
// dispatch (pi 850c210b filters AMBIENT_AUTH_MARKER in withEnvApiKey).
func TestWithEnvAPIKeyFiltersAmbientMarker(t *testing.T) {
	model := &Model{Provider: "amazon-bedrock"}
	env := map[string]string{"AWS_PROFILE": "default"}

	// Precondition: this env makes GetEnvApiKey resolve to the ambient marker.
	if key := GetEnvApiKey(model.Provider, env); key != ambientAuthMarker {
		t.Fatalf("precondition: expected ambient marker, got %q", key)
	}

	got := withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Env: env}})
	if got.APIKey != "" {
		t.Errorf("withEnvAPIKey must not inject the ambient marker as a key: got %q", got.APIKey)
	}
	if got.Env["AWS_PROFILE"] != "default" {
		t.Errorf("env must survive the ambient-marker skip: got %v", got.Env)
	}

	gotSimple := withEnvAPIKeySimple(model, &SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Env: env}}})
	if gotSimple.APIKey != "" {
		t.Errorf("withEnvAPIKeySimple must not inject the ambient marker as a key: got %q", gotSimple.APIKey)
	}
}

// pi 2cbce395 merges the resolved provider env into the request options so it
// reaches the API. The Go port has no resolution-time env source (no catalog
// provider resolves one, matching upstream where resolution.env is latent), so
// this is a passthrough: opts.Env must survive auth resolution and reach the
// provider, where it is consulted via ProviderEnvValue (e.g. PI_CACHE_RETENTION,
// Cloudflare base-URL). This locks that the env is not dropped by the key step.
func TestStreamEnvReachesProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	model := &Model{Provider: "openai"}
	env := map[string]string{"PI_CACHE_RETENTION": "long", "OPENAI_API_KEY": "scoped"}

	// Key injected from env: the returned options still carry the full Env.
	got := withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Env: env}})
	if got.Env["PI_CACHE_RETENTION"] != "long" {
		t.Errorf("env must survive key injection: got %v", got.Env)
	}

	// Explicit-key passthrough must also preserve Env unchanged.
	passthrough := withEnvAPIKey(model, &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: "explicit", Env: env}})
	if passthrough.Env["PI_CACHE_RETENTION"] != "long" {
		t.Errorf("env must survive explicit-key passthrough: got %v", passthrough.Env)
	}
}

// TestQwenTokenPlanEnvKeys locks the Qwen Token Plan built-in provider wiring
// (upstream bbb91fa8): both providers map to their API-key env vars and register
// as built-ins from the catalog with env-key auth.
func TestQwenTokenPlanEnvKeys(t *testing.T) {
	want := map[string]string{
		"qwen-token-plan":    "QWEN_TOKEN_PLAN_API_KEY",
		"qwen-token-plan-cn": "QWEN_TOKEN_PLAN_CN_API_KEY",
		// pi c03d78bdc: the Individual plan deliberately REUSES the base
		// plan's env var rather than introducing its own.
		"qwen-token-plan-individual": "QWEN_TOKEN_PLAN_API_KEY",
	}
	m := BuiltinModels()
	for provider, envVar := range want {
		vars := apiKeyEnvVars(provider)
		if len(vars) != 1 || vars[0] != envVar {
			t.Errorf("apiKeyEnvVars(%q) = %v, want [%s]", provider, vars, envVar)
		}
		if prov := m.GetProvider(provider); prov == nil || len(prov.GetModels()) == 0 {
			t.Errorf("%q not registered as a built-in provider with models", provider)
		}
	}
}

// pi c1019d92 adds the Baseten provider: its API key comes from BASETEN_API_KEY.
// The provider itself stays latent here until the catalog carries Baseten models.
func TestBasetenEnvKey(t *testing.T) {
	if vars := apiKeyEnvVars("baseten"); len(vars) != 1 || vars[0] != "BASETEN_API_KEY" {
		t.Fatalf("apiKeyEnvVars(\"baseten\") = %v, want [BASETEN_API_KEY]", vars)
	}
	if got := GetEnvApiKey("baseten", map[string]string{"BASETEN_API_KEY": "scoped"}); got != "scoped" {
		t.Errorf("GetEnvApiKey(baseten) = %q, want %q", got, "scoped")
	}
}

// pi 24e5cc04: ANTHROPIC_AUTH_TOKEN participates in discovery/status but is a
// bearer token, not an API key, so GetEnvApiKey skips it and the key slot falls
// to ANTHROPIC_OAUTH_TOKEN/ANTHROPIC_API_KEY.
func TestAnthropicAuthTokenSkippedByGetEnvApiKey(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	// Discovery lists the auth token first, ahead of oauth and api-key.
	if vars := apiKeyEnvVars("anthropic"); len(vars) != 3 || vars[0] != AnthropicAuthTokenEnv ||
		vars[1] != "ANTHROPIC_OAUTH_TOKEN" || vars[2] != "ANTHROPIC_API_KEY" {
		t.Fatalf("apiKeyEnvVars(anthropic) = %v", vars)
	}

	// Only the auth token set: it is discovered, but the key slot is empty.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok")
	if keys := FindEnvKeys("anthropic", nil); len(keys) != 1 || keys[0] != AnthropicAuthTokenEnv {
		t.Fatalf("FindEnvKeys should discover the auth token: %v", keys)
	}
	if got := GetEnvApiKey("anthropic", nil); got != "" {
		t.Fatalf("GetEnvApiKey must skip the auth token: got %q", got)
	}

	// With an API key also set, the key slot uses it (never the auth token).
	t.Setenv("ANTHROPIC_API_KEY", "sk-real")
	if got := GetEnvApiKey("anthropic", nil); got != "sk-real" {
		t.Fatalf("GetEnvApiKey should return the api key: got %q", got)
	}
}

// pi b73412a37 adds the Meta provider (Muse subscription). Its API key rides
// on META_API_KEY, which is what gives the catalog-derived provider env-key
// auth: builtinProviderAuth wires EnvAPIKeyAuth for any provider this table
// names, so a missing row leaves Meta with the generic ambient resolver and no
// documented env var.
func TestMetaEnvKey(t *testing.T) {
	if vars := apiKeyEnvVars("meta"); len(vars) != 1 || vars[0] != "META_API_KEY" {
		t.Fatalf("apiKeyEnvVars(\"meta\") = %v, want [META_API_KEY]", vars)
	}
	if got := GetEnvApiKey("meta", map[string]string{"META_API_KEY": "scoped"}); got != "scoped" {
		t.Errorf("GetEnvApiKey(meta) = %q, want %q", got, "scoped")
	}
}

// pi a328aa89a adds typesafe: "TYPESAFE_API_KEY" to getApiKeyEnvVars.
func TestTypesafeEnvKey(t *testing.T) {
	if vars := apiKeyEnvVars("typesafe"); len(vars) != 1 || vars[0] != "TYPESAFE_API_KEY" {
		t.Fatalf("apiKeyEnvVars(\"typesafe\") = %v, want [TYPESAFE_API_KEY]", vars)
	}
	if got := GetEnvApiKey("typesafe", map[string]string{"TYPESAFE_API_KEY": "scoped"}); got != "scoped" {
		t.Errorf("GetEnvApiKey(typesafe) = %q, want %q", got, "scoped")
	}
}

// testdata/envkeys/capture.mjs reads pi's API-key env var table through
// findEnvKeys, for every catalog provider and for the ids pi renamed away
// (azure-openai-responses became azure in a37306d43). The port's table must
// list the same variables in the same order, and none for a provider pi has
// none for. A provider the capture lacks means the catalog moved: re-capture.
func TestAPIKeyEnvVarsMatchPi(t *testing.T) {
	const file = "testdata/envkeys/env-keys-1.3.0.json"
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/envkeys/capture.mjs)", file, err)
	}
	var capture struct {
		EnvKeys map[string][]string `json:"envKeys"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if len(capture.EnvKeys) == 0 {
		t.Fatalf("%s holds no providers", file)
	}
	for provider, want := range capture.EnvKeys {
		if got := apiKeyEnvVars(provider); !slices.Equal(got, want) {
			t.Errorf("apiKeyEnvVars(%q) = %q, pi %q", provider, got, want)
		}
	}
	for _, provider := range GetProviders() {
		if _, ok := capture.EnvKeys[provider]; !ok {
			t.Errorf("catalog provider %q is not in %s; re-capture it from the build the catalog came from", provider, file)
		}
	}
}
