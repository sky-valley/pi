package ai

import "testing"

// TestGetApiProvidersEnumerates ports api-registry.ts:84-86 getApiProviders:
// the registry must be enumerable. pi returns Map insertion order; the Go port
// sorts by Api for determinism.
func TestGetApiProvidersEnumerates(t *testing.T) {
	defer UnregisterApiProviders("registry-test")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-b"}, "registry-test")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-a"}, "registry-test")

	got := GetApiProviders()
	idxA, idxB := -1, -1
	for i, p := range got {
		switch p.Api {
		case "zz-test-api-a":
			idxA = i
		case "zz-test-api-b":
			idxB = i
		}
	}
	if idxA == -1 || idxB == -1 {
		t.Fatalf("registered providers missing from GetApiProviders(): %v", got)
	}
	if idxA > idxB {
		t.Fatalf("providers not sorted by Api: a@%d b@%d", idxA, idxB)
	}

	UnregisterApiProviders("registry-test")
	for _, p := range GetApiProviders() {
		if p.Api == "zz-test-api-a" || p.Api == "zz-test-api-b" {
			t.Fatalf("provider %s survived unregistration", p.Api)
		}
	}
}
