package ai

// Auth substrate ported from pi packages/ai/src/auth/types.ts (732bb161).
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
	APIKey  string
	Headers map[string]string
	BaseURL string
}

// CredentialKind tags a stored Credential ("api-key" or "oauth").
type CredentialKind string

const (
	CredentialAPIKey CredentialKind = "api-key"
	CredentialOAuth  CredentialKind = "oauth"
)

// OAuthCredentials is the stored OAuth token triplet (utils/oauth/types.ts).
// Expires is epoch milliseconds, matching pi's Date.now() comparisons.
type OAuthCredentials struct {
	Refresh string `json:"refresh"`
	Access  string `json:"access"`
	Expires int64  `json:"expires"`
}

// Credential is one type-tagged credential per provider — the shape of pi's
// auth.json. pi models it as a discriminated union (ApiKeyCredential |
// OAuthCredential); Go uses a single Type-tagged struct whose JSON serializes
// to either {type:"api-key",key,metadata} or {type:"oauth",refresh,access,
// expires}, the idiomatic flat tagged-union. For an api-key credential Metadata
// holds non-key values such as Cloudflare account/gateway ids.
type Credential struct {
	Type     CredentialKind    `json:"type"`
	Key      string            `json:"key,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Refresh  string            `json:"refresh,omitempty"`
	Access   string            `json:"access,omitempty"`
	Expires  int64             `json:"expires,omitempty"`
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
type CredentialStore interface {
	// Read returns the stored credential, possibly expired, or (nil, nil) when
	// none is stored. Display/status use; resolved request auth comes from the
	// Models runtime's GetAuth.
	Read(providerID string) (*Credential, error)

	// Modify is the only write path. fn sees the current credential (nil when
	// none) because correct writes (refresh, login-during-refresh) depend on
	// it; it returns the new credential, or nil to leave the entry unchanged.
	// Writes are serialized per provider id. Returns the post-write credential.
	// An error from fn propagates.
	Modify(providerID string, fn func(current *Credential) (*Credential, error)) (*Credential, error)

	// Delete removes a credential (logout), serialized against Modify.
	Delete(providerID string) error
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

// AuthEvent is a login progress event (auth_url / device_code / progress).
type AuthEventType string

const (
	AuthEventURL        AuthEventType = "auth_url"
	AuthEventDeviceCode AuthEventType = "device_code"
	AuthEventProgress   AuthEventType = "progress"
)

// AuthEvent carries login progress information to the UI.
type AuthEvent struct {
	Type             AuthEventType
	URL              string
	Instructions     string
	UserCode         string
	VerificationURI  string
	IntervalSeconds  int
	ExpiresInSeconds int
	Message          string
}

// AuthLoginCallbacks serves both api-key and OAuth login flows. Prompt returns
// the entered/selected string (select returns the option id) or an error on
// cancel; Notify emits a progress event.
type AuthLoginCallbacks interface {
	Prompt(prompt AuthPrompt) (string, error)
	Notify(event AuthEvent)
}

// ApiKeyAuth is api-key auth: a stored key/metadata plus ambient sources (env
// vars, AWS profiles, ADC files). pi models this as an object with method
// fields; Go mirrors that as a struct of funcs (idiomatic, since instances are
// built by helpers like envApiKeyAuth, not multiply-implemented).
type ApiKeyAuth struct {
	// Name is the display name, e.g. "Anthropic API key".
	Name string

	// Login is interactive setup (prompt for key/metadata). Nil = ambient-only.
	// Out of scope for the port; present for structural parity.
	Login func(callbacks AuthLoginCallbacks) (*Credential, error)

	// Resolve resolves auth from the stored credential and/or ambient sources,
	// merging per field (credential.Key else env("..."), metadata.accountId
	// else env("...")). Returns (nil, nil) when not configured. credential is
	// nil when nothing is stored.
	Resolve func(model *Model, ctx AuthContext, credential *Credential) (*AuthResult, error)
}

// OAuthAuth is OAuth auth. The Refresh/ToAuth split lets the Models runtime own
// the locked refresh pattern: Refresh produces a credential, ToAuth derives
// request auth from whatever credential ends up stored.
type OAuthAuth struct {
	// Name is the display name, e.g. "Anthropic (Claude Pro/Max)".
	Name string

	// Login runs the interactive OAuth flow. Out of scope for the port
	// (OAuth-acquisition exclusion); present for structural parity.
	Login func(callbacks AuthLoginCallbacks) (*Credential, error)

	// Refresh exchanges the refresh token; returns an error on failure
	// (invalid_grant etc.). The Models runtime runs this under the store lock.
	Refresh func(credential OAuthCredentials) (OAuthCredentials, error)

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
