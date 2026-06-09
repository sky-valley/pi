package coding

import (
	"fmt"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// DefaultModelSpec is the model used when none is specified.
const DefaultModelSpec = "anthropic/claude-sonnet-4-5"

// ResolveModel resolves a model spec ("provider/id" or bare "id") to a Model
// from the catalog. An empty spec resolves to DefaultModelSpec.
func ResolveModel(spec string) (*ai.Model, error) {
	if spec == "" {
		spec = DefaultModelSpec
	}
	if provider, id, ok := strings.Cut(spec, "/"); ok {
		if m := ai.GetModel(provider, id); m != nil {
			return m, nil
		}
		return nil, fmt.Errorf("model not found: %s/%s", provider, id)
	}
	// Bare id: search across providers.
	for _, provider := range ai.GetProviders() {
		if m := ai.GetModel(provider, spec); m != nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("model not found: %s", spec)
}
