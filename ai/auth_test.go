package ai

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuthContext is an injectable AuthContext over a fixed env map.
type fakeAuthContext struct {
	env   map[string]string
	files map[string]bool
}

func (f fakeAuthContext) Env(name string) string   { return f.env[name] }
func (f fakeAuthContext) FileExists(p string) bool { return f.files[p] }

func TestInMemoryCredentialStore(t *testing.T) {
	s := NewInMemoryCredentialStore()

	if c, err := s.Read(context.Background(), "p"); err != nil || c != nil {
		t.Fatalf("empty read = (%v, %v), want (nil, nil)", c, err)
	}

	// Modify sets a credential and returns the post-write value.
	got, err := s.Modify(context.Background(), "p", func(cur *Credential) (*Credential, error) {
		if cur != nil {
			t.Errorf("fn should see nil current, got %v", cur)
		}
		return &Credential{Type: CredentialAPIKey, Key: "k1"}, nil
	})
	if err != nil || got == nil || got.Key != "k1" {
		t.Fatalf("modify = (%v, %v), want key k1", got, err)
	}

	// Read returns a copy (mutating it must not affect the store).
	c, _ := s.Read(context.Background(), "p")
	c.Key = "mutated"
	if again, _ := s.Read(context.Background(), "p"); again.Key != "k1" {
		t.Errorf("store credential was aliased: got %q", again.Key)
	}

	// fn returning nil leaves the entry unchanged and returns current.
	got, _ = s.Modify(context.Background(), "p", func(cur *Credential) (*Credential, error) { return nil, nil })
	if got == nil || got.Key != "k1" {
		t.Errorf("no-op modify should return current k1, got %v", got)
	}

	// fn error leaves the entry unchanged.
	if _, err := s.Modify(context.Background(), "p", func(cur *Credential) (*Credential, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Error("modify should propagate fn error")
	}
	if after, _ := s.Read(context.Background(), "p"); after == nil || after.Key != "k1" {
		t.Errorf("entry should survive a failed modify, got %v", after)
	}

	// Delete removes it.
	_ = s.Delete(context.Background(), "p")
	if after, _ := s.Read(context.Background(), "p"); after != nil {
		t.Errorf("delete should remove the entry, got %v", after)
	}
}

// TestCredentialAPIKeyJSON locks the on-disk auth.json format aligned in pi
// 49fbe683: the discriminator serializes as "api_key" (underscore, not the old
// "api-key") and the provider-scoped values serialize under "env" (not the old
// "metadata"). Serialization-visible: this is the persisted credential shape.
func TestCredentialAPIKeyJSON(t *testing.T) {
	cred := &Credential{
		Type: CredentialAPIKey,
		Key:  "secret",
		Env:  map[string]string{"accountId": "acct-123", "gatewayId": "gw-1"},
	}

	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	// Exact on-disk discriminator and env key.
	if !strings.Contains(got, `"type":"api_key"`) {
		t.Errorf("type must serialize as api_key, got %s", got)
	}
	if !strings.Contains(got, `"env":{`) {
		t.Errorf("provider env must serialize under \"env\", got %s", got)
	}
	// The old format must be gone.
	if strings.Contains(got, "api-key") {
		t.Errorf("legacy \"api-key\" discriminator must not appear, got %s", got)
	}
	if strings.Contains(got, "metadata") {
		t.Errorf("legacy \"metadata\" key must not appear, got %s", got)
	}

	// Round-trips back to an equal value, and parses on the new api_key type.
	var back Credential
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != CredentialAPIKey {
		t.Errorf("decoded type = %q, want %q", back.Type, CredentialAPIKey)
	}
	if !reflect.DeepEqual(&back, cred) {
		t.Errorf("round-trip mismatch: %+v vs %+v", &back, cred)
	}

	// Decoding the literal on-disk shape selects the api-key resolve path.
	var fromDisk Credential
	if err := json.Unmarshal([]byte(`{"type":"api_key","key":"k","env":{"accountId":"a"}}`), &fromDisk); err != nil {
		t.Fatalf("unmarshal disk shape: %v", err)
	}
	if fromDisk.Type != CredentialAPIKey {
		t.Fatalf("on-disk api_key must match CredentialAPIKey, got %q", fromDisk.Type)
	}
	res, err := resolveProviderAuth(context.Background(),
		"test",
		ProviderAuth{APIKey: EnvAPIKeyAuth("Test")},
		seededStore(t, "test", &fromDisk),
		fakeAuthContext{},
		nil,
	)
	if err != nil || res == nil || res.Auth.APIKey != "k" {
		t.Fatalf("api_key credential must resolve via the api-key path: %+v (err %v)", res, err)
	}
}

func seededStore(t *testing.T, provider string, c *Credential) CredentialStore {
	t.Helper()
	s := NewInMemoryCredentialStore()
	if _, err := s.Modify(context.Background(), provider, func(*Credential) (*Credential, error) { return c, nil }); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return s
}

// TestResolveProviderAuthRequestOverrides locks ef231c49: a request apiKey
// resolves directly ahead of a stored credential, and request env overlays the
// AuthContext seen by the provider's Resolve.
func TestResolveProviderAuthRequestOverrides(t *testing.T) {
	auth := ProviderAuth{APIKey: &ApiKeyAuth{
		Name: "Test",
		Resolve: func(_ context.Context, authCtx AuthContext, credential *Credential) (*AuthResult, error) {
			key := authCtx.Env("FALLBACK_KEY")
			if credential != nil && credential.Key != "" {
				key = credential.Key
			}
			if key == "" {
				return nil, nil
			}
			return &AuthResult{Auth: ModelAuth{APIKey: key}, Env: map[string]string{"ACCT": authCtx.Env("ACCT")}}, nil
		},
	}}
	stored := &Credential{Type: CredentialAPIKey, Key: "stored-key"}

	// Request apiKey override wins over the stored credential.
	res, err := resolveProviderAuth(context.Background(), "test", auth, seededStore(t, "test", stored), fakeAuthContext{}, &AuthResolutionOverrides{APIKey: "req-key"})
	if err != nil || res == nil || res.Auth.APIKey != "req-key" {
		t.Fatalf("override apiKey must resolve directly: %+v err %v", res, err)
	}

	// Override env overlays ambient and is visible to Resolve.
	res, err = resolveProviderAuth(context.Background(), "test", auth, seededStore(t, "test", stored),
		fakeAuthContext{env: map[string]string{"ACCT": "ambient"}},
		&AuthResolutionOverrides{Env: map[string]string{"ACCT": "scoped"}})
	if err != nil || res == nil || res.Env["ACCT"] != "scoped" {
		t.Fatalf("override env must overlay ambient in Resolve(): %+v err %v", res, err)
	}
}

func TestEnvAPIKeyAuthResolve(t *testing.T) {
	auth := EnvAPIKeyAuth("Test key", "PRIMARY_KEY", "SECONDARY_KEY")

	// Stored credential key wins over env, and its env section passes through
	// (upstream 1942b260 — a stored credential must not drop its `env` overrides).
	ctx := fakeAuthContext{env: map[string]string{"PRIMARY_KEY": "from-env"}}
	res, err := auth.Resolve(context.Background(), ctx, &Credential{Type: CredentialAPIKey, Key: "stored", Env: map[string]string{"PI_CACHE_RETENTION": "1h"}})
	if err != nil || res == nil || res.Auth.APIKey != "stored" || res.Source != "stored credential" {
		t.Fatalf("stored key should win: %+v (err %v)", res, err)
	}
	if res.Env["PI_CACHE_RETENTION"] != "1h" {
		t.Fatalf("stored credential env section dropped: %+v", res.Env)
	}

	// No stored credential: first set env var in order resolves.
	res, _ = auth.Resolve(context.Background(), fakeAuthContext{env: map[string]string{"SECONDARY_KEY": "second"}}, nil)
	if res == nil || res.Auth.APIKey != "second" || res.Source != "SECONDARY_KEY" {
		t.Fatalf("env fallback wrong: %+v", res)
	}

	// Unconfigured -> nil.
	if res, _ := auth.Resolve(context.Background(), fakeAuthContext{}, nil); res != nil {
		t.Fatalf("unconfigured should be nil, got %+v", res)
	}
}

// TestAnthropicAPIKeyAuthResolve locks pi's anthropicApiKeyAuth precedence
// (upstream 24e5cc04): stored key → ANTHROPIC_AUTH_TOKEN (Authorization header) →
// ANTHROPIC_OAUTH_TOKEN/ANTHROPIC_API_KEY (api key).
func TestAnthropicAPIKeyAuthResolve(t *testing.T) {
	auth := anthropicAPIKeyAuth()

	// Stored credential key wins over both the auth token and the api-key envs.
	ctx := fakeAuthContext{env: map[string]string{
		AnthropicAuthTokenEnv: "tok", "ANTHROPIC_API_KEY": "sk-env",
	}}
	res, err := auth.Resolve(context.Background(), ctx, &Credential{Type: CredentialAPIKey, Key: "stored"})
	if err != nil || res == nil || res.Auth.APIKey != "stored" || res.Source != "stored credential" {
		t.Fatalf("stored key should win: %+v (err %v)", res, err)
	}

	// Auth token authenticates via an Authorization header, never x-api-key, and
	// beats a plain ANTHROPIC_API_KEY.
	res, _ = auth.Resolve(context.Background(), fakeAuthContext{env: map[string]string{
		AnthropicAuthTokenEnv: "tok", "ANTHROPIC_API_KEY": "sk-env",
	}}, nil)
	if res == nil || res.Auth.APIKey != "" || res.Source != AnthropicAuthTokenEnv ||
		res.Auth.Headers["Authorization"] == nil || *res.Auth.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("auth token should resolve to a bearer header: %+v", res)
	}

	// Without the auth token, the api-key envs resolve as a key.
	res, _ = auth.Resolve(context.Background(), fakeAuthContext{env: map[string]string{"ANTHROPIC_API_KEY": "sk-env"}}, nil)
	if res == nil || res.Auth.APIKey != "sk-env" || res.Source != "ANTHROPIC_API_KEY" {
		t.Fatalf("api-key fallback wrong: %+v", res)
	}

	// Unconfigured -> nil.
	if res, _ := auth.Resolve(context.Background(), fakeAuthContext{}, nil); res != nil {
		t.Fatalf("unconfigured should be nil, got %+v", res)
	}
}

// TestAnthropicAPIKeyAuthResolvesFederation mirrors upstream a9424cd43's
// anthropic-federation.test.ts resolver cases: with no key and no auth token,
// ANTHROPIC_FEDERATION_RULE_ID, ANTHROPIC_ORGANIZATION_ID and
// ANTHROPIC_IDENTITY_TOKEN_FILE (all three) configure the provider with no
// request auth and the ids in env, the service account and workspace ids
// riding along when set; keys and ANTHROPIC_AUTH_TOKEN keep winning.
func TestAnthropicAPIKeyAuthResolvesFederation(t *testing.T) {
	auth := anthropicAPIKeyAuth()
	federation := map[string]string{
		"ANTHROPIC_FEDERATION_RULE_ID":  "fdrl_test",
		"ANTHROPIC_ORGANIZATION_ID":     "org-test",
		"ANTHROPIC_SERVICE_ACCOUNT_ID":  "svac_test",
		"ANTHROPIC_IDENTITY_TOKEN_FILE": "/tmp/identity.jwt",
	}
	with := func(extra map[string]string, omit ...string) map[string]string {
		env := maps.Clone(federation)
		maps.Copy(env, extra)
		for _, name := range omit {
			delete(env, name)
		}
		return env
	}
	resolve := func(env map[string]string) *AuthResult {
		t.Helper()
		res, err := auth.Resolve(context.Background(), fakeAuthContext{env: env}, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return res
	}
	federated := func(env map[string]string) *AuthResult {
		return &AuthResult{Env: env, Source: "workload identity federation"}
	}

	if got, want := resolve(federation), federated(federation); !reflect.DeepEqual(got, want) {
		t.Errorf("federation: got %+v, want %+v", got, want)
	}
	withWorkspace := with(map[string]string{"ANTHROPIC_WORKSPACE_ID": "wrkspc_test"})
	if got, want := resolve(withWorkspace), federated(withWorkspace); !reflect.DeepEqual(got, want) {
		t.Errorf("workspace: got %+v, want %+v", got, want)
	}
	noServiceAccount := with(nil, "ANTHROPIC_SERVICE_ACCOUNT_ID")
	if got, want := resolve(noServiceAccount), federated(noServiceAccount); !reflect.DeepEqual(got, want) {
		t.Errorf("service account is optional: got %+v, want %+v", got, want)
	}
	for _, required := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE"} {
		if got := resolve(with(nil, required)); got != nil {
			t.Errorf("without %s: got %+v, want unconfigured", required, got)
		}
	}
	if got := resolve(with(map[string]string{"ANTHROPIC_API_KEY": "api-key"})); got == nil ||
		!reflect.DeepEqual(*got, AuthResult{Auth: ModelAuth{APIKey: "api-key"}, Source: "ANTHROPIC_API_KEY"}) {
		t.Errorf("api key should win over federation: %+v", got)
	}
	got := resolve(with(map[string]string{AnthropicAuthTokenEnv: "auth-token"}))
	if got == nil || got.Source != AnthropicAuthTokenEnv || got.Env != nil ||
		got.Auth.Headers["Authorization"] == nil || *got.Auth.Headers["Authorization"] != "Bearer auth-token" {
		t.Errorf("auth token should win over federation: %+v", got)
	}
}

func TestResolveProviderAuthAPIKeyAmbient(t *testing.T) {
	auth := ProviderAuth{APIKey: EnvAPIKeyAuth("Test", "TEST_KEY")}
	store := NewInMemoryCredentialStore()
	ctx := fakeAuthContext{env: map[string]string{"TEST_KEY": "ambient"}}

	res, err := resolveProviderAuth(context.Background(), "test", auth, store, ctx, nil)
	if err != nil || res == nil || res.Auth.APIKey != "ambient" {
		t.Fatalf("ambient api-key resolution wrong: %+v (err %v)", res, err)
	}
}

func TestResolveProviderAuthOAuthRefreshUnderLock(t *testing.T) {
	store := NewInMemoryCredentialStore()
	// Seed an EXPIRED oauth credential.
	_, _ = store.Modify(context.Background(), "oauthp", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1}, nil
	})

	refreshCalls := 0
	auth := ProviderAuth{OAuth: &OAuthAuth{
		Name: "Test OAuth",
		Refresh: func(_ context.Context, c OAuthCredentials) (OAuthCredentials, error) {
			refreshCalls++
			return OAuthCredentials{Refresh: "r1", Access: "new", Expires: nowMillis() + 3_600_000}, nil
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) {
			return ModelAuth{APIKey: c.Access}, nil
		},
	}}

	res, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), nil)
	if err != nil || res == nil || res.Auth.APIKey != "new" || res.Source != "OAuth" {
		t.Fatalf("expired oauth should refresh then derive: %+v (err %v)", res, err)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected exactly one refresh, got %d", refreshCalls)
	}
	// The rotated credential must be persisted.
	if stored, _ := store.Read(context.Background(), "oauthp"); stored == nil || stored.Access != "new" {
		t.Fatalf("rotated credential not persisted: %+v", stored)
	}

	// A still-valid credential is not refreshed.
	res, _ = resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), nil)
	if res == nil || res.Auth.APIKey != "new" || refreshCalls != 1 {
		t.Fatalf("valid token should not refresh again: %+v calls=%d", res, refreshCalls)
	}
}

func TestResolveProviderAuthOAuthRefreshFailure(t *testing.T) {
	store := NewInMemoryCredentialStore()
	_, _ = store.Modify(context.Background(), "oauthp", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1}, nil
	})

	auth := ProviderAuth{OAuth: &OAuthAuth{
		Refresh: func(_ context.Context, c OAuthCredentials) (OAuthCredentials, error) {
			return OAuthCredentials{}, errors.New("invalid_grant")
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{}, nil },
	}}

	_, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), nil)
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrOAuth {
		t.Fatalf("refresh failure should be ModelsError code oauth, got %v", err)
	}
	// The stored credential is preserved for retry.
	if stored, _ := store.Read(context.Background(), "oauthp"); stored == nil || stored.Access != "old" {
		t.Fatalf("failed refresh must preserve the stored credential, got %+v", stored)
	}
}

// TestResolveStoredOAuthBoundsRefreshDuration locks pi acbdc0d25: the refresh
// runs under a context bounded by BOTH the caller's cancellation and a fifteen
// second timeout, so a provider that never answers cannot hold the credential
// store lock forever. Upstream's own test for this was removed the next commit
// (b784c8096) as flaky; this one observes the bound through the context the
// provider actually receives instead of waiting on wall-clock time.
func TestResolveStoredOAuthBoundsRefreshDuration(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 0) // expired: forces a refresh

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()

	var deadline time.Time
	var hasDeadline, cancelPropagated bool
	auth := ProviderAuth{OAuth: &OAuthAuth{
		Refresh: func(ctx context.Context, c OAuthCredentials) (OAuthCredentials, error) {
			deadline, hasDeadline = ctx.Deadline()
			// The caller's cancellation must still reach the provider.
			cancelCaller()
			<-ctx.Done()
			cancelPropagated = errors.Is(ctx.Err(), context.Canceled)
			return OAuthCredentials{}, ctx.Err()
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
	}}

	before := time.Now()
	_, _ = resolveProviderAuth(callerCtx, "oauthp", auth, store, authCtx(), nil)

	if !hasDeadline {
		t.Fatal("refresh context has no deadline: the refresh is unbounded")
	}
	if got := deadline.Sub(before); got > 16*time.Second || got < 14*time.Second {
		t.Fatalf("refresh deadline is %v out, want ~15s", got)
	}
	if !cancelPropagated {
		t.Fatal("caller cancellation did not reach the refresh context")
	}
}

// TestResolveStoredOAuthRefreshContextIsComposite locks upstream 42a06f947 and
// d3da2e968, which tightened pi's assertion from "the refresh signal IS the
// caller's signal" to "the refresh signal is a DISTINCT composite that aborts
// with the caller and carries the caller's abort reason". The Go composite is
// context.WithTimeout(callerCtx, ...): a context of its own, done as soon as
// the caller is done, and — because Go propagates a cancellation cause to every
// derived context — reporting the caller's cause from context.Cause. Deriving
// the refresh context from anything but the caller's (a detached background
// context, say) would keep the deadline but lose both of the latter.
func TestResolveStoredOAuthRefreshContextIsComposite(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 0) // expired: forces a refresh
	reason := errors.New("caller went away")
	callerCtx, cancelCaller := context.WithCancelCause(context.Background())
	defer cancelCaller(nil)

	var distinct bool
	var refreshErr, refreshCause error
	auth := ProviderAuth{OAuth: &OAuthAuth{
		Refresh: func(ctx context.Context, c OAuthCredentials) (OAuthCredentials, error) {
			distinct = ctx != callerCtx
			cancelCaller(reason)
			<-ctx.Done()
			refreshErr, refreshCause = ctx.Err(), context.Cause(ctx)
			return OAuthCredentials{}, ctx.Err()
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
	}}

	_, _ = resolveProviderAuth(callerCtx, "oauthp", auth, store, authCtx(), nil)

	if !distinct {
		t.Fatal("refresh received the caller's own context, not a composite derived from it")
	}
	if !errors.Is(refreshErr, context.Canceled) {
		t.Fatalf("caller cancellation did not cancel the refresh context: %v", refreshErr)
	}
	if !errors.Is(refreshCause, reason) {
		t.Fatalf("caller's cancellation cause did not reach the refresh context: %v", refreshCause)
	}
}

// Upstream 02eed88fd: lazyOAuth passes login options on to the loaded flow.
func TestLazyOAuthForwardsLoginOptions(t *testing.T) {
	var got *LoginOptions
	wrapper := LazyOAuth(LazyOAuthOptions{
		Name: "Test",
		Load: func() (*OAuthAuth, error) {
			return &OAuthAuth{Login: func(_ context.Context, _ AuthInteraction, options *LoginOptions) (*Credential, error) {
				got = options
				return &Credential{Type: CredentialOAuth}, nil
			}}, nil
		},
	})
	options := &LoginOptions{GetDeviceID: func() string { return "device-1" }}
	if _, err := wrapper.Login(context.Background(), nil, options); err != nil {
		t.Fatal(err)
	}
	if got != options {
		t.Fatalf("the loaded flow received %p, want the caller's options %p", got, options)
	}
}

// TestLazyOAuthCarriesDescriptorFields locks upstream b0bd0ff9d
// (IsSubscription) and a01baaae (LoginLabel): LazyOAuth carries the descriptor
// fields onto the wrapper itself, readable without loading the implementation,
// so hosts can label subscription-backed OAuth (the footer's "(sub)") before
// any auth flow runs — while Login/Refresh/ToAuth still route to the lazily
// loaded implementation.
func TestLazyOAuthCarriesDescriptorFields(t *testing.T) {
	loads := 0
	impl := &OAuthAuth{
		Login: func(_ context.Context, _ AuthInteraction, _ *LoginOptions) (*Credential, error) {
			return &Credential{Type: CredentialOAuth, Access: "logged-in"}, nil
		},
		Refresh: func(_ context.Context, c OAuthCredentials) (OAuthCredentials, error) {
			return OAuthCredentials{Refresh: "r1", Access: "new", Expires: 42}, nil
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) {
			return ModelAuth{APIKey: c.Access}, nil
		},
	}
	wrapper := LazyOAuth(LazyOAuthOptions{
		Name:           "Test (subscription)",
		IsSubscription: true,
		LoginLabel:     "Sign in with Test",
		Load: func() (*OAuthAuth, error) {
			loads++
			return impl, nil
		},
	})

	// The descriptor fields are readable without loading the implementation.
	if wrapper.Name != "Test (subscription)" {
		t.Errorf("Name = %q, want the input's name", wrapper.Name)
	}
	if !wrapper.IsSubscription {
		t.Error("IsSubscription was dropped by the wrapper")
	}
	if wrapper.LoginLabel != "Sign in with Test" {
		t.Error("LoginLabel was dropped by the wrapper")
	}
	if loads != 0 {
		t.Fatalf("reading descriptor fields must not load the implementation (loaded %d times)", loads)
	}

	// Login, Refresh, and ToAuth route to the implementation, loading it once.
	cred, err := wrapper.Login(context.Background(), nil, nil)
	if err != nil || cred == nil || cred.Access != "logged-in" {
		t.Fatalf("Login must route to the implementation: %+v (err %v)", cred, err)
	}
	refreshed, err := wrapper.Refresh(context.Background(), OAuthCredentials{Refresh: "r0"})
	if err != nil || refreshed.Access != "new" {
		t.Fatalf("Refresh must route to the implementation: %+v (err %v)", refreshed, err)
	}
	auth, err := wrapper.ToAuth(refreshed)
	if err != nil || auth.APIKey != "new" {
		t.Fatalf("ToAuth must route to the implementation: %+v (err %v)", auth, err)
	}
	if loads != 1 {
		t.Fatalf("expected exactly one load, got %d", loads)
	}

	// Like pi's openRouterOAuth, a wrapper that never opts in reports false.
	plain := LazyOAuth(LazyOAuthOptions{Name: "Plain OAuth", Load: func() (*OAuthAuth, error) { return impl, nil }})
	if plain.IsSubscription {
		t.Error("a wrapper without the opt-in must not report a subscription")
	}
}

// TestCredentialStoreList locks pi ff28097a's list(): non-secret metadata
// only, one entry per provider.
func TestCredentialStoreList(t *testing.T) {
	s := NewInMemoryCredentialStore()
	_, _ = s.Modify(context.Background(), "b-oauth", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r", Access: "secret", Expires: 1}, nil
	})
	_, _ = s.Modify(context.Background(), "a-key", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialAPIKey, Key: "secret"}, nil
	})

	infos, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []CredentialInfo{
		{ProviderID: "a-key", Type: CredentialAPIKey},
		{ProviderID: "b-oauth", Type: CredentialOAuth},
	}
	if !reflect.DeepEqual(infos, want) {
		t.Fatalf("list = %+v, want %+v", infos, want)
	}
}

func authCtx() AuthContext { return fakeAuthContext{} }

// oauthProviderAuth builds an OAuth provider whose refresh yields a token
// expiring in expiresInMs and counts its calls. The counter is atomic because
// refresh runs inside the credential store's modify callback.
func oauthProviderAuth(calls *atomic.Int64, expiresInMs int64) ProviderAuth {
	return ProviderAuth{OAuth: &OAuthAuth{
		Name: "Test OAuth",
		Refresh: func(_ context.Context, c OAuthCredentials) (OAuthCredentials, error) {
			calls.Add(1)
			return OAuthCredentials{Refresh: "r1", Access: "new", Expires: nowMillis() + expiresInMs}, nil
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
	}}
}

// seedOAuthStore returns a store holding an OAuth credential expiring in
// expiresInMs.
func seedOAuthStore(t *testing.T, providerID string, expiresInMs int64) CredentialStore {
	t.Helper()
	return seededStore(t, providerID, &Credential{
		Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: nowMillis() + expiresInMs,
	})
}

// TestResolveStoredOAuthRefreshesWithinDefaultValidityWindow locks upstream
// 99e34013: a token still valid but expiring inside the default five-minute
// window is refreshed, where previously only a fully expired token was.
func TestResolveStoredOAuthRefreshesWithinDefaultValidityWindow(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 2*60*1000) // two minutes left

	var calls atomic.Int64
	auth := oauthProviderAuth(&calls, 3_600_000)

	res, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), nil)
	if err != nil || res == nil || res.Auth.APIKey != "new" {
		t.Fatalf("token inside the five-minute window must refresh: %+v (err %v)", res, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one refresh, got %d", calls.Load())
	}
}

// divergentReadStore reports an expired credential to the optimistic read but
// hands the under-lock callback a credential that is unexpired yet expires
// soon, so only the re-check's validity window decides whether a refresh
// happens. It is single-goroutine: it models divergent reads, not a race.
type divergentReadStore struct {
	CredentialStore
	underLock *Credential
}

func (s divergentReadStore) Read(context.Context, string) (*Credential, error) {
	return &Credential{Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1}, nil
}

func (s divergentReadStore) Modify(_ context.Context, _ string, fn func(*Credential) (*Credential, error)) (*Credential, error) {
	next, err := fn(s.underLock)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return s.underLock, nil
	}
	return next, nil
}

// TestResolveStoredOAuthUnderLockUsesValidityWindow locks that the
// double-checked re-check uses the same window as the optimistic check: a
// credential another request left with two minutes of life still refreshes.
func TestResolveStoredOAuthUnderLockUsesValidityWindow(t *testing.T) {
	store := divergentReadStore{
		CredentialStore: NewInMemoryCredentialStore(),
		underLock: &Credential{
			Type: CredentialOAuth, Refresh: "r0", Access: "short", Expires: nowMillis() + 2*60*1000,
		},
	}

	var calls atomic.Int64
	auth := oauthProviderAuth(&calls, 3_600_000)

	res, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), nil)
	if err != nil || res == nil || res.Auth.APIKey != "new" {
		t.Fatalf("under-lock re-check must apply the validity window: %+v (err %v)", res, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one refresh, got %d", calls.Load())
	}
}

// TestResolveStoredOAuthExplicitMinValidityUnsatisfied locks the post-refresh
// contract that only explicit callers get: a refreshed token that still expires
// inside the requested window is an error.
func TestResolveStoredOAuthExplicitMinValidityUnsatisfied(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 1000)

	var calls atomic.Int64
	auth := oauthProviderAuth(&calls, 10*60*1000) // ten minutes, short of the hour asked for

	min := int64(60 * 60 * 1000)
	_, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), &AuthResolutionOverrides{MinOAuthValidityMs: &min})
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrOAuth {
		t.Fatalf("want an ErrOAuth ModelsError, got %v", err)
	}
	if me.Message != "OAuth refresh returned a token that expires too soon for oauthp" {
		t.Fatalf("error message must match pi byte for byte, got %q", me.Message)
	}
}

// TestResolveStoredOAuthDefaultWindowImposesNoContract locks the asymmetry: the
// default window triggers a refresh but never rejects the refreshed token.
func TestResolveStoredOAuthDefaultWindowImposesNoContract(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 1000)

	var calls atomic.Int64
	auth := oauthProviderAuth(&calls, 60*1000) // still inside the five-minute window

	res, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), nil)
	if err != nil || res == nil || res.Auth.APIKey != "new" {
		t.Fatalf("default window must not reject a short-lived refreshed token: %+v (err %v)", res, err)
	}
}

// TestResolveStoredOAuthZeroMinValidityArmsContract pins the gate at "set at
// all", not "set to something positive": pi tests `!== undefined`, so an
// explicit zero still arms the post-refresh contract under the default window.
func TestResolveStoredOAuthZeroMinValidityArmsContract(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 1000)

	var calls atomic.Int64
	auth := oauthProviderAuth(&calls, 60*1000) // refreshes to 60s: inside the default window

	min := int64(0)
	_, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), &AuthResolutionOverrides{MinOAuthValidityMs: &min})
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrOAuth {
		t.Fatalf("an explicit zero minimum must still arm the contract, got %v", err)
	}
}

// TestResolveStoredOAuthMinValidityCannotShrinkWindow locks the max(): an
// override below five minutes leaves the default window in force.
func TestResolveStoredOAuthMinValidityCannotShrinkWindow(t *testing.T) {
	store := seedOAuthStore(t, "oauthp", 2*60*1000) // two minutes: inside the default window only

	var calls atomic.Int64
	auth := oauthProviderAuth(&calls, 3_600_000)

	min := int64(1000)
	res, err := resolveProviderAuth(context.Background(), "oauthp", auth, store, authCtx(), &AuthResolutionOverrides{MinOAuthValidityMs: &min})
	if err != nil || res == nil || res.Auth.APIKey != "new" {
		t.Fatalf("a smaller override must not shrink the window: %+v (err %v)", res, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one refresh, got %d", calls.Load())
	}
}

// TestCredentialStoreCancelsQueuedModify mirrors pi "cancels queued credential
// mutations without running them later" (upstream fed6009c): a mutation whose
// context is cancelled while it is still queued behind another gives up without
// ever running its callback, and the in-flight mutation still settles.
func TestCredentialStoreCancelsQueuedModify(t *testing.T) {
	s := NewInMemoryCredentialStore()
	firstRunning := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.Modify(context.Background(), "p1", func(*Credential) (*Credential, error) {
			close(firstRunning)
			<-releaseFirst
			return &Credential{Type: CredentialAPIKey, Key: "first"}, nil
		})
		firstDone <- err
	}()
	<-firstRunning

	var secondRan atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	queued := make(chan struct{})
	go func() {
		close(queued)
		_, err := s.Modify(ctx, "p1", func(*Credential) (*Credential, error) {
			secondRan.Store(true)
			return &Credential{Type: CredentialAPIKey, Key: "second"}, nil
		})
		secondDone <- err
	}()
	<-queued
	cancel()

	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled queued mutation must report cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		close(releaseFirst)
		t.Fatal("a cancelled queued mutation must stop waiting for the chain")
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("the in-flight mutation must still settle: %v", err)
	}
	if secondRan.Load() {
		t.Fatal("a cancelled queued mutation must never run its callback")
	}
	stored, _ := s.Read(context.Background(), "p1")
	if stored == nil || stored.Key != "first" {
		t.Fatalf("stored credential = %+v, want the first mutation's value", stored)
	}
}

// TestCredentialStoreCancelDuringCallbackDropsWrite locks the other half of pi's
// reworked enqueue: a mutation cancelled while its callback was in flight does
// not commit, so the stored credential is exactly what it was.
func TestCredentialStoreCancelDuringCallbackDropsWrite(t *testing.T) {
	s := NewInMemoryCredentialStore()
	if _, err := s.Modify(context.Background(), "p1", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialAPIKey, Key: "existing"}, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.Modify(ctx, "p1", func(*Credential) (*Credential, error) {
		cancel() // the caller gives up while the callback is still working
		return &Credential{Type: CredentialAPIKey, Key: "late"}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	stored, _ := s.Read(context.Background(), "p1")
	if stored == nil || stored.Key != "existing" {
		t.Fatalf("a cancelled mutation must not commit: %+v", stored)
	}
}
