package ai

import (
	"context"
	"errors"
	"testing"
)

// capture builds ProviderStreams whose Stream records the model + options it
// was handed and returns a closed stream.
func capture(gotModel **Model, gotOpts **StreamOptions) ProviderStreams {
	return ProviderStreams{
		Stream: func(_ context.Context, model *Model, _ Context, opts *StreamOptions) *AssistantMessageEventStream {
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

	p.Stream(context.Background(), &Model{Provider: "multi", ID: "a", Api: "api-a"}, Context{}, nil)
	if *gotA == nil || (*gotA).Api != "api-a" {
		t.Fatalf("api-a not dispatched, got %v", *gotA)
	}
	// A model whose api has no implementation yields a stream error.
	res := p.Stream(context.Background(), &Model{Provider: "multi", ID: "c", Api: "api-z"}, Context{}, nil).Result()
	if res.StopReason != StopError {
		t.Fatalf("missing api should produce a stream error, got %v", res.StopReason)
	}
}

func TestModelsCollection(t *testing.T) {
	m := CreateModels(nil)
	pa := CreateProvider(CreateProviderOptions{ID: "a", Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("a", "A")}, Models: []*Model{{Provider: "a", ID: "m1"}}})
	pb := CreateProvider(CreateProviderOptions{ID: "b", Auth: ProviderAuth{APIKey: EnvAPIKeyAuth("b", "B")}, Models: []*Model{{Provider: "b", ID: "m2"}}})
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
	m.Stream(context.Background(), model, Context{}, &StreamOptions{APIKey: "explicit"})
	if gotOpts.APIKey != "explicit" {
		t.Fatalf("explicit key should win: %q", gotOpts.APIKey)
	}
}

func TestModelsApplyAuthBaseURLHeadersEnv(t *testing.T) {
	var gotModel *Model
	var gotOpts *StreamOptions
	auth := &ApiKeyAuth{
		Name: "custom",
		Resolve: func(_ *Model, _ AuthContext, _ *Credential) (*AuthResult, error) {
			return &AuthResult{
				Auth: ModelAuth{APIKey: "k", BaseURL: "https://auth.example", Headers: map[string]string{"H": "auth", "Keep": "auth"}},
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
	m.Stream(context.Background(), model, Context{}, &StreamOptions{
		Headers: map[string]string{"H": "explicit"},
		Env:     map[string]string{"E": "explicit"},
	})
	if gotModel.BaseURL != "https://auth.example" {
		t.Errorf("auth baseURL should override: %q", gotModel.BaseURL)
	}
	if model.BaseURL != "https://model" {
		t.Errorf("original model must not be mutated: %q", model.BaseURL)
	}
	if gotOpts.Headers["H"] != "explicit" || gotOpts.Headers["Keep"] != "auth" {
		t.Errorf("header merge wrong (explicit wins per key): %v", gotOpts.Headers)
	}
	if gotOpts.Env["E"] != "explicit" || gotOpts.Env["KeepEnv"] != "auth" {
		t.Errorf("env merge wrong (explicit wins per key): %v", gotOpts.Env)
	}
}

func TestModelsUnknownProvider(t *testing.T) {
	m := CreateModels(nil)
	res := m.Stream(context.Background(), &Model{Provider: "ghost", ID: "x", Api: "api"}, Context{}, nil).Result()
	if res.StopReason != StopError {
		t.Fatalf("unknown provider should error, got %v", res.StopReason)
	}
}

func TestModelsGetAuthUnconfigured(t *testing.T) {
	m := CreateModels(nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("p", "DEFINITELY_UNSET_KEY_XYZ")},
		Models: []*Model{{Provider: "p", ID: "m", Api: "api"}},
	}))
	res, err := m.GetAuth(&Model{Provider: "p", ID: "m", Api: "api"})
	if err != nil || res != nil {
		t.Fatalf("unconfigured provider should resolve (nil, nil), got (%+v, %v)", res, err)
	}
	// Unknown provider also resolves nil.
	if res, err := m.GetAuth(&Model{Provider: "ghost"}); err != nil || res != nil {
		t.Fatalf("unknown provider GetAuth = (%+v, %v)", res, err)
	}
}

func TestModelsRefreshDynamic(t *testing.T) {
	calls := 0
	m := CreateModels(nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "dyn",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("dyn", "K")},
		Models: nil,
		RefreshModels: func() ([]*Model, error) {
			calls++
			return []*Model{{Provider: "dyn", ID: "fetched"}}, nil
		},
	}))
	if err := m.Refresh("dyn"); err != nil {
		t.Fatalf("refresh err: %v", err)
	}
	if got := m.GetModels("dyn"); len(got) != 1 || got[0].ID != "fetched" {
		t.Fatalf("refresh did not update model list: %v", got)
	}

	// Refresh failure on a named provider wraps as ModelsError model_source.
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:            "boom",
		Auth:          ProviderAuth{APIKey: EnvAPIKeyAuth("boom", "K")},
		RefreshModels: func() ([]*Model, error) { return nil, errors.New("network") },
	}))
	err := m.Refresh("boom")
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrModelSource {
		t.Fatalf("named refresh failure should wrap as model_source, got %v", err)
	}
	// Best-effort all-refresh never errors.
	if err := m.Refresh(""); err != nil {
		t.Fatalf("refresh-all should not error: %v", err)
	}
}

func ptrStreams(s ProviderStreams) *ProviderStreams { return &s }
