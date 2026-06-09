package ai

import "testing"

func TestCatalogLoads(t *testing.T) {
	m := GetModel("anthropic", "claude-3-5-haiku-20241022")
	if m == nil || m.MaxTokens != 8192 {
		t.Fatalf("haiku not loaded: %#v", m)
	}
	if len(GetProviders()) < 10 {
		t.Fatalf("too few providers: %d", len(GetProviders()))
	}
}
