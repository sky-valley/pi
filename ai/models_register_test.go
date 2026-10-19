package ai

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// runRegistryChild runs the calling test again in a fresh process, whose
// model registry nothing has read yet: the catalog loads lazily, once.
func runRegistryChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv("PI_REGISTRY_CHILD") == t.Name() {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "PI_REGISTRY_CHILD="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("child process ran no test:\n%s", out)
	}
	return false
}

// TestRegisterModelBeforeTheCatalogLoads: a model a host registers before
// anything has read the registry is the one the registry then holds — the
// catalog, loading afterwards, must not overwrite it — and keeps the
// catalog's slot for its id, as an assignment to an existing key of pi's
// MODELS object does.
func TestRegisterModelBeforeTheCatalogLoads(t *testing.T) {
	if !runRegistryChild(t) {
		return
	}
	RegisterModel(&Model{Provider: "anthropic", ID: "claude-sonnet-4-5", Api: APIAnthropicMessages, BaseURL: "https://proxy.example"})
	if got := GetModel("anthropic", "claude-sonnet-4-5"); got == nil || got.BaseURL != "https://proxy.example" {
		t.Errorf("registered model read back as %+v, want BaseURL https://proxy.example", got)
	}
	var ids []string
	for _, m := range GetModels("anthropic") {
		ids = append(ids, m.ID)
	}
	if got, want := slices.Index(ids, "claude-sonnet-4-5"), catalogIndex(t, "anthropic", "claude-sonnet-4-5"); got != want {
		t.Errorf("claude-sonnet-4-5 is model %d of anthropic, the catalog's slot is %d", got, want)
	}
}

// catalogIndex is id's position among provider's models in the embedded
// catalog.
func catalogIndex(t *testing.T, provider, id string) int {
	t.Helper()
	var ids []string
	if err := decodeCatalog(modelsCatalogJSON, func(string) {}, func(p, i string, _ *Model) {
		if p == provider {
			ids = append(ids, i)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return slices.Index(ids, id)
}

// TestRegisterModelOfANewProviderComesAfterTheCatalog: a provider a host
// registers before anything has read the registry is listed after the
// catalog's providers, as a key added to pi's MODELS object would be.
func TestRegisterModelOfANewProviderComesAfterTheCatalog(t *testing.T) {
	if !runRegistryChild(t) {
		return
	}
	RegisterModel(&Model{Provider: "host-provider", ID: "host-model", Api: APIOpenAICompletions})
	providers := GetProviders()
	if len(providers) == 0 || providers[len(providers)-1] != "host-provider" {
		head := providers
		if len(head) > 3 {
			head = head[:3]
		}
		t.Errorf("providers start %v and end %q, want the catalog's and then host-provider", head, providers[len(providers)-1])
	}
}
