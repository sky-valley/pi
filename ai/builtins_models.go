package ai

import "sort"

// BuiltinModels constructs a Models collection from the embedded catalog,
// wiring each provider's models, ProviderAuth, and registered ApiProvider
// stream implementations into the runtime Provider/Models object-model (pi
// builtinModels over createModels/createProvider). Stream implementations come
// from the global ApiProvider registry, which the ai/providers package
// populates on import; a host that imports ai/providers gets fully streamable
// providers, otherwise GetAuth/GetModels still work and unwired apis error on
// stream (matching unported apis).
//
// The pre-existing global free functions (Stream/GetModel/GetEnvApiKey, …)
// remain the compat surface — pi's "@earendil-works/pi-ai/compat".
func BuiltinModels() MutableModels {
	LoadBuiltinModels()
	m := CreateModels(nil)

	providerIDs := GetProviders()
	sort.Strings(providerIDs) // deterministic collection order

	for _, providerID := range providerIDs {
		models := GetModels(providerID)
		apiMap := map[Api]ProviderStreams{}
		for _, mod := range models {
			if _, seen := apiMap[mod.Api]; seen {
				continue
			}
			if ap, ok := GetApiProvider(mod.Api); ok {
				apiMap[mod.Api] = ProviderStreams{Stream: ap.Stream, StreamSimple: ap.StreamSimple}
			}
		}
		m.SetProvider(CreateProvider(CreateProviderOptions{
			ID:       providerID,
			Auth:     builtinProviderAuth(providerID),
			Models:   models,
			APIByApi: apiMap,
		}))
	}
	return m
}

// builtinProviderAuth builds the ProviderAuth for a built-in provider. Providers
// with known API-key env vars use the standard EnvAPIKeyAuth; providers
// configured only by ambient credentials (Vertex ADC, Bedrock IAM, …) get a
// resolver that defers to GetEnvApiKey, preserving the exact ambient-detection
// behavior of the compat path.
func builtinProviderAuth(providerID string) ProviderAuth {
	if vars := apiKeyEnvVars(providerID); len(vars) > 0 {
		return ProviderAuth{APIKey: EnvAPIKeyAuth(providerID, vars...)}
	}
	return ProviderAuth{APIKey: &ApiKeyAuth{
		Name: providerID,
		Resolve: func(_ *Model, _ AuthContext, cred *Credential) (*AuthResult, error) {
			if cred != nil && cred.Key != "" {
				return &AuthResult{Auth: ModelAuth{APIKey: cred.Key}, Source: "stored credential"}, nil
			}
			key := GetEnvApiKey(providerID, nil)
			if key == "" {
				return nil, nil
			}
			return &AuthResult{Auth: ModelAuth{APIKey: key}}, nil
		},
	}}
}
