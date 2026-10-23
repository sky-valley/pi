package ai

import "context"

// Auth substrate ported from pi packages/ai/src/auth/types.ts (732bb161,
// ff28097a).
//
// Structural divergence (G-package): pi splits auth into its own module
// (packages/ai/src/auth/*). In Go an ai/auth subpackage would import ai for
// Model/Api while the Models runtime in ai imports the auth substrate — an
// import cycle. So the substrate lives as auth_*.go files in package ai,
// mirroring pi's auth/* file-for-file. Async (Promise) maps to synchronous
// (T, error); undefined maps to nil pointers / empty strings.

// ModelAuth is the request auth for a single model request. A value that cannot
// be expressed as APIKey, Headers, or BaseURL is provider config, not auth.
type ModelAuth struct {
	APIKey string
	// Headers carry header-owned auth. A nil value is a deletion marker that
	// suppresses a provider default header (see ProviderHeaders) — how
	// cloudflare-ai-gateway turns off the upstream Authorization/x-api-key.
	Headers ProviderHeaders
	BaseURL string
}

// CredentialKind tags a stored Credential ("api_key" or "oauth").
type CredentialKind string

const (
	CredentialAPIKey CredentialKind = "api_key"
	CredentialOAuth  CredentialKind = "oauth"
)

// OAuthCredentials is the stored OAuth token triplet (auth/types.ts; relocated
// upstream from utils/oauth/types.ts in ff28097a). Expires is epoch
// milliseconds, matching pi's Date.now() comparisons.
type OAuthCredentials struct {
	Refresh string `json:"refresh"`
	Access  string `json:"access"`
	Expires int64  `json:"expires"`
}

// Credential is one type-tagged credential per provider — the shape of pi's
// auth.json. pi models it as a discriminated union (ApiKeyCredential |
// OAuthCredential); Go uses a single Type-tagged struct whose JSON serializes
// to either {type:"api_key",key,env} or {type:"oauth",refresh,access,
// expires}, the idiomatic flat tagged-union. For an api-key credential Env
// holds provider-scoped environment/config values such as Cloudflare
// account/gateway ids.
type Credential struct {
	Type    CredentialKind    `json:"type"`
	Key     string            `json:"key,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Refresh string            `json:"refresh,omitempty"`
	Access  string            `json:"access,omitempty"`
	Expires int64             `json:"expires,omitempty"`
	// AvailableModelIDs restricts the provider's catalog for this credential.
	// pi's OAuthCredentials carries an open index signature ([key: string]:
	// unknown); the one real use is github-copilot's availableModelIds, which
	// the Go port types explicitly (consumed by the provider's FilterModels).
	AvailableModelIDs []string `json:"availableModelIds,omitempty"`
}

// CredentialInfo is non-secret credential metadata for account/status
// enumeration (pi CredentialInfo).
type CredentialInfo struct {
	ProviderID string
	Type       CredentialKind
}

// OAuthCredentials projects the oauth fields of a Credential.
func (c *Credential) OAuthCredentials() OAuthCredentials {
	return OAuthCredentials{Refresh: c.Refresh, Access: c.Access, Expires: c.Expires}
}

// oauthCredential builds an oauth-typed Credential from a token triplet.
func oauthCredential(o OAuthCredentials) *Credential {
	return &Credential{Type: CredentialOAuth, Refresh: o.Refresh, Access: o.Access, Expires: o.Expires}
}

// AuthResult is the result of resolving auth for a model.
type AuthResult struct {
	Auth ModelAuth
	// Env holds provider-scoped environment/config values resolved from the
	// credential and ambient context (pi 2cbce395). Latent until a provider's
	// Resolve populates it; merged into the request options' Env by the Models
	// runtime.
	Env map[string]string
	// Source is a human-readable label for status UI, e.g. "ANTHROPIC_API_KEY",
	// "OAuth", "~/.aws/credentials".
	Source string
}

// CredentialStore is app-owned credential storage, keyed by provider id, one
// credential per provider. Modify is the only write path, so every mutation is
// a serialized read-modify-write; the Models runtime runs OAuth refresh inside
// Modify so concurrent requests cannot double-refresh a rotated token.
//
// Error semantics: Read returns (nil, nil) for missing entries. Methods return
// an error only on storage failure; the Models runtime wraps such errors in a
// ModelsError with code "auth".
// Cancellation is caller-owned (upstream fed6009c added AuthOperationOptions.
// signal to every method): each operation takes a context.Context, the Go idiom
// for pi's optional AbortSignal. A store that queues work must stop waiting when
// the caller's context is done, and must not apply a mutation whose context was
// cancelled while its callback was in flight.
type CredentialStore interface {
	// Read returns the stored credential, possibly expired, or (nil, nil) when
	// none is stored. Display/status use; resolved request auth comes from the
	// Models runtime's GetAuth.
	Read(ctx context.Context, providerID string) (*Credential, error)

	// List returns stored credential metadata without resolving or exposing
	// secrets. Implementations must not execute configured API-key commands
	// while listing.
	List(ctx context.Context) ([]CredentialInfo, error)

	// Modify is the only write path. fn sees the current credential (nil when
	// none) because correct writes (refresh, login-during-refresh) depend on
	// it; it returns the new credential, or nil to leave the entry unchanged.
	// Writes are serialized per provider id. Returns the post-write credential.
	// An error from fn propagates.
	Modify(ctx context.Context, providerID string, fn func(current *Credential) (*Credential, error)) (*Credential, error)

	// Delete removes a credential (logout), serialized against Modify.
	Delete(ctx context.Context, providerID string) error
}

// AuthContext is the environment access for auth resolution. Injectable for
// tests. Env returns "" for an absent or empty variable; FileExists supports a
// leading "~".
type AuthContext interface {
	Env(name string) string
	FileExists(path string) bool
}

// AuthPrompt is shown to the user during login. Login acquisition is out of
// scope for the port (pi's interactive flows); the types are ported for
// structural parity of the auth substrate.
type AuthPromptType string

const (
	AuthPromptText       AuthPromptType = "text"
	AuthPromptSecret     AuthPromptType = "secret"
	AuthPromptSelect     AuthPromptType = "select"
	AuthPromptManualCode AuthPromptType = "manual_code"
)

// AuthSelectOption is one option of an AuthPromptSelect prompt.
type AuthSelectOption struct {
	ID          string
	Label       string
	Description string
}

// AuthPrompt is a single login prompt (text/secret/select/manual_code).
type AuthPrompt struct {
	Type        AuthPromptType
	Message     string
	Placeholder string
	Options     []AuthSelectOption // select only
}

// AuthEvent is a login progress event (info / auth_url / device_code /
// progress).
type AuthEventType string

const (
	AuthEventInfo       AuthEventType = "info"
	AuthEventURL        AuthEventType = "auth_url"
	AuthEventDeviceCode AuthEventType = "device_code"
	AuthEventProgress   AuthEventType = "progress"
)

// AuthInfoLink is a link attached to an info login event (pi AuthInfoLink).
type AuthInfoLink struct {
	URL   string
	Label string
}

// AuthEvent carries login progress information to the UI.
type AuthEvent struct {
	Type            AuthEventType
	URL             string
	Instructions    string
	UserCode        string
	VerificationURI string
	// OpenBrowserOnConfirm and CopyCodeOnConfirm (device_code only) are pi's
	// `openBrowserOnConfirm?` and `copyCodeOnConfirm?` (upstream 7151099d0):
	// whether a client offers to open VerificationURI, best-effort, and to copy
	// UserCode over the clipboard once the user confirms. Unset means true
	// (read them with Or(true)), and clients may ignore them.
	OpenBrowserOnConfirm OptBool
	CopyCodeOnConfirm    OptBool
	IntervalSeconds      int
	ExpiresInSeconds     int
	Message              string
	Links                []AuthInfoLink // info only
}

// AuthInteraction serves both api-key and OAuth login flows (pi
// AuthInteraction; renamed upstream from AuthLoginCallbacks in ff28097a).
// Prompt returns the entered/selected string (select returns the option id) or
// an error on cancel; Notify emits a progress event.
//
// pi fed6009c introduced ProviderAuthInteraction = AuthInteraction & {signal},
// the normalized interaction handed to provider login implementations. Go keeps
// cancellation on the context.Context that accompanies the interaction, so the
// interaction type itself is unchanged and every provider Login receives a
// non-nil ctx — pi's "always a concrete signal" guarantee.
type AuthInteraction interface {
	Prompt(prompt AuthPrompt) (string, error)
	Notify(event AuthEvent)
}

// AuthCheck reports whether a provider has complete auth configuration
// (pi AuthCheck). Type reuses CredentialKind — pi's AuthType is the same
// "api_key" | "oauth" value set.
type AuthCheck struct {
	Source string
	Type   CredentialKind
}

// LoginOptions is app-supplied context for Models.Login (pi LoginOptions,
// upstream 02eed88fd).
type LoginOptions struct {
	// GetDeviceID returns the stable ID of this app installation, e.g. sent to
	// OpenAI as its agent host ID. Only login flows that need it call it, so an
	// app can create the ID on first use, and it must return the same ID on
	// every later call.
	GetDeviceID func() string
	// AgentName is the name this app introduces itself with during login,
	// e.g. OpenAI's agent name hint and Codex originator. Empty means pi's own
	// name.
	AgentName string
}

// ApiKeyAuth is api-key auth: a stored key/provider env plus ambient sources
// (env vars, AWS profiles, ADC files). pi models this as an object with method
// fields; Go mirrors that as a struct of funcs (idiomatic, since instances are
// built by helpers like envApiKeyAuth, not multiply-implemented).
type ApiKeyAuth struct {
	// Name is the display name, e.g. "Anthropic API key".
	Name string

	// Login is interactive setup (prompt for key/provider env). Nil =
	// ambient-only. Out of scope for the port; present for structural parity.
	// ctx cancels the flow (pi ProviderAuthInteraction.signal, fed6009c).
	Login func(ctx context.Context, interaction AuthInteraction) (*Credential, error)

	// Check is an optional side-effect-free availability check (pi
	// ApiKeyAuth.check). Use it when Resolve may execute commands or perform
	// other request-time work. Nil means the Models runtime checks availability
	// by resolving auth. ctx cancels the check (pi's required signal).
	Check func(ctx context.Context, authCtx AuthContext, credential *Credential) (*AuthCheck, error)

	// Resolve resolves auth from the stored credential and/or ambient sources,
	// merging per field (credential.Key else env("..."), credential.Env?.NAME
	// else env("...")). Returns (nil, nil) when not configured. credential is
	// nil when nothing is stored. Resolution is provider-scoped (ff28097a
	// removed pi's model parameter); model-specific endpoint preparation
	// happens after auth has been resolved. ctx cancels the resolution (pi's
	// required signal); implementations that block must honor it.
	Resolve func(ctx context.Context, authCtx AuthContext, credential *Credential) (*AuthResult, error)
}

// OAuthAuth is OAuth auth. The Refresh/ToAuth split lets the Models runtime own
// the locked refresh pattern: Refresh produces a credential, ToAuth derives
// request auth from whatever credential ends up stored.
type OAuthAuth struct {
	// Name is the display name, e.g. "Anthropic (Claude Pro/Max)".
	Name string

	// IsSubscription reports whether access through this auth method is backed
	// by a provider subscription (pi b0bd0ff9d; optional, false when unset).
	IsSubscription bool

	// LoginLabel is the selector label for the OAuth login option,
	// e.g. "Sign in with SuperGrok or X Premium" (pi a01baaae; optional, ""
	// when unset). Latent: the interactive login UI is unported.
	LoginLabel string

	// Login runs the interactive OAuth flow. Out of scope for the port
	// (OAuth-acquisition exclusion); present for structural parity. ctx cancels
	// the flow (pi ProviderAuthInteraction.signal, fed6009c). options is the
	// app-supplied context Models.Login forwards, nil when there is none.
	Login func(ctx context.Context, interaction AuthInteraction, options *LoginOptions) (*Credential, error)

	// Refresh exchanges the refresh token; returns an error on failure
	// (invalid_grant etc.). The Models runtime runs this under the store lock.
	// ctx cancels the exchange; it is always the caller's live context
	// (fed6009c made pi's signal required rather than optional, and every
	// resolution path now threads one).
	Refresh func(ctx context.Context, credential OAuthCredentials) (OAuthCredentials, error)

	// ToAuth is the side-effect-free derivation of request auth from a valid
	// credential (covers per-credential baseUrl, e.g. GitHub Copilot).
	ToAuth func(credential OAuthCredentials) (ModelAuth, error)
}

// ProviderAuth is a provider's auth. At least one of APIKey/OAuth must be set:
// even ambient-credential providers and keyless local servers provide APIKey
// auth whose Resolve reports whether the provider is configured.
type ProviderAuth struct {
	APIKey *ApiKeyAuth
	OAuth  *OAuthAuth
}
