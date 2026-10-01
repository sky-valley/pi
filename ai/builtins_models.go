package ai

import (
	"context"
	"sort"
)

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
		if len(models) == 0 {
			// A catalog key with no chat models serves only non-chat models:
			// typesafe, whose classifiers pi's builtinModels registers through
			// createProvider's "classifiers" option. Classifiers arrive with
			// Scope queue row 3; until then the provider has nothing to serve,
			// and CreateProvider refuses a provider with no implementation, as
			// pi's createProvider does.
			continue
		}
		// Every api the provider's models use gets an entry, so the provider
		// always has the implementation CreateProvider requires. An api with no
		// registered ApiProvider — this host did not import ai/providers, or
		// the adapter is not ported — gets an empty one, which streams the
		// same "has no API implementation" error as a missing entry.
		apiMap := map[Api]ProviderStreams{}
		for _, mod := range models {
			if _, seen := apiMap[mod.Api]; seen {
				continue
			}
			var streams ProviderStreams
			if ap, ok := GetApiProvider(mod.Api); ok {
				streams = ProviderStreams{
					Stream:         ap.Stream,
					StreamSimple:   ap.StreamSimple,
					FetchDeferred:  ap.FetchDeferred,
					CancelDeferred: ap.CancelDeferred,
				}
			}
			apiMap[mod.Api] = streams
		}
		m.SetProvider(CreateProvider(CreateProviderOptions{
			ID:           providerID,
			Auth:         builtinProviderAuth(providerID),
			Models:       models,
			FilterModels: builtinFilterModels(providerID),
			APIByApi:     apiMap,
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
	if providerID == "anthropic" {
		// Anthropic resolves auth in a custom order (upstream 24e5cc04): a stored
		// key wins, then ANTHROPIC_AUTH_TOKEN authenticates via an Authorization
		// header, then the oauth/api-key env vars. The generic EnvAPIKeyAuth would
		// wrongly surface the auth token as an api key (→ x-api-key).
		return ProviderAuth{APIKey: anthropicAPIKeyAuth()}
	}
	if vars := apiKeyEnvVars(providerID); len(vars) > 0 {
		return ProviderAuth{APIKey: EnvAPIKeyAuth(providerID, vars...)}
	}
	return ProviderAuth{APIKey: &ApiKeyAuth{
		Name: providerID,
		Resolve: func(ctx context.Context, _ AuthContext, cred *Credential) (*AuthResult, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if cred != nil && cred.Key != "" {
				// Pass the credential's env section through (upstream 1942b260 —
				// this generic ambient resolver stands in for pi's bedrockAuth et al.,
				// which the fix also touched).
				return &AuthResult{Auth: ModelAuth{APIKey: cred.Key}, Env: cred.Env, Source: "stored credential"}, nil
			}
			key := GetEnvApiKey(providerID, nil)
			if key == "" {
				return nil, nil
			}
			return &AuthResult{Auth: ModelAuth{APIKey: key}}, nil
		},
	}}
}

// anthropicAPIKeyAuth mirrors pi anthropicApiKeyAuth() (upstream 24e5cc04): a
// stored credential key wins; otherwise ANTHROPIC_AUTH_TOKEN authenticates via
// an Authorization: Bearer header (never x-api-key), and only if it is absent do
// ANTHROPIC_OAUTH_TOKEN/ANTHROPIC_API_KEY resolve as api keys; the workload
// identity federation variables (upstream a9424cd43) come last of all. This
// keeps the facade GetAuth/Stream path byte-faithful and preserves pi's
// credential-first precedence, which the generic env-key resolver could not
// express.
func anthropicAPIKeyAuth() *ApiKeyAuth {
	return &ApiKeyAuth{
		Name: "Anthropic API key",
		Login: func(ctx context.Context, interaction AuthInteraction) (*Credential, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			key, err := interaction.Prompt(AuthPrompt{Type: AuthPromptSecret, Message: "Enter Anthropic API key"})
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return &Credential{Type: CredentialAPIKey, Key: key}, nil
		},
		Resolve: func(ctx context.Context, authCtx AuthContext, credential *Credential) (*AuthResult, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if credential != nil && credential.Key != "" {
				return &AuthResult{Auth: ModelAuth{APIKey: credential.Key}, Env: credential.Env, Source: "stored credential"}, nil
			}
			if token := authCtx.Env(AnthropicAuthTokenEnv); token != "" {
				bearer := "Bearer " + token
				return &AuthResult{Auth: ModelAuth{Headers: ProviderHeaders{"Authorization": &bearer}}, Source: AnthropicAuthTokenEnv}, nil
			}
			for _, envVar := range []string{"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
				if value := authCtx.Env(envVar); value != "" {
					return &AuthResult{Auth: ModelAuth{APIKey: value}, Source: envVar}, nil
				}
			}

			// Workload identity federation (upstream a9424cd43): the adapter
			// exchanges the identity token for a short-lived access token, as
			// @anthropic-ai/sdk does for pi, and refreshes it itself. Last in line
			// so keys and ANTHROPIC_AUTH_TOKEN keep winning, as in the SDK. The ids
			// are provider config rather than auth, so they travel in Env, with no
			// request auth at all.
			federation := map[string]string{}
			for _, envVar := range []string{AnthropicFederationRuleIDEnv, AnthropicOrganizationIDEnv, AnthropicIdentityTokenFileEnv} {
				value := authCtx.Env(envVar)
				if value == "" {
					return nil, nil
				}
				federation[envVar] = value
			}
			for _, envVar := range []string{AnthropicServiceAccountIDEnv, AnthropicWorkspaceIDEnv} {
				if value := authCtx.Env(envVar); value != "" {
					federation[envVar] = value
				}
			}
			return &AuthResult{Env: federation, Source: "workload identity federation"}, nil
		},
	}
}

// builtinFilterModels returns the credential-specific availability policy for
// a built-in provider, or nil when it has none. github-copilot restricts its
// catalog to the OAuth credential's availableModelIds (pi
// providers/github-copilot.ts filterModels).
func builtinFilterModels(providerID string) func([]*Model, *Credential) []*Model {
	if providerID != "github-copilot" {
		return nil
	}
	return func(models []*Model, credential *Credential) []*Model {
		if credential == nil || credential.Type != CredentialOAuth || credential.AvailableModelIDs == nil {
			return models
		}
		available := make(map[string]bool, len(credential.AvailableModelIDs))
		for _, id := range credential.AvailableModelIDs {
			available[id] = true
		}
		out := make([]*Model, 0, len(models))
		for _, model := range models {
			if available[model.ID] {
				out = append(out, model)
			}
		}
		return out
	}
}
