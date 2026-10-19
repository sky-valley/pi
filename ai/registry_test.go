package ai

import (
	"slices"
	"strings"
	"testing"
)

// TestGetApiProvidersEnumerates ports api-registry.ts:84-86 getApiProviders:
// the registry must be enumerable, in its Map's insertion order — a provider
// registered again keeps its place, and one unregistered and registered again
// comes last.
func TestGetApiProvidersEnumerates(t *testing.T) {
	defer UnregisterApiProviders("registry-test")
	defer UnregisterApiProviders("registry-test-c")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-b"}, "registry-test")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-c"}, "registry-test-c")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-a"}, "registry-test")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-b"}, "registry-test")

	order := func() []Api {
		var apis []Api
		for _, p := range GetApiProviders() {
			if strings.HasPrefix(string(p.Api), "zz-test-api-") {
				apis = append(apis, p.Api)
			}
		}
		return apis
	}
	if got, want := order(), []Api{"zz-test-api-b", "zz-test-api-c", "zz-test-api-a"}; !slices.Equal(got, want) {
		t.Fatalf("GetApiProviders() order %v, pi's Map order %v", got, want)
	}
	UnregisterApiProviders("registry-test-c")
	RegisterApiProvider(ApiProvider{Api: "zz-test-api-c"}, "registry-test-c")
	if got, want := order(), []Api{"zz-test-api-b", "zz-test-api-a", "zz-test-api-c"}; !slices.Equal(got, want) {
		t.Fatalf("after re-adding c: order %v, pi's Map order %v", got, want)
	}

	UnregisterApiProviders("registry-test")
	for _, p := range GetApiProviders() {
		if p.Api == "zz-test-api-a" || p.Api == "zz-test-api-b" {
			t.Fatalf("provider %s survived unregistration", p.Api)
		}
	}
}
