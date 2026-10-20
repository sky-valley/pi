package ai

import (
	"context"
	"slices"
	"sort"
	"testing"
)

// BuiltinModels composes the catalog, ProviderAuth, and the ApiProvider
// registry into the runtime. Streaming dispatch itself is covered by
// TestCreateProviderDispatch + the registry tests; here we lock the catalog
// wiring, the auth-substrate integration, and deterministic order.
func TestBuiltinModelsCatalogAndAuth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin-test")

	m := BuiltinModels(nil)

	p := m.GetProvider("openai")
	if p == nil {
		t.Fatal("openai provider not present in BuiltinModels")
	}
	if len(m.GetModels("openai")) == 0 {
		t.Fatal("openai has no catalog models")
	}

	// The auth substrate resolves the env key for a catalog provider.
	res, err := m.GetAuth(context.Background(), &Model{Provider: "openai", Api: APIOpenAICompletions, ID: "x"}, nil)
	if err != nil {
		t.Fatalf("GetAuth error: %v", err)
	}
	if res == nil || res.Auth.APIKey != "sk-builtin-test" || res.Source != "OPENAI_API_KEY" {
		t.Fatalf("openai auth resolution wrong: %+v", res)
	}

	// Collection order is deterministic (sorted provider ids).
	ids := make([]string, 0)
	for _, pr := range m.GetProviders() {
		ids = append(ids, pr.ID())
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("provider order not sorted: %v", ids)
	}
}

// TestBuiltinGithubCopilotFilterModels locks pi providers/github-copilot.ts
// filterModels: an OAuth credential carrying availableModelIds restricts the
// catalog; a credential without the field leaves it untouched.
func TestBuiltinGithubCopilotFilterModels(t *testing.T) {
	m := BuiltinModels(nil)
	p := m.GetProvider("github-copilot")
	if p == nil {
		t.Fatal("github-copilot provider not present")
	}
	models := p.GetModels()
	if len(models) < 2 {
		t.Fatalf("need at least two copilot models, got %d", len(models))
	}
	keptID := models[0].ID

	restricted := p.FilterModels(models, &Credential{
		Type:              CredentialOAuth,
		AvailableModelIDs: []string{keptID},
	})
	if len(restricted) != 1 || restricted[0].ID != keptID {
		t.Fatalf("filter must restrict to availableModelIds: %v", restricted)
	}

	// No availableModelIds -> unfiltered (pi: non-array -> return models).
	if got := p.FilterModels(models, &Credential{Type: CredentialOAuth}); len(got) != len(models) {
		t.Fatalf("credential without ids must not filter: %d vs %d", len(got), len(models))
	}
	// Non-OAuth credential -> unfiltered.
	if got := p.FilterModels(models, &Credential{Type: CredentialAPIKey, AvailableModelIDs: []string{keptID}}); len(got) != len(models) {
		t.Fatalf("api-key credential must not filter: %d vs %d", len(got), len(models))
	}
	// Empty (non-nil) list filters everything (pi: empty set).
	if got := p.FilterModels(models, &Credential{Type: CredentialOAuth, AvailableModelIDs: []string{}}); len(got) != 0 {
		t.Fatalf("empty availableModelIds must filter all models, got %d", len(got))
	}
	// A provider without a filter policy is the identity.
	if other := m.GetProvider("openai"); other != nil {
		otherModels := other.GetModels()
		if got := other.FilterModels(otherModels, &Credential{Type: CredentialOAuth, AvailableModelIDs: []string{}}); len(got) != len(otherModels) {
			t.Fatal("providers without a policy must not filter")
		}
	}
}

// TestBuiltinAmbientProviderEnvPassthrough locks that the generic ambient-provider
// resolver passes a stored credential's env section through (upstream 1942b260 —
// the amazon-bedrock half of the "env section ignored" fix; amazon-bedrock is not
// in apiKeyEnvVars, so it routes to this resolver).
func TestBuiltinAmbientProviderEnvPassthrough(t *testing.T) {
	pa := builtinProviderAuth("amazon-bedrock")
	if pa.APIKey == nil {
		t.Fatal("expected an API-key auth for amazon-bedrock")
	}
	res, err := pa.APIKey.Resolve(context.Background(), fakeAuthContext{}, &Credential{Type: CredentialAPIKey, Key: "stored", Env: map[string]string{"AWS_PROFILE": "prod"}})
	if err != nil || res == nil || res.Auth.APIKey != "stored" {
		t.Fatalf("resolve wrong: %+v (err %v)", res, err)
	}
	if res.Env["AWS_PROFILE"] != "prod" {
		t.Fatalf("stored credential env section dropped: %+v", res.Env)
	}
}

// A regen tripwire, not an endorsement. BuiltinModels cannot build a catalog
// provider that has no chat models: its models are classifiers or images,
// which wait on Scope queue row 3, while pi's builtinModels registers it. At
// 0.99.1 that is typesafe alone. A regen that adds another such provider
// widens the gap, and must be recorded on row 3 before this list moves.
func TestCatalogProvidersWithoutChatModels(t *testing.T) {
	var chatless []string
	for _, p := range GetProviders() {
		if len(GetModels(p)) == 0 {
			chatless = append(chatless, p)
		}
	}
	sort.Strings(chatless)
	if want := []string{"typesafe"}; !slices.Equal(chatless, want) {
		t.Fatalf("catalog providers with no chat models = %v, want %v; record the change on Scope queue row 3", chatless, want)
	}
}
