package ai

import (
	"sort"
	"testing"
)

// BuiltinModels composes the catalog, ProviderAuth, and the ApiProvider
// registry into the runtime. Streaming dispatch itself is covered by
// TestCreateProviderDispatch + the registry tests; here we lock the catalog
// wiring, the auth-substrate integration, and deterministic order.
func TestBuiltinModelsCatalogAndAuth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin-test")

	m := BuiltinModels()

	p := m.GetProvider("openai")
	if p == nil {
		t.Fatal("openai provider not present in BuiltinModels")
	}
	if len(m.GetModels("openai")) == 0 {
		t.Fatal("openai has no catalog models")
	}

	// The auth substrate resolves the env key for a catalog provider.
	res, err := m.GetAuth(&Model{Provider: "openai", Api: APIOpenAICompletions, ID: "x"})
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
