package ai

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sky-valley/pi/internal/jstext"
)

// ModelsErrorCode classifies a ModelsError (pi utils/models-error.ts, which
// auth/resolve.ts re-exports since upstream a328aa89a).
type ModelsErrorCode string

const (
	ErrModelSource     ModelsErrorCode = "model_source"
	ErrModelValidation ModelsErrorCode = "model_validation"
	ErrProvider        ModelsErrorCode = "provider"
	ErrStream          ModelsErrorCode = "stream"
	ErrAuth            ModelsErrorCode = "auth"
	ErrOAuth           ModelsErrorCode = "oauth"
)

// ModelsError is a coded error from model/auth resolution (pi ModelsError).
// Cause is unwrappable.
type ModelsError struct {
	Code    ModelsErrorCode
	Message string
	Cause   error
}

// Error is pi's ModelsError.message, which is all pi's callers surface — an
// error stream's errorMessage, a rejected cancelDeferred. The code is not part
// of it; callers that need the code read the Code field.
//
// A cause is kept in the text the way pi's withCauseDetail keeps "the
// underlying reason in it": the cause's text, trimmed as JavaScript trims
// (jstext.Trim, not strings.TrimSpace — see docs/UPSTREAM.md D16), is appended
// after ": " unless it is empty or the message already contains it.
//
// One case Go cannot see: pi formats an Error cause as `message || name`, so a
// cause whose message is empty still contributes its name (": Error"). A Go
// error has no name, so a cause whose text is empty contributes nothing.
func (e *ModelsError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	detail := jstext.Trim(e.Cause.Error())
	if detail == "" || strings.Contains(e.Message, detail) {
		return e.Message
	}
	return e.Message + ": " + detail
}

func (e *ModelsError) Unwrap() error { return e.Cause }

// newModelsError builds a coded resolution error.
func newModelsError(code ModelsErrorCode, message string, cause error) *ModelsError {
	return &ModelsError{Code: code, Message: message, Cause: cause}
}

// isCancellation reports whether err is the caller's own cancellation rather
// than a genuine failure. pi's raceWithAbortSignal rejects with the abort reason
// before a wrapped downstream rejection can surface, so an aborted caller never
// sees a ModelsError. The public entry points run the same race (raceContext in
// models_runtime.go); this keeps the unraced inner paths consistent with it by
// unwrapping cancellation instead of dressing it up as an auth failure.
func isCancellation(ctx context.Context, err error) bool {
	cause := ctx.Err()
	return cause != nil && errors.Is(err, cause)
}

// AuthResolutionOverrides carry request-scoped apiKey/env into resolution so a
// provider's resolve() sees them (pi resolve.ts AuthResolutionOverrides,
// upstream ef231c49). An explicit apiKey resolves directly against a synthetic
// stored credential; env overlays the AuthContext and merges into a stored
// credential's env.
type AuthResolutionOverrides struct {
	APIKey string
	Env    map[string]string
	// MinOAuthValidityMs requires this much remaining OAuth-token validity;
	// defaults to five minutes. Nil means unset (pi: undefined). The effective
	// window is max(this, five minutes), so a smaller value does not relax when
	// a refresh triggers. Setting it at all — unlike leaving it nil — also arms a
	// contract: resolution fails with ErrOAuth if the refreshed token still
	// expires inside that window.
	MinOAuthValidityMs *int64
}

// Cancellation note: pi fed6009c added a `signal` field to
// AuthResolutionOverrides so the optional public signal could be normalized and
// threaded down. Go carries cancellation on the context.Context that every
// resolution entry point already takes, so no override field is needed — the
// caller's context is the signal.

// resolveProviderAuth is the auth resolution shared by all operations in a
// Models collection (pi resolve.ts resolveProviderAuth). A stored
// credential owns the provider: ambient/env is consulted only when nothing is
// stored. No silent env fallback after a failed refresh or for a credential
// type without a matching handler. Returns (nil, nil) when unconfigured.
//
// It returns as soon as ctx is done, even while a resolver that ignores ctx
// is still running (pi's raceWithAbortSignal around the resolution).
func resolveProviderAuth(
	ctx context.Context,
	providerID string,
	auth ProviderAuth,
	credentials CredentialStore,
	authCtx AuthContext,
	overrides *AuthResolutionOverrides,
) (*AuthResult, error) {
	return raceContext(ctx, func() (*AuthResult, error) {
		return resolveProviderAuthWithContext(ctx, providerID, auth, credentials, authCtx, overrides)
	})
}

func resolveProviderAuthWithContext(
	ctx context.Context,
	providerID string,
	auth ProviderAuth,
	credentials CredentialStore,
	authCtx AuthContext,
	overrides *AuthResolutionOverrides,
) (*AuthResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Request-scoped env is visible to the provider's resolve() (ef231c49).
	requestCtx := authCtx
	if overrides != nil && len(overrides.Env) > 0 {
		requestCtx = overlayEnvAuthContext(authCtx, overrides.Env)
	}

	// An explicit request apiKey resolves directly, ahead of any stored
	// credential (pi: overrides?.apiKey !== undefined).
	if overrides != nil && overrides.APIKey != "" && auth.APIKey != nil {
		return resolveApiKey(ctx, requestCtx, auth.APIKey, providerID, &Credential{
			Type: CredentialAPIKey,
			Key:  overrides.APIKey,
			Env:  overrides.Env,
		})
	}

	stored, err := readCredential(ctx, credentials, providerID)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if stored.Type == CredentialOAuth && auth.OAuth != nil {
			var minValidityMs *int64
			if overrides != nil {
				minValidityMs = overrides.MinOAuthValidityMs
			}
			return resolveStoredOAuth(ctx, credentials, providerID, auth.OAuth, stored, minValidityMs)
		}
		if stored.Type == CredentialAPIKey && auth.APIKey != nil {
			credential := stored
			if overrides != nil && len(overrides.Env) > 0 {
				clone := *stored
				clone.Env = mergeStringMap(stored.Env, overrides.Env) // overrides win
				credential = &clone
			}
			return resolveApiKey(ctx, requestCtx, auth.APIKey, providerID, credential)
		}
		return nil, nil
	}

	// Ambient (env vars, AWS profiles, ADC files).
	if auth.APIKey != nil {
		return resolveApiKey(ctx, requestCtx, auth.APIKey, providerID, nil)
	}
	return nil, nil
}

// overlayAuthContext makes overrides.env take precedence over the base
// AuthContext (pi: env[name] || base.env(name); empty falls through).
type overlayAuthContext struct {
	base AuthContext
	env  map[string]string
}

func (o overlayAuthContext) Env(name string) string {
	if v := o.env[name]; v != "" {
		return v
	}
	return o.base.Env(name)
}

func (o overlayAuthContext) FileExists(path string) bool { return o.base.FileExists(path) }

func overlayEnvAuthContext(base AuthContext, env map[string]string) AuthContext {
	return overlayAuthContext{base: base, env: env}
}

// defaultOAuthMinimumValidityMs is the default floor for the remaining validity
// an OAuth token must have to be used without a refresh.
const defaultOAuthMinimumValidityMs int64 = 5 * 60 * 1000

// defaultOAuthRefreshTimeout bounds a single token refresh (pi acbdc0d25
// DEFAULT_OAUTH_REFRESH_TIMEOUT_MS). It holds the credential-store lock, so a
// provider that never answers would wedge every other resolution behind it.
const defaultOAuthRefreshTimeout = 15 * time.Second

// refreshStoredOAuthCredential refreshes a stored OAuth credential under the
// credential-store lock and persists the result before the lock is released.
// needsRefresh is re-checked under the lock, so concurrent callers and
// processes refresh only once.
//
// ctx cancels only the wait for the lock. Once a refresh starts, the provider
// may already have rotated the refresh token, so the refresh and its
// persistence run detached from ctx's cancellation (keeping its values) and
// are bounded only by defaultOAuthRefreshTimeout. Otherwise a cancelled caller
// could discard the only valid refresh token. Callers that must return
// promptly on cancellation race this call with ctx.
//
// It returns the stored OAuth credential after the operation, or nil when the
// provider no longer has an OAuth credential.
func refreshStoredOAuthCredential(
	ctx context.Context,
	credentials CredentialStore,
	providerID string,
	oauth *OAuthAuth,
	needsRefresh func(*Credential) bool,
) (*Credential, error) {
	lockWait, cancelLockWait := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelLockWait(nil)
	stopLockWaitCancel := context.AfterFunc(ctx, func() { cancelLockWait(context.Cause(ctx)) })
	defer stopLockWaitCancel()
	if ctx.Err() != nil {
		cancelLockWait(context.Cause(ctx))
	}

	post, err := credentials.Modify(lockWait, providerID, func(current *Credential) (*Credential, error) {
		stopLockWaitCancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if current == nil || current.Type != CredentialOAuth {
			return nil, nil // logged out meanwhile
		}
		if !needsRefresh(current) {
			return nil, nil // another process/request refreshed
		}
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultOAuthRefreshTimeout)
		defer cancel()
		refreshed, err := oauth.Refresh(refreshCtx, current.OAuthCredentials())
		if err != nil {
			return nil, newModelsError(ErrOAuth, "OAuth refresh failed for "+providerID, err)
		}
		return oauthCredential(refreshed), nil
	})
	if err != nil {
		var me *ModelsError
		if errors.As(err, &me) {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, newModelsError(ErrAuth, "Credential store modify failed for "+providerID, err)
	}
	if post == nil || post.Type != CredentialOAuth {
		return nil, nil
	}
	return post, nil
}

// resolveStoredOAuth resolves OAuth with double-checked locking: tokens with
// less than five minutes remaining are refreshed through
// refreshStoredOAuthCredential. minOAuthValidityMs widens that window (never
// shrinks it) and, when non-nil, additionally requires the refreshed token to
// clear it.
func resolveStoredOAuth(
	ctx context.Context,
	credentials CredentialStore,
	providerID string,
	oauth *OAuthAuth,
	stored *Credential,
	minOAuthValidityMs *int64,
) (*AuthResult, error) {
	minimumValidityMs := defaultOAuthMinimumValidityMs
	if minOAuthValidityMs != nil && *minOAuthValidityMs > minimumValidityMs {
		minimumValidityMs = *minOAuthValidityMs
	}
	expiresSoon := func(credential *Credential) bool { return nowMillis()+minimumValidityMs >= credential.Expires }
	credential := stored.OAuthCredentials()

	if expiresSoon(stored) {
		// Optimistic check said expired; the authoritative check runs under the lock.
		post, err := refreshStoredOAuthCredential(ctx, credentials, providerID, oauth, expiresSoon)
		if err != nil {
			return nil, err
		}
		if post == nil {
			return nil, nil // logged out meanwhile
		}
		credential = post.OAuthCredentials()
		// The normal five-minute window triggers a refresh but does not impose a
		// provider contract. Callers that set the minimum explicitly do require it
		// after the refresh. pi's such caller is its bearer-token export CLI, which
		// is host-side and unported; here the branch is reachable only through the
		// exported overrides.
		if minOAuthValidityMs != nil && expiresSoon(post) {
			return nil, newModelsError(ErrOAuth,
				"OAuth refresh returned a token that expires too soon for "+providerID, nil)
		}
	}

	auth, err := oauth.ToAuth(credential)
	if err != nil {
		return nil, newModelsError(ErrOAuth, "OAuth auth derivation failed for "+providerID, err)
	}
	return &AuthResult{Auth: auth, Source: "OAuth"}, nil
}

// resolveApiKey runs a provider's api-key resolver and wraps failures.
func resolveApiKey(
	ctx context.Context,
	authCtx AuthContext,
	apiKey *ApiKeyAuth,
	providerID string,
	credential *Credential,
) (*AuthResult, error) {
	res, err := apiKey.Resolve(ctx, authCtx, credential)
	if err != nil {
		if isCancellation(ctx, err) {
			return nil, err
		}
		return nil, newModelsError(ErrAuth, "API key auth failed for provider "+providerID, err)
	}
	return res, nil
}

// readCredential reads from the store and wraps failures.
func readCredential(ctx context.Context, credentials CredentialStore, providerID string) (*Credential, error) {
	c, err := credentials.Read(ctx, providerID)
	if err != nil {
		if isCancellation(ctx, err) {
			return nil, err
		}
		return nil, newModelsError(ErrAuth, "Credential store read failed for "+providerID, err)
	}
	return c, nil
}
