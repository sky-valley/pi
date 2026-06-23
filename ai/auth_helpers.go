package ai

import "sync"

// EnvAPIKeyAuth builds the standard api-key auth (pi
// packages/ai/src/auth/helpers.ts envApiKeyAuth): a stored credential key wins,
// otherwise the first set env var resolves. Includes a Login that prompts for
// the key (acquisition, out of scope but ported for parity). Providers with
// non-standard resolution (metadata, ambient files, IAM) write their own
// ApiKeyAuth.
func EnvAPIKeyAuth(name string, envVars ...string) *ApiKeyAuth {
	return &ApiKeyAuth{
		Name: name,
		Login: func(callbacks AuthLoginCallbacks) (*Credential, error) {
			key, err := callbacks.Prompt(AuthPrompt{Type: AuthPromptSecret, Message: "Enter " + name})
			if err != nil {
				return nil, err
			}
			return &Credential{Type: CredentialAPIKey, Key: key}, nil
		},
		Resolve: func(_ *Model, ctx AuthContext, credential *Credential) (*AuthResult, error) {
			if credential != nil && credential.Key != "" {
				return &AuthResult{Auth: ModelAuth{APIKey: credential.Key}, Source: "stored credential"}, nil
			}
			for _, envVar := range envVars {
				if value := ctx.Env(envVar); value != "" {
					return &AuthResult{Auth: ModelAuth{APIKey: value}, Source: envVar}, nil
				}
			}
			return nil, nil
		},
	}
}

// LazyOAuth wraps a lazily-loaded OAuthAuth so provider definitions can
// advertise OAuth without constructing the implementation up front (pi
// helpers.ts lazyOAuth). The implementation loads once on first
// Login/Refresh/ToAuth; pi memoizes the load promise, Go uses sync.Once.
func LazyOAuth(name string, load func() (*OAuthAuth, error)) *OAuthAuth {
	var (
		once    sync.Once
		loaded  *OAuthAuth
		loadErr error
	)
	get := func() (*OAuthAuth, error) {
		once.Do(func() { loaded, loadErr = load() })
		return loaded, loadErr
	}
	return &OAuthAuth{
		Name: name,
		Login: func(callbacks AuthLoginCallbacks) (*Credential, error) {
			o, err := get()
			if err != nil {
				return nil, err
			}
			return o.Login(callbacks)
		},
		Refresh: func(credential OAuthCredentials) (OAuthCredentials, error) {
			o, err := get()
			if err != nil {
				return OAuthCredentials{}, err
			}
			return o.Refresh(credential)
		},
		ToAuth: func(credential OAuthCredentials) (ModelAuth, error) {
			o, err := get()
			if err != nil {
				return ModelAuth{}, err
			}
			return o.ToAuth(credential)
		},
	}
}
