package ai

import (
	"errors"
	"fmt"
)

// ModelsErrorCode classifies a ModelsError (pi packages/ai/src/auth/resolve.ts).
type ModelsErrorCode string

const (
	ErrModelSource     ModelsErrorCode = "model_source"
	ErrModelValidation ModelsErrorCode = "model_validation"
	ErrProvider        ModelsErrorCode = "provider"
	ErrStream          ModelsErrorCode = "stream"
	ErrAuth            ModelsErrorCode = "auth"
	ErrOAuth           ModelsErrorCode = "oauth"
)

// ModelsError is a coded error from model/auth resolution. Cause is unwrappable.
type ModelsError struct {
	Code    ModelsErrorCode
	Message string
	Cause   error
}

func (e *ModelsError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *ModelsError) Unwrap() error { return e.Cause }

// newModelsError builds a coded resolution error.
func newModelsError(code ModelsErrorCode, message string, cause error) *ModelsError {
	return &ModelsError{Code: code, Message: message, Cause: cause}
}

// resolveProviderAuth is the auth resolution shared by the Models and
// ImagesModels collections (pi resolve.ts resolveProviderAuth). A stored
// credential owns the provider: ambient/env is consulted only when nothing is
// stored. No silent env fallback after a failed refresh or for a credential
// type without a matching handler. Returns (nil, nil) when unconfigured.
func resolveProviderAuth(
	providerID string,
	auth ProviderAuth,
	model *Model,
	credentials CredentialStore,
	ctx AuthContext,
) (*AuthResult, error) {
	stored, err := readCredential(credentials, providerID)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if stored.Type == CredentialOAuth && auth.OAuth != nil {
			return resolveStoredOAuth(credentials, providerID, auth.OAuth, stored)
		}
		if stored.Type == CredentialAPIKey && auth.APIKey != nil {
			return resolveApiKey(ctx, auth.APIKey, model, stored)
		}
		return nil, nil
	}

	// Ambient (env vars, AWS profiles, ADC files).
	if auth.APIKey != nil {
		return resolveApiKey(ctx, auth.APIKey, model, nil)
	}
	return nil, nil
}

// resolveStoredOAuth resolves OAuth with double-checked locking: valid tokens
// cost zero locks; expired tokens lock, re-check expiry under the lock, refresh
// once globally, and persist the rotated credential before release.
func resolveStoredOAuth(
	credentials CredentialStore,
	providerID string,
	oauth *OAuthAuth,
	stored *Credential,
) (*AuthResult, error) {
	credential := stored.OAuthCredentials()

	if nowMillis() >= credential.Expires {
		// Optimistic check said expired; the authoritative check runs under the lock.
		post, err := credentials.Modify(providerID, func(current *Credential) (*Credential, error) {
			if current == nil || current.Type != CredentialOAuth {
				return nil, nil // logged out meanwhile
			}
			if nowMillis() < current.Expires {
				return nil, nil // another request refreshed
			}
			refreshed, rerr := oauth.Refresh(current.OAuthCredentials())
			if rerr != nil {
				return nil, newModelsError(ErrOAuth, "OAuth refresh failed for "+providerID, rerr)
			}
			return oauthCredential(refreshed), nil
		})
		if err != nil {
			var me *ModelsError
			if errors.As(err, &me) {
				return nil, err
			}
			return nil, newModelsError(ErrAuth, "Credential store modify failed for "+providerID, err)
		}
		if post == nil || post.Type != CredentialOAuth {
			return nil, nil // logged out meanwhile
		}
		credential = post.OAuthCredentials()
	}

	auth, err := oauth.ToAuth(credential)
	if err != nil {
		return nil, newModelsError(ErrOAuth, "OAuth auth derivation failed for "+providerID, err)
	}
	return &AuthResult{Auth: auth, Source: "OAuth"}, nil
}

// resolveApiKey runs a provider's api-key resolver and wraps failures.
func resolveApiKey(
	ctx AuthContext,
	apiKey *ApiKeyAuth,
	model *Model,
	credential *Credential,
) (*AuthResult, error) {
	res, err := apiKey.Resolve(model, ctx, credential)
	if err != nil {
		return nil, newModelsError(ErrAuth, "API key auth failed for provider "+string(model.Provider), err)
	}
	return res, nil
}

// readCredential reads from the store and wraps failures.
func readCredential(credentials CredentialStore, providerID string) (*Credential, error) {
	c, err := credentials.Read(providerID)
	if err != nil {
		return nil, newModelsError(ErrAuth, "Credential store read failed for "+providerID, err)
	}
	return c, nil
}
