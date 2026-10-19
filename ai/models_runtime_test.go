package ai

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capture builds ProviderStreams whose Stream records the model + options it
// was handed and returns a closed stream.
func capture(gotModel **Model, gotOpts **StreamOptions) ProviderStreams {
	return ProviderStreams{
		Stream: func(_ context.Context, model *Model, _ TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
			*gotModel = model
			*gotOpts = opts
			s := NewAssistantMessageEventStream()
			s.End()
			return s
		},
	}
}

func TestCreateProviderDispatch(t *testing.T) {
	var gotA, gotB **Model = new(*Model), new(*Model)
	var optsA, optsB **StreamOptions = new(*StreamOptions), new(*StreamOptions)

	p := CreateProvider(CreateProviderOptions{
		ID:   "multi",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("multi", "X")},
		Models: []*Model{
			{Provider: "multi", ID: "a", Api: "api-a"},
			{Provider: "multi", ID: "b", Api: "api-b"},
		},
		APIByApi: map[Api]ProviderStreams{
			"api-a": capture(gotA, optsA),
			"api-b": capture(gotB, optsB),
		},
	})

	p.Stream(context.Background(), &Model{Provider: "multi", ID: "a", Api: "api-a"}, TranscriptContext{}, nil)
	if *gotA == nil || (*gotA).Api != "api-a" {
		t.Fatalf("api-a not dispatched, got %v", *gotA)
	}
	// A model whose api has no implementation yields a stream error.
	res := p.Stream(context.Background(), &Model{Provider: "multi", ID: "c", Api: "api-z"}, TranscriptContext{}, nil).Result()
	if res.StopReason != StopError {
		t.Fatalf("missing api should produce a stream error, got %v", res.StopReason)
	}
}

func TestModelsCollection(t *testing.T) {
	m := CreateModels(nil)
	pa := CreateProvider(CreateProviderOptions{ID: "a", Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("a", "A")}, Models: []*Model{{Provider: "a", ID: "m1"}}, API: stubAPI()})
	pb := CreateProvider(CreateProviderOptions{ID: "b", Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("b", "B")}, Models: []*Model{{Provider: "b", ID: "m2"}}, API: stubAPI()})
	m.SetProvider(pa)
	m.SetProvider(pb)

	if ps := m.GetProviders(); len(ps) != 2 || ps[0].ID() != "a" || ps[1].ID() != "b" {
		t.Fatalf("provider order wrong: %v", ps)
	}
	if m.GetProvider("b") == nil {
		t.Fatal("GetProvider(b) nil")
	}
	if all := m.GetModels(""); len(all) != 2 {
		t.Fatalf("GetModels(all) = %d, want 2", len(all))
	}
	if one := m.GetModels("a"); len(one) != 1 || one[0].ID != "m1" {
		t.Fatalf("GetModels(a) wrong: %v", one)
	}
	if m.GetModel("b", "m2") == nil || m.GetModel("b", "nope") != nil {
		t.Fatal("GetModel lookup wrong")
	}

	m.DeleteProvider("a")
	if m.GetProvider("a") != nil || len(m.GetProviders()) != 1 {
		t.Fatal("DeleteProvider failed")
	}
	m.ClearProviders()
	if len(m.GetProviders()) != 0 {
		t.Fatal("ClearProviders failed")
	}
}

func TestModelsApplyAuthEnvKey(t *testing.T) {
	t.Setenv("APPLY_KEY", "from-env")
	var gotModel *Model
	var gotOpts *StreamOptions
	m := CreateModels(nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("p", "APPLY_KEY")},
		Models: []*Model{{Provider: "p", ID: "m", Api: "api"}},
		API:    ptrStreams(capture(&gotModel, &gotOpts)),
	}))

	model := &Model{Provider: "p", ID: "m", Api: "api"}
	m.Stream(context.Background(), model, Context{}, nil)
	if gotOpts == nil || gotOpts.APIKey != "from-env" {
		t.Fatalf("auth key not applied: %+v", gotOpts)
	}
	// Explicit apiKey wins.
	m.Stream(context.Background(), model, Context{}, &ModelsStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: "explicit"}}})
	if gotOpts.APIKey != "explicit" {
		t.Fatalf("explicit key should win: %q", gotOpts.APIKey)
	}
}

func TestModelsApplyAuthBaseURLHeadersEnv(t *testing.T) {
	var gotModel *Model
	var gotOpts *StreamOptions
	auth := &ApiKeyAuth{
		Name: "custom",
		Resolve: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthResult, error) {
			return &AuthResult{
				Auth: ModelAuth{APIKey: "k", BaseURL: "https://auth.example", Headers: ProviderHeaders{"H": HeaderValue("auth"), "Keep": HeaderValue("auth")}},
				Env:  map[string]string{"E": "auth", "KeepEnv": "auth"},
			}, nil
		},
	}
	m := CreateModels(nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: auth},
		Models: []*Model{{Provider: "p", ID: "m", Api: "api"}},
		API:    ptrStreams(capture(&gotModel, &gotOpts)),
	}))

	model := &Model{Provider: "p", ID: "m", Api: "api", BaseURL: "https://model"}
	m.Stream(context.Background(), model, Context{}, &ModelsStreamOptions{StreamOptions: StreamOptions{
		ProviderRequestOptions: ProviderRequestOptions{Headers: ProviderHeaders{"H": HeaderValue("explicit")}, Env: map[string]string{"E": "explicit"}},
	}})
	if gotModel.BaseURL != "https://auth.example" {
		t.Errorf("auth baseURL should override: %q", gotModel.BaseURL)
	}
	if model.BaseURL != "https://model" {
		t.Errorf("original model must not be mutated: %q", model.BaseURL)
	}
	if headerValue(t, gotOpts.Headers, "H") != "explicit" || headerValue(t, gotOpts.Headers, "Keep") != "auth" {
		t.Errorf("header merge wrong (explicit wins per key): %v", gotOpts.Headers)
	}
	if gotOpts.Env["E"] != "explicit" || gotOpts.Env["KeepEnv"] != "auth" {
		t.Errorf("env merge wrong (explicit wins per key): %v", gotOpts.Env)
	}
}

// pi's lazyStream reports a setup failure as error.message, and a
// ModelsError's message carries no code: "Unknown provider: ghost", exactly.
func TestModelsUnknownProvider(t *testing.T) {
	m := CreateModels(nil)
	res := m.Stream(context.Background(), &Model{Provider: "ghost", ID: "x", Api: "api"}, Context{}, nil).Result()
	if res.StopReason != StopError {
		t.Fatalf("unknown provider should error, got %v", res.StopReason)
	}
	if res.ErrorMessage != "Unknown provider: ghost" {
		t.Fatalf("errorMessage = %q, want pi's %q", res.ErrorMessage, "Unknown provider: ghost")
	}
}

func TestModelsGetAuthUnconfigured(t *testing.T) {
	m := CreateModels(nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("p", "DEFINITELY_UNSET_KEY_XYZ")},
		Models: []*Model{{Provider: "p", ID: "m", Api: "api"}},
		API:    stubAPI(),
	}))
	res, err := m.GetAuth(context.Background(), &Model{Provider: "p", ID: "m", Api: "api"}, nil)
	if err != nil || res != nil {
		t.Fatalf("unconfigured provider should resolve (nil, nil), got (%+v, %v)", res, err)
	}
	// Unknown provider also resolves nil.
	if res, err := m.GetAuth(context.Background(), &Model{Provider: "ghost"}, nil); err != nil || res != nil {
		t.Fatalf("unknown provider GetAuth = (%+v, %v)", res, err)
	}
	// Streaming against an unconfigured provider is an error (ff28097a: the
	// pre-facade runtime passed the request through untouched).
	sres := m.Stream(context.Background(), &Model{Provider: "p", ID: "m", Api: "api"}, Context{}, nil).Result()
	if sres.StopReason != StopError || sres.ErrorMessage != "Provider is not configured: p" {
		t.Fatalf("unconfigured stream should error with pi's message, got %q / %q", sres.StopReason, sres.ErrorMessage)
	}
}

// fixedAuthContext returns a Models collection whose auth context reads from a
// fixed env map (no OS environment).
func modelsWithEnv(env map[string]string, opts *CreateModelsOptions) MutableModels {
	if opts == nil {
		opts = &CreateModelsOptions{}
	}
	opts.AuthContext = fakeAuthContext{env: env}
	return CreateModels(opts)
}

// TestModelsRefreshDynamic mirrors pi "refresh() updates every configured
// dynamic provider and reports failures": errors are collected per provider
// without failing the sweep, and successful providers still update.
func TestModelsRefreshDynamic(t *testing.T) {
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dyn",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dyn", "K")},
		FetchModels: func(_ context.Context, _ RefreshModelsContext) ([]*Model, error) {
			return []*Model{{Provider: "dyn", ID: "fetched"}}, nil
		},
		API: stubAPI(),
	}))
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "boom",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("boom", "K")},
		FetchModels: func(_ context.Context, _ RefreshModelsContext) ([]*Model, error) {
			return nil, errors.New("network")
		},
		API: stubAPI(),
	}))

	result := m.Refresh(context.Background(), nil)
	if result.Aborted {
		t.Fatal("refresh should not report aborted")
	}
	if got := m.GetModels("dyn"); len(got) != 1 || got[0].ID != "fetched" {
		t.Fatalf("refresh did not update model list: %v", got)
	}
	if err := result.Errors["boom"]; err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("boom's failure should be reported: %v", result.Errors)
	}
	if _, ok := result.Errors["dyn"]; ok {
		t.Fatal("dyn should not report an error")
	}
}

// TestModelsRefreshProviderContextIsComposite applies upstream 42a06f947 /
// d3da2e968's tightened composite assertion to the other cancellation
// composite in the runtime: the per-provider refresh context. beginProviderRefresh
// derives it from the caller's context so a supersede can cancel one provider's
// refresh alone, and that derivation must not cost the caller's own
// cancellation — the provider's context is distinct from the caller's, is
// cancelled with it, and reports the caller's cause.
func TestModelsRefreshProviderContextIsComposite(t *testing.T) {
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	reason := errors.New("caller went away")
	callerCtx, cancelCaller := context.WithCancelCause(context.Background())
	defer cancelCaller(nil)

	recorded := make(chan struct{})
	var distinct bool
	var fetchErr, fetchCause error
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dyn",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dyn", "K")},
		FetchModels: func(ctx context.Context, _ RefreshModelsContext) ([]*Model, error) {
			defer close(recorded)
			distinct = ctx != callerCtx
			cancelCaller(reason)
			<-ctx.Done()
			fetchErr, fetchCause = ctx.Err(), context.Cause(ctx)
			return nil, ctx.Err()
		},
		API: stubAPI(),
	}))

	m.Refresh(callerCtx, nil)
	<-recorded

	if !distinct {
		t.Fatal("provider refresh ran on the caller's own context, not a per-provider composite")
	}
	if !errors.Is(fetchErr, context.Canceled) {
		t.Fatalf("caller cancellation did not cancel the provider refresh context: %v", fetchErr)
	}
	if !errors.Is(fetchCause, reason) {
		t.Fatalf("caller's cancellation cause did not reach the provider refresh context: %v", fetchCause)
	}
}

// TestModelsRefreshPersistsAndRestores mirrors pi "persists dynamic catalogs
// and restores them without network access": a refresh writes through the
// ModelsStore, and a network-disabled refresh on a fresh collection restores
// the stored overlay without fetching.
func TestModelsRefreshPersistsAndRestores(t *testing.T) {
	store := NewInMemoryModelsStore()
	env := map[string]string{"K": "key"}
	fetched := []*Model{{Provider: "dyn", ID: "remote"}}
	newProvider := func(calls *int) Provider {
		return CreateProvider(CreateProviderOptions{
			ID:   "dyn",
			Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dyn", "K")},
			FetchModels: func(_ context.Context, _ RefreshModelsContext) ([]*Model, error) {
				*calls++
				return fetched, nil
			},
			API: stubAPI(),
		})
	}

	var calls int
	m := modelsWithEnv(env, &CreateModelsOptions{ModelsStore: store})
	m.SetProvider(newProvider(&calls))
	if res := m.Refresh(context.Background(), nil); len(res.Errors) != 0 || calls != 1 {
		t.Fatalf("initial refresh: errors=%v calls=%d", res.Errors, calls)
	}
	stored, _ := store.Read(context.Background(), "dyn")
	if stored == nil || len(stored.Models) != 1 || stored.Models[0].ID != "remote" {
		t.Fatalf("refresh must persist through the store: %v", stored)
	}
	if stored.CheckedAt == 0 {
		t.Fatal("a completed remote check must stamp checkedAt (bd9e09db)")
	}

	// A fresh collection sharing the store restores offline.
	var calls2 int
	m2 := modelsWithEnv(env, &CreateModelsOptions{ModelsStore: store})
	m2.SetProvider(newProvider(&calls2))
	allowNetwork := false
	res := m2.Refresh(context.Background(), &ModelsRefreshOptions{AllowNetwork: &allowNetwork})
	if len(res.Errors) != 0 || calls2 != 0 {
		t.Fatalf("offline refresh must not fetch: errors=%v calls=%d", res.Errors, calls2)
	}
	if got := m2.GetModels("dyn"); len(got) != 1 || got[0].ID != "remote" {
		t.Fatalf("offline refresh must restore the stored catalog: %v", got)
	}
}

// TestModelsRefreshSkipsUnconfigured mirrors pi "passes effective API-key
// credentials and skips unconfigured providers".
func TestModelsRefreshSkipsUnconfigured(t *testing.T) {
	var gotCredential *Credential
	m := modelsWithEnv(map[string]string{"CONFIGURED_KEY": "k-123"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "configured",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("configured", "CONFIGURED_KEY")},
		FetchModels: func(_ context.Context, req RefreshModelsContext) ([]*Model, error) {
			gotCredential = req.Credential
			return nil, nil
		},
		API: stubAPI(),
	}))
	skipped := false
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "unconfigured",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("unconfigured", "UNSET_KEY")},
		FetchModels: func(_ context.Context, _ RefreshModelsContext) ([]*Model, error) {
			skipped = true
			return nil, nil
		},
		API: stubAPI(),
	}))

	result := m.Refresh(context.Background(), nil)
	if len(result.Errors) != 0 {
		t.Fatalf("no errors expected: %v", result.Errors)
	}
	if gotCredential == nil || gotCredential.Type != CredentialAPIKey || gotCredential.Key != "k-123" {
		t.Fatalf("configured provider must receive the effective api-key credential: %+v", gotCredential)
	}
	if skipped {
		t.Fatal("unconfigured provider must be skipped, not fetched")
	}
}

// TestModelsRefreshOAuthBeforeModels mirrors pi "refreshes expired OAuth
// before refreshing models": an expired stored OAuth credential is exchanged
// (and persisted) before the provider fetch sees it.
func TestModelsRefreshOAuthBeforeModels(t *testing.T) {
	creds := NewInMemoryCredentialStore()
	_, _ = creds.Modify(context.Background(), "dyn", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1}, nil
	})
	m := modelsWithEnv(nil, &CreateModelsOptions{Credentials: creds})

	var gotAccess string
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "dyn",
		Auth: ProviderAuth{OAuth: &OAuthAuth{
			Name: "dyn",
			Refresh: func(_ context.Context, c OAuthCredentials) (OAuthCredentials, error) {
				return OAuthCredentials{Refresh: "r1", Access: "new", Expires: nowMillis() + 3_600_000}, nil
			},
			ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
		}},
		FetchModels: func(_ context.Context, req RefreshModelsContext) ([]*Model, error) {
			if req.Credential != nil {
				gotAccess = req.Credential.Access
			}
			return nil, nil
		},
		API: stubAPI(),
	}))

	if res := m.Refresh(context.Background(), nil); len(res.Errors) != 0 {
		t.Fatalf("refresh errors: %v", res.Errors)
	}
	if gotAccess != "new" {
		t.Fatalf("fetch must see the refreshed OAuth credential, got %q", gotAccess)
	}
	if stored, _ := creds.Read(context.Background(), "dyn"); stored == nil || stored.Access != "new" {
		t.Fatalf("rotated credential must be persisted: %+v", stored)
	}
}

// TestModelsErrorKeepsCause mirrors pi "keeps the underlying reason in wrapped
// oauth refresh errors" (upstream 4cf0a729): the cause's text rides in the
// message itself, after the wrapper's own words.
func TestModelsErrorKeepsCause(t *testing.T) {
	creds := NewInMemoryCredentialStore()
	_, _ = creds.Modify(context.Background(), "p1", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r", Access: "old", Expires: 0}, nil
	})
	m := modelsWithEnv(nil, &CreateModelsOptions{Credentials: creds})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "p1",
		Auth: ProviderAuth{OAuth: &OAuthAuth{
			Name: "p1",
			Refresh: func(context.Context, OAuthCredentials) (OAuthCredentials, error) {
				return OAuthCredentials{}, errors.New("token refresh failed (400): invalid_grant")
			},
			ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
		}},
		API: stubAPI(),
	}))

	_, err := m.GetAuth(context.Background(), &Model{Provider: "p1", ID: "m", Api: "api"}, nil)
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrOAuth {
		t.Fatalf("want an ErrOAuth ModelsError, got %v", err)
	}
	const want = "OAuth refresh failed for p1: token refresh failed (400): invalid_grant"
	if err.Error() != want {
		t.Fatalf("error = %q, want pi's %q", err.Error(), want)
	}
}

// ModelsError's text is pi's ModelsError.message (utils/models-error.ts
// withCauseDetail at a328aa89a): no code, and a cause's text appended only when
// it is non-empty after a JavaScript trim and not already in the message.
func TestModelsErrorMessageMatchesPi(t *testing.T) {
	feff := string(rune(0xFEFF)) // JS trims it; strings.TrimSpace does not
	nel := string(rune(0x85))    // strings.TrimSpace trims it; JS does not
	for _, tc := range []struct {
		name string
		err  *ModelsError
		want string
	}{
		{"no cause", newModelsError(ErrProvider, "Unknown provider: ghost", nil), "Unknown provider: ghost"},
		{"cause appended", newModelsError(ErrAuth, "API key auth failed for provider p", errors.New("nope")),
			"API key auth failed for provider p: nope"},
		{"cause trimmed", newModelsError(ErrAuth, "m", errors.New("  spaced\n")), "m: spaced"},
		{"blank cause", newModelsError(ErrAuth, "m", errors.New(" \t\n")), "m"},
		{"cause already in the message", newModelsError(ErrAuth, "refresh failed: invalid_grant", errors.New("invalid_grant")),
			"refresh failed: invalid_grant"},
		{"BOM is JS whitespace", newModelsError(ErrAuth, "m", errors.New(feff+"x"+feff)), "m: x"},
		{"NEL is not JS whitespace", newModelsError(ErrAuth, "m", errors.New(nel+"x")), "m: " + nel + "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestModelsRefreshAborted mirrors pi "returns aborted state without
// reporting cancellation as a provider error".
func TestModelsRefreshAborted(t *testing.T) {
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dyn",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dyn", "K")},
		FetchModels: func(fctx context.Context, _ RefreshModelsContext) ([]*Model, error) {
			cancel()
			return nil, fctx.Err()
		},
		API: stubAPI(),
	}))

	result := m.Refresh(ctx, nil)
	if !result.Aborted {
		t.Fatal("cancelled refresh must report aborted")
	}
	if len(result.Errors) != 0 {
		t.Fatalf("cancellation must not be reported as a provider error: %v", result.Errors)
	}
}

// TestModelsCheckAuthAndGetAvailable mirrors pi "checks provider auth without
// refreshing OAuth and filters available models".
func TestModelsCheckAuthAndGetAvailable(t *testing.T) {
	creds := NewInMemoryCredentialStore()
	// Expired OAuth: checkAuth must NOT refresh it.
	_, _ = creds.Modify(context.Background(), "oauthp", func(*Credential) (*Credential, error) {
		return &Credential{
			Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1,
			AvailableModelIDs: []string{"kept"},
		}, nil
	})
	m := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{Credentials: creds})

	refreshed := false
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "oauthp",
		Auth: ProviderAuth{OAuth: &OAuthAuth{
			Name: "oauthp",
			Refresh: func(_ context.Context, c OAuthCredentials) (OAuthCredentials, error) {
				refreshed = true
				return c, nil
			},
			ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
		}},
		Models: []*Model{
			{Provider: "oauthp", ID: "kept"},
			{Provider: "oauthp", ID: "dropped"},
		},
		FilterModels: func(models []*Model, credential *Credential) []*Model {
			if credential == nil || credential.AvailableModelIDs == nil {
				return models
			}
			available := map[string]bool{}
			for _, id := range credential.AvailableModelIDs {
				available[id] = true
			}
			var out []*Model
			for _, model := range models {
				if available[model.ID] {
					out = append(out, model)
				}
			}
			return out
		},
		API: stubAPI(),
	}))
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "unconfigured",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("unconfigured", "UNSET_KEY")},
		Models: []*Model{{Provider: "unconfigured", ID: "hidden"}},
		API:    stubAPI(),
	}))
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "keyed",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("keyed", "K")},
		Models: []*Model{{Provider: "keyed", ID: "visible"}},
		API:    stubAPI(),
	}))

	check, err := m.CheckAuth(context.Background(), "oauthp")
	if err != nil || check == nil || check.Type != CredentialOAuth || check.Source != "OAuth" {
		t.Fatalf("oauth checkAuth = (%+v, %v)", check, err)
	}
	if refreshed {
		t.Fatal("checkAuth must not refresh OAuth")
	}
	if check, _ := m.CheckAuth(context.Background(), "unconfigured"); check != nil {
		t.Fatalf("unconfigured checkAuth should be nil, got %+v", check)
	}
	if check, _ := m.CheckAuth(context.Background(), "keyed"); check == nil || check.Type != CredentialAPIKey || check.Source != "K" {
		t.Fatalf("keyed checkAuth wrong: %+v", check)
	}
	if check, _ := m.CheckAuth(context.Background(), "ghost"); check != nil {
		t.Fatal("unknown provider checkAuth should be nil")
	}

	available, err := m.GetAvailable(context.Background(), "")
	if err != nil {
		t.Fatalf("getAvailable: %v", err)
	}
	var ids []string
	for _, model := range available {
		ids = append(ids, model.Provider+"/"+model.ID)
	}
	want := []string{"oauthp/kept", "keyed/visible"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("available = %v, want %v", ids, want)
	}
}

// TestModelsCheckAuthUsesCheckHook locks pi ApiKeyAuth.check: when present it
// is preferred over resolving auth, and its failures wrap as ModelsError.
func TestModelsCheckAuthUsesCheckHook(t *testing.T) {
	resolved := false
	m := modelsWithEnv(nil, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "cmd",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "cmd",
			Check: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthCheck, error) {
				return &AuthCheck{Source: "command", Type: CredentialAPIKey}, nil
			},
			Resolve: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthResult, error) {
				resolved = true
				return &AuthResult{Auth: ModelAuth{APIKey: "side-effect"}}, nil
			},
		}},
		API: stubAPI(),
	}))

	check, err := m.CheckAuth(context.Background(), "cmd")
	if err != nil || check == nil || check.Source != "command" {
		t.Fatalf("check hook result wrong: (%+v, %v)", check, err)
	}
	if resolved {
		t.Fatal("check hook must prevent side-effectful resolve")
	}

	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "bad",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "bad",
			Check: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthCheck, error) {
				return nil, errors.New("exec failed")
			},
		}},
		API: stubAPI(),
	}))
	_, err = m.CheckAuth(context.Background(), "bad")
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrAuth || !strings.Contains(me.Message, "API key auth check failed for provider bad") {
		t.Fatalf("check failure should wrap as ModelsError auth: %v", err)
	}
}

// fakeInteraction answers every prompt with a fixed string.
type fakeInteraction struct{ answer string }

func (f fakeInteraction) Prompt(AuthPrompt) (string, error) { return f.answer, nil }
func (f fakeInteraction) Notify(AuthEvent)                  {}

// Upstream 02eed88fd: Models.login hands its LoginOptions to the provider's
// login flow, which is how Sign in with ChatGPT gets the app's device ID.
func TestModelsLoginForwardsLoginOptions(t *testing.T) {
	var got *LoginOptions
	m := modelsWithEnv(nil, &CreateModelsOptions{Credentials: NewInMemoryCredentialStore()})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "p",
		Auth: ProviderAuth{OAuth: &OAuthAuth{
			Login: func(_ context.Context, _ AuthInteraction, options *LoginOptions) (*Credential, error) {
				got = options
				return &Credential{Type: CredentialOAuth, Access: "a", Refresh: "r"}, nil
			},
		}},
		API: stubAPI(),
	}))
	options := &LoginOptions{GetDeviceID: func() string { return "device-1" }}
	if _, err := m.Login(context.Background(), "p", CredentialOAuth, fakeInteraction{}, options); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.GetDeviceID == nil || got.GetDeviceID() != "device-1" {
		t.Fatalf("the OAuth login flow received %+v, want the caller's options", got)
	}
}

// TestModelsLoginLogout mirrors pi "runs provider login and logout through
// the credential store".
func TestModelsLoginLogout(t *testing.T) {
	creds := NewInMemoryCredentialStore()
	m := modelsWithEnv(nil, &CreateModelsOptions{Credentials: creds})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "p",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("p", "P_KEY")},
		API:  stubAPI(),
	}))

	credential, err := m.Login(context.Background(), "p", CredentialAPIKey, fakeInteraction{answer: "typed-key"}, nil)
	if err != nil || credential == nil || credential.Key != "typed-key" {
		t.Fatalf("login = (%+v, %v)", credential, err)
	}
	if stored, _ := creds.Read(context.Background(), "p"); stored == nil || stored.Key != "typed-key" {
		t.Fatalf("login must persist the credential: %+v", stored)
	}

	// Unsupported login type errors with pi's message.
	_, err = m.Login(context.Background(), "p", CredentialOAuth, fakeInteraction{}, nil)
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrAuth || !strings.Contains(me.Message, "does not support oauth login") {
		t.Fatalf("unsupported login should error: %v", err)
	}
	_, err = m.Login(context.Background(), "ghost", CredentialAPIKey, fakeInteraction{}, nil)
	if !errors.As(err, &me) || me.Code != ErrProvider {
		t.Fatalf("unknown provider login should error: %v", err)
	}

	// An OAuth method without a login flow is pi's `!method?.login`: the same
	// error, not a call through a nil function.
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "no-flow",
		Auth: ProviderAuth{OAuth: &OAuthAuth{Name: "No flow"}},
		API:  stubAPI(),
	}))
	_, err = m.Login(context.Background(), "no-flow", CredentialOAuth, fakeInteraction{}, nil)
	if !errors.As(err, &me) || me.Code != ErrAuth || !strings.Contains(me.Message, "does not support oauth login") {
		t.Fatalf("an OAuth method without Login should refuse: %v", err)
	}

	if err := m.Logout(context.Background(), "p"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if stored, _ := creds.Read(context.Background(), "p"); stored != nil {
		t.Fatalf("logout must delete the credential: %+v", stored)
	}
}

// TestModelsModelHeadersAndTransform mirrors pi "adds model headers only for
// model auth and transforms assembled headers once": GetAuth(model) merges the
// model's static headers case-insensitively; GetProviderAuth does not; the
// Models-only TransformHeaders hook runs on the fully assembled headers and is
// stripped before provider dispatch.
func TestModelsModelHeadersAndTransform(t *testing.T) {
	var gotModel *Model
	var gotOpts *StreamOptions
	m := modelsWithEnv(nil, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "p",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "p",
			Resolve: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthResult, error) {
				return &AuthResult{Auth: ModelAuth{APIKey: "k", Headers: ProviderHeaders{"X-Auth": HeaderValue("auth"), "Keep": HeaderValue("auth")}}}, nil
			},
		}},
		Models: []*Model{{Provider: "p", ID: "m", Api: "api"}},
		API:    ptrStreams(capture(&gotModel, &gotOpts)),
	}))
	model := &Model{
		Provider: "p", ID: "m", Api: "api",
		Headers: ProviderHeaders{"x-auth": HeaderValue("model")},
	}

	// Model auth merges model headers, replacing case-insensitive matches.
	res, err := m.GetAuth(context.Background(), model, nil)
	if err != nil || res == nil {
		t.Fatalf("getAuth(model) = (%+v, %v)", res, err)
	}
	if headerValue(t, res.Auth.Headers, "x-auth") != "model" || headerValue(t, res.Auth.Headers, "Keep") != "auth" {
		t.Fatalf("model header merge wrong: %v", res.Auth.Headers)
	}
	if _, stale := res.Auth.Headers["X-Auth"]; stale {
		t.Fatalf("case-insensitive replace must drop the stale key: %v", res.Auth.Headers)
	}

	// Provider auth carries no model headers.
	pres, err := m.GetProviderAuth(context.Background(), "p", nil)
	if err != nil || pres == nil {
		t.Fatalf("getProviderAuth = (%+v, %v)", pres, err)
	}
	if headerValue(t, pres.Auth.Headers, "X-Auth") != "auth" {
		t.Fatalf("provider auth must not merge model headers: %v", pres.Auth.Headers)
	}

	// TransformHeaders sees the assembled headers exactly once.
	transformCalls := 0
	m.Stream(context.Background(), model, Context{}, &ModelsStreamOptions{
		StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Headers: ProviderHeaders{"Req": HeaderValue("1")}}},
		ModelsRequestTransforms: ModelsRequestTransforms{
			TransformHeaders: func(headers ProviderHeaders) (ProviderHeaders, error) {
				transformCalls++
				headers["Transformed"] = HeaderValue("yes")
				return headers, nil
			},
		},
	})
	if transformCalls != 1 {
		t.Fatalf("transform must run exactly once, ran %d", transformCalls)
	}
	if headerValue(t, gotOpts.Headers, "Transformed") != "yes" || headerValue(t, gotOpts.Headers, "Req") != "1" ||
		headerValue(t, gotOpts.Headers, "x-auth") != "model" {
		t.Fatalf("transformed headers not dispatched: %v", gotOpts.Headers)
	}
}

// TestCreateProviderDynamicOverlay locks pi createProvider's currentModels
// merge: a dynamic model replaces the baseline entry with its id, new ids
// append, and the baseline survives a cleared overlay.
func TestCreateProviderDynamicOverlay(t *testing.T) {
	fetched := []*Model{
		{Provider: "p", ID: "base", Name: "replaced"},
		{Provider: "p", ID: "extra"},
		{Provider: "other", ID: "foreign"}, // filtered on restore only
	}
	p := CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("p", "K")},
		Models: []*Model{{Provider: "p", ID: "base", Name: "baseline"}, {Provider: "p", ID: "static"}},
		FetchModels: func(_ context.Context, _ RefreshModelsContext) ([]*Model, error) {
			return fetched, nil
		},
		API: stubAPI(),
	})
	if !p.DynamicModels() {
		t.Fatal("provider with FetchModels must report DynamicModels")
	}

	store := NewInMemoryModelsStore()
	m := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{ModelsStore: store})
	m.SetProvider(p)
	if res := m.Refresh(context.Background(), nil); len(res.Errors) != 0 {
		t.Fatalf("refresh: %v", res.Errors)
	}
	got := p.GetModels()
	var ids []string
	for _, model := range got {
		ids = append(ids, model.ID)
	}
	// base is replaced in place, static kept, extra + foreign appended
	// (pi's fetch path stores the fetched overlay verbatim).
	want := []string{"base", "static", "extra", "foreign"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("merged models = %v, want %v", ids, want)
	}
	if got[0].Name != "replaced" {
		t.Fatalf("dynamic model must replace the baseline entry: %+v", got[0])
	}

	// Restore path filters foreign-provider entries (pi: stored.filter by id).
	p2 := CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("p", "K")},
		Models: []*Model{{Provider: "p", ID: "static"}},
		FetchModels: func(_ context.Context, _ RefreshModelsContext) ([]*Model, error) {
			return nil, errors.New("offline")
		},
		API: stubAPI(),
	})
	m2 := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{ModelsStore: store})
	m2.SetProvider(p2)
	allowNetwork := false
	if res := m2.Refresh(context.Background(), &ModelsRefreshOptions{AllowNetwork: &allowNetwork}); len(res.Errors) != 0 {
		t.Fatalf("offline refresh must not fetch: %v", res.Errors)
	}
	ids = nil
	for _, model := range p2.GetModels() {
		ids = append(ids, model.ID)
	}
	if !reflect.DeepEqual(ids, []string{"static", "base", "extra"}) {
		t.Fatalf("restored models = %v (foreign must be filtered)", ids)
	}
}

func ptrStreams(s ProviderStreams) *ProviderStreams { return &s }

// TestModelsRefreshForce locks pi 97f9978f: ModelsRefreshOptions.Force threads
// into the provider's RefreshModelsContext so fetch implementations can bypass
// freshness checks — but the best-effort cache-restore call after a failure
// deliberately stays force-free (matching pi's error path).
func TestModelsRefreshForce(t *testing.T) {
	var forces []bool
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dyn",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dyn", "K")},
		FetchModels: func(_ context.Context, req RefreshModelsContext) ([]*Model, error) {
			forces = append(forces, req.Force)
			return nil, nil
		},
		API: stubAPI(),
	}))

	m.Refresh(context.Background(), &ModelsRefreshOptions{Force: true})
	m.Refresh(context.Background(), nil)
	if len(forces) != 2 || !forces[0] || forces[1] {
		t.Fatalf("force threading wrong: %v (want [true false])", forces)
	}

	// Failure path: the main call carries force, the cache-restore does not.
	var restoreForces []bool
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "boom",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("boom", "K")},
		FetchModels: func(_ context.Context, req RefreshModelsContext) ([]*Model, error) {
			if req.AllowNetwork {
				restoreForces = append(restoreForces, req.Force)
				return nil, errors.New("network")
			}
			// cache-restore call (AllowNetwork false) never reaches fetch;
			// record via the context should it ever fetch.
			restoreForces = append(restoreForces, req.Force)
			return nil, nil
		},
		API: stubAPI(),
	}))
	res := m.Refresh(context.Background(), &ModelsRefreshOptions{Force: true})
	if res.Errors["boom"] == nil {
		t.Fatalf("boom must fail: %v", res.Errors)
	}
	if len(restoreForces) < 1 || !restoreForces[0] {
		t.Fatalf("main call must carry force: %v", restoreForces)
	}
	if len(restoreForces) > 1 {
		t.Fatalf("cache-restore must not fetch (AllowNetwork false): %v", restoreForces)
	}
}

// recordingModelsStore records the context each operation was handed, so tests
// can assert that storage work is bound to the refresh's own context.
type recordingModelsStore struct {
	inner ModelsStore

	mu   sync.Mutex
	ctxs []context.Context
}

func (s *recordingModelsStore) record(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxs = append(s.ctxs, ctx)
}

func (s *recordingModelsStore) seen() []context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]context.Context, len(s.ctxs))
	copy(out, s.ctxs)
	return out
}

func (s *recordingModelsStore) Read(ctx context.Context, providerID string) (*ModelsStoreEntry, error) {
	s.record(ctx)
	return s.inner.Read(ctx, providerID)
}

func (s *recordingModelsStore) Write(ctx context.Context, providerID string, entry ModelsStoreEntry) error {
	s.record(ctx)
	return s.inner.Write(ctx, providerID, entry)
}

func (s *recordingModelsStore) Delete(ctx context.Context, providerID string) error {
	s.record(ctx)
	return s.inner.Delete(ctx, providerID)
}

// TestModelsRefreshSelectedProviders mirrors pi "restricts refresh work to
// selected providers" (upstream fed6009c): Providers narrows the sweep, unknown
// ids are ignored, and each selected provider runs a cache phase before its
// network phase.
func TestModelsRefreshSelectedProviders(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	// Record from the RefreshModels seam rather than FetchModels, so the
	// cache-only phase is observable too.
	for _, id := range []string{"one", "two"} {
		m.SetProvider(recordingRefreshProvider(id, &mu, &calls))
	}

	result := m.Refresh(context.Background(), &ModelsRefreshOptions{Providers: []string{"two", "unknown"}})

	if len(result.Errors) != 0 {
		t.Fatalf("no errors expected: %v", result.Errors)
	}
	if !reflect.DeepEqual(calls, []string{"two:cache", "two:network"}) {
		t.Fatalf("refresh calls = %v, want [two:cache two:network]", calls)
	}
}

// refreshPhaseProvider is a dynamic provider that records each refresh phase and
// the context it was handed.
type refreshPhaseProvider struct {
	Provider
	id     string
	mu     *sync.Mutex
	calls  *[]string
	seenMu sync.Mutex
	seen   []context.Context
}

func recordingRefreshProvider(id string, mu *sync.Mutex, calls *[]string) *refreshPhaseProvider {
	return &refreshPhaseProvider{
		Provider: CreateProvider(CreateProviderOptions{
			ID:          id,
			Auth:        ProviderAuth{APIKey: EnvAPIKeyAuth(id, "K")},
			FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) { return nil, nil },
			API:         stubAPI(),
		}),
		id:    id,
		mu:    mu,
		calls: calls,
	}
}

func (p *refreshPhaseProvider) RefreshModels(ctx context.Context, req RefreshModelsContext) error {
	phase := "cache"
	if req.AllowNetwork {
		phase = "network"
	}
	p.mu.Lock()
	*p.calls = append(*p.calls, p.id+":"+phase)
	p.mu.Unlock()
	p.seenMu.Lock()
	p.seen = append(p.seen, ctx)
	p.seenMu.Unlock()
	return p.Provider.RefreshModels(ctx, req)
}

func (p *refreshPhaseProvider) contexts() []context.Context {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	out := make([]context.Context, len(p.seen))
	copy(out, p.seen)
	return out
}

// TestModelsRefreshRestoresCacheBeforeAuth mirrors pi "restores cached models
// before waiting for network auth": the cache-only phase publishes the stored
// catalog before auth resolution is even attempted, so a refresh blocked in
// auth still leaves the last-known models visible — and cancelling it returns
// without ever reaching the network fetch.
func TestModelsRefreshRestoresCacheBeforeAuth(t *testing.T) {
	store := NewInMemoryModelsStore()
	if err := store.Write(context.Background(), "dynamic", ModelsStoreEntry{
		Models: []*Model{{Provider: "dynamic", ID: "cached"}},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	authStarted := make(chan struct{})
	releaseAuth := make(chan struct{})
	authReturned := make(chan struct{})
	fetched := false

	m := modelsWithEnv(nil, &CreateModelsOptions{ModelsStore: store})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "dynamic",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "Blocked auth",
			Resolve: func(context.Context, AuthContext, *Credential) (*AuthResult, error) {
				close(authStarted)
				<-releaseAuth
				close(authReturned)
				return &AuthResult{Auth: ModelAuth{APIKey: "key"}}, nil
			},
		}},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			fetched = true
			return nil, errors.New("must not fetch")
		},
		API: stubAPI(),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan ModelsRefreshResult, 1)
	go func() { results <- m.Refresh(ctx, &ModelsRefreshOptions{Providers: []string{"dynamic"}}) }()
	<-authStarted

	if m.GetModel("dynamic", "cached") == nil {
		t.Fatal("the cache phase must restore the stored catalog before auth resolution")
	}
	cancel()
	if result := <-results; !result.Aborted {
		t.Fatalf("cancelled refresh must report aborted: %+v", result)
	}
	close(releaseAuth)
	<-authReturned
	if fetched {
		t.Fatal("a cancelled refresh must not reach the network fetch")
	}
}

// TestModelsPublishPersistenceChoices mirrors pi "lets providers choose
// persistent deletion and ephemeral publication atomically": a provider may
// delete its stored entry, and a publication without a persistence choice
// applies only the in-memory update — with the update always running after the
// storage mutation it was published with.
func TestModelsPublishPersistenceChoices(t *testing.T) {
	store := NewInMemoryModelsStore()
	if err := store.Write(context.Background(), "dynamic", ModelsStoreEntry{
		Models: []*Model{{Provider: "dynamic", ID: "stored"}},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	state := "initial"
	var publishErr error

	m := modelsWithEnv(nil, &CreateModelsOptions{ModelsStore: store})
	m.SetProvider(&publishingProvider{
		Provider: CreateProvider(CreateProviderOptions{
			ID:          "dynamic",
			Auth:        ProviderAuth{APIKey: EnvAPIKeyAuth("dynamic")},
			FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) { return nil, nil },
			API:         stubAPI(),
		}),
		refresh: func(_ context.Context, req RefreshModelsContext) error {
			if req.Stored == nil || len(req.Stored.Models) != 1 || req.Stored.Models[0].ID != "stored" {
				return errors.New("provider must see the stored snapshot")
			}
			if _, err := req.Publish(ModelsPublication{
				DeletePersisted: true,
				Update: func() {
					if entry, _ := store.Read(context.Background(), "dynamic"); entry != nil {
						publishErr = errors.New(
							"the stored entry must already be deleted when the update runs (deletion missing or ordered after it)")
					}
					state = "deleted"
				},
			}); err != nil {
				return err
			}
			_, err := req.Publish(ModelsPublication{Update: func() { state = "ephemeral" }})
			return err
		},
	})

	allowNetwork := false
	result := m.Refresh(context.Background(), &ModelsRefreshOptions{AllowNetwork: &allowNetwork})
	if len(result.Errors) != 0 {
		t.Fatalf("no errors expected: %v", result.Errors)
	}
	if publishErr != nil {
		t.Fatal(publishErr)
	}
	if entry, _ := store.Read(context.Background(), "dynamic"); entry != nil {
		t.Fatalf("persist deletion must clear the stored entry: %+v", entry)
	}
	if state != "ephemeral" {
		t.Fatalf("state = %q, want ephemeral", state)
	}
}

// publishingProvider overrides RefreshModels with a test-supplied body.
type publishingProvider struct {
	Provider
	refresh func(ctx context.Context, req RefreshModelsContext) error
}

func (p *publishingProvider) RefreshModels(ctx context.Context, req RefreshModelsContext) error {
	return p.refresh(ctx, req)
}

// TestModelsRefreshBindsStorageToRefreshContext mirrors pi "binds model-store
// waits to the provider refresh signal": every ModelsStore operation a refresh
// performs — both phase reads and the publication write — carries the same
// context the provider was handed, so cancelling the refresh cancels its
// storage work too.
func TestModelsRefreshBindsStorageToRefreshContext(t *testing.T) {
	store := &recordingModelsStore{inner: NewInMemoryModelsStore()}
	var mu sync.Mutex
	var calls []string
	provider := recordingRefreshProvider("dynamic", &mu, &calls)
	m := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{ModelsStore: store})
	m.SetProvider(provider)

	result := m.Refresh(context.Background(), &ModelsRefreshOptions{Providers: []string{"dynamic"}})
	if len(result.Errors) != 0 {
		t.Fatalf("no errors expected: %v", result.Errors)
	}

	providerContexts := provider.contexts()
	if len(providerContexts) != 2 {
		t.Fatalf("want a cache and a network phase, got %d", len(providerContexts))
	}
	refreshCtx := providerContexts[0]
	if providerContexts[1] != refreshCtx {
		t.Fatal("both refresh phases must share one refresh context")
	}
	seen := store.seen()
	if len(seen) != 3 {
		t.Fatalf("want three storage operations (two reads, one write), got %d", len(seen))
	}
	for i, storeCtx := range seen {
		if storeCtx != refreshCtx {
			t.Fatalf("storage operation %d used a foreign context", i)
		}
	}
}

// TestModelsRefreshStopsWaitingOnCancel mirrors pi "stops waiting on abort when
// a provider ignores its signal": the sweep returns as soon as its context is
// done instead of waiting for a stalled provider, and the returned error map is
// a snapshot the abandoned work can no longer touch.
func TestModelsRefreshStopsWaitingOnCancel(t *testing.T) {
	release := make(chan struct{})
	failed := make(chan struct{})
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dynamic",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dynamic", "K")},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			close(failed)
			<-release
			return nil, errors.New("late provider failure")
		},
		API: stubAPI(),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan ModelsRefreshResult, 1)
	go func() { results <- m.Refresh(ctx, nil) }()
	<-failed
	cancel()

	var result ModelsRefreshResult
	select {
	case result = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled sweep must return without waiting for a stalled provider")
	}
	if !result.Aborted {
		t.Fatal("a cancelled sweep must report aborted")
	}
	if len(result.Errors) != 0 {
		t.Fatalf("cancellation must not be reported as a provider error: %v", result.Errors)
	}
	close(release)
	// The abandoned provider's late failure cannot appear in the snapshot the
	// caller already holds.
	if len(result.Errors) != 0 {
		t.Fatalf("late failure leaked into the returned result: %v", result.Errors)
	}
}

// TestModelsRefreshSupersedesInFlightRefresh mirrors pi "rejects late
// publication from a superseded non-cooperative provider" and "lets a newer
// dynamic refresh bypass and supersede older network work": a second refresh
// does not join the first (pi's inflightRefresh dedup is gone), and the older
// refresh's late result never clobbers the newer catalog.
func TestModelsRefreshSupersedesInFlightRefresh(t *testing.T) {
	store := NewInMemoryModelsStore()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	var fetches atomic.Int64

	m := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{ModelsStore: store})
	provider := CreateProvider(CreateProviderOptions{
		ID:   "dynamic",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dynamic", "K")},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			current := fetches.Add(1)
			if current == 1 {
				close(firstStarted)
				<-releaseFirst
				defer close(firstDone)
			}
			return []*Model{{Provider: "dynamic", ID: fmt.Sprintf("generation-%d", current)}}, nil
		},
		API: stubAPI(),
	})
	m.SetProvider(provider)

	firstResults := make(chan ModelsRefreshResult, 1)
	go func() {
		firstResults <- m.Refresh(context.Background(), &ModelsRefreshOptions{Providers: []string{"dynamic"}})
	}()
	<-firstStarted

	second := m.Refresh(context.Background(), &ModelsRefreshOptions{Providers: []string{"dynamic"}})
	if len(second.Errors) != 0 || second.Aborted {
		t.Fatalf("the superseding refresh must succeed: %+v", second)
	}
	close(releaseFirst)
	if first := <-firstResults; len(first.Errors) != 0 {
		t.Fatalf("a superseded refresh must not report an error: %v", first.Errors)
	}
	<-firstDone

	if got := fetches.Load(); got != 2 {
		t.Fatalf("a newer refresh must fetch instead of joining the older one, fetches=%d", got)
	}
	ids := []string{}
	for _, model := range provider.GetModels() {
		ids = append(ids, model.ID)
	}
	if !reflect.DeepEqual(ids, []string{"generation-2"}) {
		t.Fatalf("models = %v, want [generation-2]", ids)
	}
	stored, _ := store.Read(context.Background(), "dynamic")
	if stored == nil || len(stored.Models) != 1 || stored.Models[0].ID != "generation-2" {
		t.Fatalf("the superseded refresh must not publish over the newer catalog: %+v", stored)
	}
}

// TestModelsSetProviderSupersedesRefresh locks pi's setProvider/deleteProvider/
// clearProviders superseding: replacing a provider cancels the refresh in
// flight for that id, and the abandoned refresh publishes nothing.
func TestModelsSetProviderSupersedesRefresh(t *testing.T) {
	store := NewInMemoryModelsStore()
	started := make(chan struct{})
	release := make(chan struct{})
	cancelled := make(chan error, 1)

	m := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{ModelsStore: store})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dynamic",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dynamic", "K")},
		FetchModels: func(ctx context.Context, req RefreshModelsContext) ([]*Model, error) {
			close(started)
			<-release
			_, err := req.Publish(ModelsPublication{
				Persist: &ModelsStoreEntry{Models: []*Model{{Provider: "dynamic", ID: "stale"}}},
			})
			cancelled <- err
			return nil, ctx.Err()
		},
		API: stubAPI(),
	}))

	go m.Refresh(context.Background(), nil)
	<-started
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dynamic",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dynamic", "K")},
		API:  stubAPI(),
	}))
	close(release)

	if err := <-cancelled; err == nil {
		t.Fatal("publication from a superseded refresh must fail")
	}
	if stored, _ := store.Read(context.Background(), "dynamic"); stored != nil {
		t.Fatalf("a superseded refresh must not persist: %+v", stored)
	}
}

type ctxMarkerKey struct{}

// TestModelsAuthCallbacksSeeCallerContext mirrors pi "passes caller signals to
// provider auth callbacks": CheckAuth, GetProviderAuth, and Login hand the
// caller's own context to the provider's check/resolve/login.
func TestModelsAuthCallbacksSeeCallerContext(t *testing.T) {
	var received []string
	record := func(ctx context.Context) {
		marker, _ := ctx.Value(ctxMarkerKey{}).(string)
		received = append(received, marker)
	}
	m := modelsWithEnv(nil, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "p1",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "Context auth",
			Login: func(ctx context.Context, _ AuthInteraction) (*Credential, error) {
				record(ctx)
				return &Credential{Type: CredentialAPIKey, Key: "saved"}, nil
			},
			Check: func(ctx context.Context, _ AuthContext, _ *Credential) (*AuthCheck, error) {
				record(ctx)
				return &AuthCheck{Type: CredentialAPIKey}, nil
			},
			Resolve: func(ctx context.Context, _ AuthContext, _ *Credential) (*AuthResult, error) {
				record(ctx)
				return &AuthResult{Auth: ModelAuth{APIKey: "resolved"}}, nil
			},
		}},
		API: stubAPI(),
	}))

	ctx := context.WithValue(context.Background(), ctxMarkerKey{}, "caller")
	if _, err := m.CheckAuth(ctx, "p1"); err != nil {
		t.Fatalf("checkAuth: %v", err)
	}
	if _, err := m.GetProviderAuth(ctx, "p1", nil); err != nil {
		t.Fatalf("getAuth: %v", err)
	}
	if _, err := m.Login(ctx, "p1", CredentialAPIKey, fakeInteraction{answer: "unused"}, nil); err != nil {
		t.Fatalf("login: %v", err)
	}

	if !reflect.DeepEqual(received, []string{"caller", "caller", "caller"}) {
		t.Fatalf("auth callbacks did not receive the caller's context: %v", received)
	}
}

// TestModelsGetAuthCancelledOAuthRefreshPreservesCredential mirrors pi "passes
// cancellation to OAuth refresh and preserves the previous credential": the
// refresh sees the caller's context, and a refresh that ignores cancellation
// and returns late must not have its result written to the store.
//
// Upstream d3da2e968 tightened what "sees the caller's context" means: the
// refresh gets a distinct composite signal, not the caller's own object, and
// that composite both aborts with the caller and reports the caller's abort
// reason. Go's equivalent of the reason is the cancellation cause, which
// propagates to every derived context — so the refresh context must report the
// caller's cause, not a bare context.Canceled.
func TestModelsGetAuthCancelledOAuthRefreshPreservesCredential(t *testing.T) {
	creds := NewInMemoryCredentialStore()
	previous := &Credential{Type: CredentialOAuth, Refresh: "old-refresh", Access: "old", Expires: 0}
	if _, err := creds.Modify(context.Background(), "p1", func(*Credential) (*Credential, error) {
		return previous, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	refreshStarted := make(chan struct{})
	release := make(chan struct{})
	var marker string
	var refreshCtx context.Context
	m := modelsWithEnv(nil, &CreateModelsOptions{Credentials: creds})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "p1",
		Auth: ProviderAuth{OAuth: &OAuthAuth{
			Name: "p1",
			Refresh: func(ctx context.Context, c OAuthCredentials) (OAuthCredentials, error) {
				marker, _ = ctx.Value(ctxMarkerKey{}).(string)
				refreshCtx = ctx
				close(refreshStarted)
				<-release
				// Deliberately ignores cancellation and succeeds late.
				return OAuthCredentials{Refresh: "rotated", Access: "new", Expires: nowMillis() + 60_000}, nil
			},
			ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{APIKey: c.Access}, nil },
		}},
		API: stubAPI(),
	}))

	ctx, cancel := context.WithCancelCause(context.WithValue(context.Background(), ctxMarkerKey{}, "caller"))
	reason := errors.New("caller went away")
	errs := make(chan error, 1)
	go func() {
		_, err := m.GetProviderAuth(ctx, "p1", nil)
		errs <- err
	}()
	<-refreshStarted
	cancel(reason)
	close(release)

	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolution must surface the cancellation, got %v", err)
	}
	if marker != "caller" {
		t.Fatalf("OAuth refresh must receive the caller's context, marker=%q", marker)
	}
	if refreshCtx == ctx {
		t.Fatal("refresh must receive a composite derived from the caller's context, not the caller's own")
	}
	if !errors.Is(context.Cause(refreshCtx), reason) {
		t.Fatalf("caller's cancellation cause must reach the refresh context, got %v", context.Cause(refreshCtx))
	}
	stored, _ := creds.Read(context.Background(), "p1")
	if stored == nil || stored.Access != "old" || stored.Refresh != "old-refresh" {
		t.Fatalf("a cancelled refresh must leave the stored credential untouched: %+v", stored)
	}
}

// deferredStreams builds ProviderStreams that redeem a handle with a fixed
// text response and record every cancellation, with the request options the
// cancel was dispatched with.
func deferredStreams(cancelled *[]DeferredHandle, cancelOpts *[]*DeferredCancelOptions) ProviderStreams {
	return ProviderStreams{
		Stream: func(_ context.Context, _ *Model, _ TranscriptContext, _ *StreamOptions) *AssistantMessageEventStream {
			s := NewAssistantMessageEventStream()
			s.End()
			return s
		},
		FetchDeferred: func(_ context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
			s := NewAssistantMessageEventStream()
			msg := &AssistantMessage{
				Content:    ContentList{TextContent{Text: handle.ID + ":" + opts.APIKey}},
				Api:        model.Api,
				Provider:   model.Provider,
				Model:      model.ID,
				StopReason: StopStop,
			}
			s.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: msg})
			s.End()
			return s
		},
		CancelDeferred: func(_ context.Context, _ *Model, handle DeferredHandle, opts *DeferredCancelOptions) error {
			*cancelled = append(*cancelled, handle)
			if cancelOpts != nil {
				*cancelOpts = append(*cancelOpts, opts)
			}
			return nil
		},
	}
}

// TestCreateProviderAnnouncesDeferredCapabilities locks pi 382aa641c: a
// provider exposes fetchDeferred/cancelDeferred only when some underlying api
// implements them, which in Go is a type assertion for the capability
// interfaces. The two capabilities are independent.
func TestCreateProviderAnnouncesDeferredCapabilities(t *testing.T) {
	plain := CreateProvider(CreateProviderOptions{ID: "plain", API: ptrStreams(capture(new(*Model), new(*StreamOptions)))})
	if _, ok := plain.(DeferredFetcher); ok {
		t.Fatal("a provider whose api cannot fetch deferred responses must not announce DeferredFetcher")
	}
	if _, ok := plain.(DeferredCanceller); ok {
		t.Fatal("a provider whose api cannot cancel deferred responses must not announce DeferredCanceller")
	}

	fetchOnly := CreateProvider(CreateProviderOptions{ID: "fetch", API: ptrStreams(ProviderStreams{
		FetchDeferred: func(_ context.Context, _ *Model, _ DeferredHandle, _ *DeferredFetchOptions) *AssistantMessageEventStream {
			return nil
		},
	})})
	if _, ok := fetchOnly.(DeferredFetcher); !ok {
		t.Fatal("an api that fetches deferred responses must announce DeferredFetcher")
	}
	if _, ok := fetchOnly.(DeferredCanceller); ok {
		t.Fatal("fetch support must not imply cancel support")
	}

	cancelOnly := CreateProvider(CreateProviderOptions{ID: "cancel", API: ptrStreams(ProviderStreams{
		CancelDeferred: func(_ context.Context, _ *Model, _ DeferredHandle, _ *DeferredCancelOptions) error { return nil },
	})})
	if _, ok := cancelOnly.(DeferredCanceller); !ok {
		t.Fatal("an api that cancels deferred responses must announce DeferredCanceller")
	}
	if _, ok := cancelOnly.(DeferredFetcher); ok {
		t.Fatal("cancel support must not imply fetch support")
	}

	// A mixed-api provider announces the capability its other api has, but the
	// model whose api lacks it still fails — with the per-api message.
	var cancelled []DeferredHandle
	mixed := CreateProvider(CreateProviderOptions{
		ID: "mixed",
		APIByApi: map[Api]ProviderStreams{
			"api-a": deferredStreams(&cancelled, nil),
			"api-b": capture(new(*Model), new(*StreamOptions)),
		},
	})
	fetcher, ok := mixed.(DeferredFetcher)
	if !ok {
		t.Fatal("one capable api must make the provider announce DeferredFetcher")
	}
	res := fetcher.FetchDeferred(context.Background(),
		&Model{Provider: "mixed", ID: "b", Api: "api-b"}, DeferredHandle{ID: "x"}, &DeferredFetchOptions{}).Result()
	if res.StopReason != StopError || !strings.Contains(res.ErrorMessage, `deferred responses for "api-b"`) {
		t.Fatalf("an incapable api must fail per-api, got %q", res.ErrorMessage)
	}
}

// TestModelsFetchAndCancelDeferred locks pi 382aa641c's Models entry points:
// both apply auth before dispatch, FetchDeferred returns the redeemed message,
// and a provider that never announced the capability is refused.
func TestModelsFetchAndCancelDeferred(t *testing.T) {
	var cancelled []DeferredHandle
	var cancelOpts []*DeferredCancelOptions
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "deferrer",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("deferrer", "K")},
		Models: []*Model{{Provider: "deferrer", ID: "m", Api: "api-a"}},
		API:    ptrStreams(deferredStreams(&cancelled, &cancelOpts)),
	}))
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "plain",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("plain", "K")},
		Models: []*Model{{Provider: "plain", ID: "m", Api: "api-a"}},
		API:    ptrStreams(capture(new(*Model), new(*StreamOptions))),
	}))

	model := m.GetModel("deferrer", "m")
	handle := DeferredHandle{Provider: "deferrer", ModelID: "m", Api: "api-a", ID: "resp-1"}

	// The resolved api key must reach the provider, so auth is applied.
	got := m.FetchDeferred(context.Background(), model, handle, nil)
	if got.StopReason != StopStop {
		t.Fatalf("fetch should redeem the handle, got %q (%s)", got.StopReason, got.ErrorMessage)
	}
	if text := got.Content[0].(TextContent).Text; text != "resp-1:key" {
		t.Fatalf("fetch got %q, want the handle id and the resolved api key", text)
	}

	if err := m.CancelDeferred(context.Background(), model, handle, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !reflect.DeepEqual(cancelled, []DeferredHandle{handle}) {
		t.Fatalf("cancel did not reach the provider: %+v", cancelled)
	}
	// Cancelling carries request options only (upstream 686f193e5), and auth is
	// applied to them just as it is on the fetch path.
	if len(cancelOpts) != 1 || cancelOpts[0] == nil || cancelOpts[0].APIKey != "key" {
		t.Fatalf("cancel options = %+v, want the resolved api key", cancelOpts)
	}

	// A provider with no deferred support is refused at the Models layer.
	plainModel := m.GetModel("plain", "m")
	res := m.FetchDeferred(context.Background(), plainModel, handle, nil)
	if res.StopReason != StopError || !strings.Contains(res.ErrorMessage, "does not support deferred responses") {
		t.Fatalf("unsupported provider must fail in-band, got %q", res.ErrorMessage)
	}
	err := m.CancelDeferred(context.Background(), plainModel, handle, nil)
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrProvider {
		t.Fatalf("unsupported cancel should be a provider ModelsError, got %v", err)
	}

	// An unknown provider is reported as such rather than as missing support.
	res = m.FetchDeferred(context.Background(), &Model{Provider: "nope", ID: "m"}, handle, nil)
	if !strings.Contains(res.ErrorMessage, "Unknown provider") {
		t.Fatalf("unknown provider should say so, got %q", res.ErrorMessage)
	}
}

// FetchDeferred copies the caller's DeferredFetchOptions and overwrites only
// the auth-applied base, exactly as Stream and StreamSimple do. Building a
// fresh struct and hand-copying the fields it happens to know about would
// silently drop whatever is added to DeferredFetchOptions next; Wait stands in
// here for that whole class of field.
func TestModelsFetchDeferredKeepsTheCallersOwnOptions(t *testing.T) {
	var fetched []*DeferredFetchOptions
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "deferrer",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("deferrer", "K")},
		Models: []*Model{{Provider: "deferrer", ID: "m", Api: "api-a"}},
		API: ptrStreams(ProviderStreams{
			FetchDeferred: func(_ context.Context, _ *Model, _ DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
				fetched = append(fetched, opts)
				s := NewAssistantMessageEventStream()
				s.Push(AssistantMessageEvent{
					Type: EventDone, Reason: StopStop,
					Message: &AssistantMessage{StopReason: StopStop},
				})
				s.End()
				return s
			},
		}),
	}))

	wait := 250 * time.Millisecond
	m.FetchDeferred(context.Background(), m.GetModel("deferrer", "m"),
		DeferredHandle{Provider: "deferrer", ModelID: "m", Api: "api-a", ID: "resp-1"},
		&ModelsDeferredFetchOptions{DeferredFetchOptions: DeferredFetchOptions{Wait: &wait}})

	if len(fetched) != 1 || fetched[0] == nil {
		t.Fatalf("the fetch did not reach the provider: %+v", fetched)
	}
	if fetched[0].APIKey != "key" {
		t.Fatalf("APIKey = %q, want the auth-applied base to have been written over", fetched[0].APIKey)
	}
	if fetched[0].Wait == nil || *fetched[0].Wait != wait {
		t.Fatalf("Wait = %v, want the caller's own field to survive auth application", fetched[0].Wait)
	}
}

// headerValue returns the present value of headers[name], failing the test
// instead of panicking on a nil deref when the entry is absent or is a
// deletion marker — both of which are regressions worth a message.
func headerValue(t *testing.T, headers ProviderHeaders, name string) string {
	t.Helper()
	value, ok := headers[name]
	if !ok {
		t.Fatalf("header %q absent, want a present value: %v", name, headers)
	}
	if value == nil {
		t.Fatalf("header %q is a deletion marker, want a present value", name)
	}
	return *value
}

// StreamDeferred is the streaming form of redeeming a handle, and FetchDeferred
// is defined as `streamDeferred(...).result()` (upstream b37834b69). Callers
// that want the events — a durable runner rendering progress while it polls —
// must be able to observe them, and both entry points must apply auth and
// refuse an incapable provider identically, since one is now built on the other.
func TestModelsStreamDeferred(t *testing.T) {
	var cancelled []DeferredHandle
	var cancelOpts []*DeferredCancelOptions
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "deferrer",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("deferrer", "K")},
		Models: []*Model{{Provider: "deferrer", ID: "m", Api: "api-a"}},
		API:    ptrStreams(deferredStreams(&cancelled, &cancelOpts)),
	}))
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "plain",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("plain", "K")},
		Models: []*Model{{Provider: "plain", ID: "m", Api: "api-a"}},
		API:    ptrStreams(capture(new(*Model), new(*StreamOptions))),
	}))

	handle := DeferredHandle{Provider: "deferrer", ModelID: "m", Api: "api-a", ID: "resp-1"}
	stream := m.StreamDeferred(context.Background(), m.GetModel("deferrer", "m"), handle, nil)

	// The events, not just the final message: that is the whole point of the
	// entry point b37834b69 added.
	sawDone := false
	for event := range stream.Events() {
		if event.Type == EventDone {
			sawDone = true
			if text := event.Message.Content[0].(TextContent).Text; text != "resp-1:key" {
				t.Fatalf("done message = %q, want the handle id and the resolved api key", text)
			}
		}
	}
	if !sawDone {
		t.Fatalf("StreamDeferred produced no done event")
	}

	// Resolution failures still arrive in-band on the stream, as they do on the
	// FetchDeferred path built over it.
	res := m.StreamDeferred(context.Background(), m.GetModel("plain", "m"), handle, nil).Result()
	if res.StopReason != StopError || !strings.Contains(res.ErrorMessage, "does not support deferred responses") {
		t.Fatalf("unsupported provider must fail in-band, got %q", res.ErrorMessage)
	}
	// FetchDeferred is defined over StreamDeferred, so the two cannot report a
	// resolution failure differently — not by stop reason, not by message.
	fetched := m.FetchDeferred(context.Background(), m.GetModel("plain", "m"), handle, nil)
	if fetched.StopReason != res.StopReason || fetched.ErrorMessage != res.ErrorMessage {
		t.Fatalf("FetchDeferred reported (%q, %q), StreamDeferred reported (%q, %q); want the same failure", fetched.StopReason, fetched.ErrorMessage, res.StopReason, res.ErrorMessage)
	}
	res = m.StreamDeferred(context.Background(), &Model{Provider: "nope", ID: "m"}, handle, nil).Result()
	if !strings.Contains(res.ErrorMessage, "Unknown provider") {
		t.Fatalf("unknown provider should say so, got %q", res.ErrorMessage)
	}
}

// stubAPI is a chat implementation for test providers that never stream:
// CreateProvider requires at least one implementation (pi a328aa89a).
// Streaming through it fails, as streaming a provider with none used to.
func stubAPI() *ProviderStreams {
	fail := func(model *Model) *AssistantMessageEventStream {
		return ErrorStream(model, errors.New("stub test api: this provider does not stream"))
	}
	return &ProviderStreams{
		Stream: func(_ context.Context, model *Model, _ TranscriptContext, _ *StreamOptions) *AssistantMessageEventStream {
			return fail(model)
		},
		StreamSimple: func(_ context.Context, model *Model, _ TranscriptContext, _ *SimpleStreamOptions) *AssistantMessageEventStream {
			return fail(model)
		},
	}
}

// TestModelsGetAuthStopsWaitingOnCancel locks pi resolveProviderAuth's
// raceWithAbortSignal (upstream fed6009cc): a resolver that ignores its context
// cannot hold a cancelled caller hostage.
func TestModelsGetAuthStopsWaitingOnCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	m := modelsWithEnv(nil, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "cmd",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "cmd",
			Resolve: func(context.Context, AuthContext, *Credential) (*AuthResult, error) {
				close(started)
				<-release // ignores its context
				return &AuthResult{Auth: ModelAuth{APIKey: "late"}}, nil
			},
		}},
		API: stubAPI(),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := m.GetProviderAuth(ctx, "cmd", nil)
		errs <- err
	}()
	<-started
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled resolution must surface the cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled resolution must return without waiting for a stalled resolver")
	}
}

// TestModelsRefreshStopsWaitingOnSupersede locks pi's per-provider
// raceWithAbortSignal(operation, signal) in refresh(): a superseded provider's
// refresh settles at once, so the older sweep returns without waiting for a
// provider that ignores its context.
func TestModelsRefreshStopsWaitingOnSupersede(t *testing.T) {
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var fetches atomic.Int64
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:   "dynamic",
		Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("dynamic", "K")},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			if fetches.Add(1) == 1 {
				close(firstStarted)
				<-release // ignores its context
			}
			return nil, nil
		},
		API: stubAPI(),
	}))

	firstResults := make(chan ModelsRefreshResult, 1)
	go func() { firstResults <- m.Refresh(context.Background(), nil) }()
	<-firstStarted
	if second := m.Refresh(context.Background(), nil); second.Aborted || len(second.Errors) != 0 {
		t.Fatalf("the superseding refresh must succeed: %+v", second)
	}

	select {
	case first := <-firstResults:
		if first.Aborted || len(first.Errors) != 0 {
			t.Fatalf("a superseded refresh is neither aborted nor failed: %+v", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a superseded refresh must return without waiting for a stalled provider")
	}
}
