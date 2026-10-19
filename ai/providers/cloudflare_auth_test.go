package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// cloudflareAuthCapture is testdata/cloudflare-auth/cloudflare-auth-1.3.0.json
// (capture.mjs against the npm 1.3.0 build).
type cloudflareAuthCapture struct {
	Version string `json:"version"`
	Auth    []struct {
		Env        string                     `json:"env"`
		Credential string                     `json:"credential"`
		Provider   string                     `json:"provider"`
		CheckAuth  any                        `json:"checkAuth"`
		GetAuth    map[string]json.RawMessage `json:"getAuth"`
	} `json:"auth"`
	Login []struct {
		Provider   string          `json:"provider"`
		Name       string          `json:"name"`
		Prompts    []ai.AuthPrompt `json:"prompts"`
		Credential json.RawMessage `json:"credential"`
		Stored     json.RawMessage `json:"stored"`
	} `json:"login"`
	Stream []cloudflareStreamRow `json:"stream"`
	Direct []struct {
		Api     ai.Api `json:"api"`
		Options string `json:"options"`
		cloudflareOutcome
	} `json:"direct"`
}

// cloudflareOutcome is what a row's stream did: its requests, or the setup
// error when it sent none.
type cloudflareOutcome struct {
	Requests     []cloudflareRecord `json:"requests"`
	StopReason   ai.StopReason      `json:"stopReason"`
	ErrorMessage *string            `json:"errorMessage"`
}

type cloudflareStreamRow struct {
	Env        string `json:"env"`
	Credential string `json:"credential"`
	Provider   string `json:"provider"`
	ModelID    string `json:"modelId"`
	Options    string `json:"options"`
	Entry      string `json:"entry"`
	cloudflareOutcome
}

// cloudflareRecord is one request as the capture records it: the URL and the
// headers that carry Cloudflare auth.
type cloudflareRecord struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// The capture's ambient environments, stored credentials and request option
// sets, by name (capture.mjs).
var (
	cloudflareCaptureEnvs = map[string]map[string]string{
		"none":                {},
		"key":                 {"CLOUDFLARE_API_KEY": "env-key"},
		"blank-key":           {"CLOUDFLARE_API_KEY": "  ", "CLOUDFLARE_ACCOUNT_ID": "env-account", "CLOUDFLARE_GATEWAY_ID": "env-gateway"},
		"key-account":         {"CLOUDFLARE_API_KEY": "env-key", "CLOUDFLARE_ACCOUNT_ID": "env-account"},
		"key-account-gateway": {"CLOUDFLARE_API_KEY": "env-key", "CLOUDFLARE_ACCOUNT_ID": "env-account", "CLOUDFLARE_GATEWAY_ID": "env-gateway"},
		"account-gateway":     {"CLOUDFLARE_ACCOUNT_ID": "env-account", "CLOUDFLARE_GATEWAY_ID": "env-gateway"},
	}
	cloudflareCaptureCredentials = map[string]*ai.Credential{
		"none":                nil,
		"key-only":            {Type: ai.CredentialAPIKey, Key: "stored-key"},
		"key-account":         {Type: ai.CredentialAPIKey, Key: "stored-key", Env: map[string]string{"CLOUDFLARE_ACCOUNT_ID": "stored-account"}},
		"key-account-gateway": {Type: ai.CredentialAPIKey, Key: "stored-key", Env: map[string]string{"CLOUDFLARE_ACCOUNT_ID": "stored-account", "CLOUDFLARE_GATEWAY_ID": "stored-gateway"}},
		"empty-key":           {Type: ai.CredentialAPIKey, Key: "", Env: map[string]string{"CLOUDFLARE_ACCOUNT_ID": "stored-account", "CLOUDFLARE_GATEWAY_ID": "stored-gateway"}},
		"empty-account":       {Type: ai.CredentialAPIKey, Key: "stored-key", Env: map[string]string{"CLOUDFLARE_ACCOUNT_ID": ""}},
	}
	cloudflareRequestEnv  = map[string]string{"CLOUDFLARE_ACCOUNT_ID": "request-account", "CLOUDFLARE_GATEWAY_ID": "request-gateway"}
	cloudflareCaptureOpts = map[string]func() ai.StreamOptions{
		"none":   func() ai.StreamOptions { return ai.StreamOptions{} },
		"apiKey": func() ai.StreamOptions { return cloudflareRequestOptions("request-key", nil, nil) },
		"env":    func() ai.StreamOptions { return cloudflareRequestOptions("", cloudflareRequestEnv, nil) },
		"apiKey-env": func() ai.StreamOptions {
			return cloudflareRequestOptions("request-key", cloudflareRequestEnv, nil)
		},
		"gateway-header": func() ai.StreamOptions {
			header := "Bearer request-header"
			return cloudflareRequestOptions("", nil, ai.ProviderHeaders{"cf-aig-authorization": &header})
		},
	}
)

func cloudflareRequestOptions(apiKey string, env map[string]string, headers ai.ProviderHeaders) ai.StreamOptions {
	var copied map[string]string
	if env != nil {
		copied = map[string]string{}
		for k, v := range env {
			copied[k] = v
		}
	}
	return ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: apiKey, Env: copied, Headers: headers}}
}

// setCloudflareEnv makes env the whole CLOUDFLARE_* environment, as the
// capture's setEnv does (an unset variable is "", which every reader treats as
// absent).
func setCloudflareEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{"CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"} {
		t.Setenv(name, env[name])
	}
}

// cloudflareModels is pi's builtinModels({credentials: store}) with credential
// stored for both Cloudflare providers.
func cloudflareModels(t *testing.T, credential *ai.Credential) ai.MutableModels {
	t.Helper()
	store := ai.NewInMemoryCredentialStore()
	if credential != nil {
		for _, id := range []string{"cloudflare-workers-ai", "cloudflare-ai-gateway"} {
			stored := *credential
			if _, err := store.Modify(context.Background(), id, func(*ai.Credential) (*ai.Credential, error) { return &stored, nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	models := ai.CreateModels(&ai.CreateModelsOptions{Credentials: store})
	for _, provider := range ai.BuiltinModels().GetProviders() {
		models.SetProvider(provider)
	}
	return models
}

// cloudflareRecorder answers every request 401 (no SDK retries it) and
// records it as the capture does.
type cloudflareRecorder struct {
	mu       sync.Mutex
	requests []cloudflareRecord
}

func (r *cloudflareRecorder) Do(req *http.Request) (*http.Response, error) {
	headers := map[string]string{}
	for _, name := range []string{"authorization", "x-api-key", "cf-aig-authorization"} {
		if values := req.Header.Values(name); len(values) > 0 {
			headers[name] = strings.Join(values, ", ")
		}
	}
	r.mu.Lock()
	r.requests = append(r.requests, cloudflareRecord{Method: req.Method, URL: req.URL.String(), Headers: headers})
	r.mu.Unlock()
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"captured","type":"authentication_error"}}`)),
		Request:    req,
	}, nil
}

// piAuthResult is an AuthResult as pi's getAuth returns it, JSON-shaped.
func piAuthResult(result *ai.AuthResult) any {
	if result == nil {
		return nil
	}
	auth := map[string]any{}
	if result.Auth.APIKey != "" {
		auth["apiKey"] = result.Auth.APIKey
	}
	if result.Auth.Headers != nil {
		headers := map[string]any{}
		for name, value := range result.Auth.Headers {
			if value == nil {
				headers[name] = nil
			} else {
				headers[name] = *value
			}
		}
		auth["headers"] = headers
	}
	if result.Auth.BaseURL != "" {
		auth["baseUrl"] = result.Auth.BaseURL
	}
	out := map[string]any{"auth": auth, "source": result.Source}
	if result.Env != nil {
		env := map[string]any{}
		for k, v := range result.Env {
			env[k] = v
		}
		out["env"] = env
	}
	return out
}

func jsonValue(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return jsonValue(t, raw)
}

func loadCloudflareAuthCapture(t *testing.T) cloudflareAuthCapture {
	t.Helper()
	raw, err := os.ReadFile("testdata/cloudflare-auth/cloudflare-auth-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture cloudflareAuthCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

// TestCloudflareAuthMatchesPi: the Cloudflare providers of BuiltinModels
// resolve auth as pi's cloudflareWorkersAIAuth / cloudflareAIGatewayAuth do —
// the key, the account id and (ai-gateway) the gateway id are all required,
// each from the stored credential (or the request's apiKey/env) else the
// environment; the ids travel as env and ai-gateway's key as header-owned
// auth — so CheckAuth, GetAuth and login match pi row for row.
func TestCloudflareAuthMatchesPi(t *testing.T) {
	capture := loadCloudflareAuthCapture(t)
	ctx := context.Background()
	for _, row := range capture.Auth {
		name := row.Env + "/" + row.Credential + "/" + row.Provider
		setCloudflareEnv(t, cloudflareCaptureEnvs[row.Env])
		models := cloudflareModels(t, cloudflareCaptureCredentials[row.Credential])
		check, err := models.CheckAuth(ctx, row.Provider)
		if err != nil {
			t.Fatalf("%s: CheckAuth: %v", name, err)
		}
		var got any
		if check != nil {
			got = map[string]any{"source": check.Source, "type": string(check.Type)}
		}
		if !reflect.DeepEqual(asJSON(t, got), row.CheckAuth) {
			t.Errorf("%s: checkAuth\n go %v\n pi %v", name, asJSON(t, got), row.CheckAuth)
		}
		for overridesName, want := range row.GetAuth {
			var overrides *ai.AuthResolutionOverrides
			if opts := cloudflareCaptureOpts[overridesName](); opts.APIKey != "" || opts.Env != nil {
				overrides = &ai.AuthResolutionOverrides{APIKey: opts.APIKey, Env: opts.Env}
			}
			result, err := models.GetProviderAuth(ctx, row.Provider, overrides)
			if err != nil {
				t.Fatalf("%s: getAuth(%s): %v", name, overridesName, err)
			}
			if got, want := asJSON(t, piAuthResult(result)), jsonValue(t, want); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: getAuth(%s)\n go %v\n pi %v", name, overridesName, got, want)
			}
		}
	}

	setCloudflareEnv(t, nil)
	for _, row := range capture.Login {
		store := ai.NewInMemoryCredentialStore()
		models := ai.CreateModels(&ai.CreateModelsOptions{Credentials: store})
		for _, provider := range ai.BuiltinModels().GetProviders() {
			models.SetProvider(provider)
		}
		if got := models.GetProvider(row.Provider).Auth().APIKey.Name; got != row.Name {
			t.Errorf("%s: name %q, pi %q", row.Provider, got, row.Name)
		}
		interaction := &recordingInteraction{answers: []string{"login-key", "login-account", "login-gateway"}}
		credential, err := models.Login(ctx, row.Provider, ai.CredentialAPIKey, interaction, nil)
		if err != nil {
			t.Fatalf("%s: login: %v", row.Provider, err)
		}
		if !reflect.DeepEqual(interaction.prompts, row.Prompts) {
			t.Errorf("%s: prompts\n go %+v\n pi %+v", row.Provider, interaction.prompts, row.Prompts)
		}
		if got, want := asJSON(t, credential), jsonValue(t, row.Credential); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: credential\n go %v\n pi %v", row.Provider, got, want)
		}
		stored, err := store.Read(ctx, row.Provider)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := asJSON(t, stored), jsonValue(t, row.Stored); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: stored\n go %v\n pi %v", row.Provider, got, want)
		}
	}
}

type recordingInteraction struct {
	answers []string
	prompts []ai.AuthPrompt
}

func (r *recordingInteraction) Prompt(prompt ai.AuthPrompt) (string, error) {
	r.prompts = append(r.prompts, prompt)
	if len(r.prompts) > len(r.answers) {
		return "", fmt.Errorf("unexpected prompt %q", prompt.Message)
	}
	return r.answers[len(r.prompts)-1], nil
}

func (r *recordingInteraction) Notify(ai.AuthEvent) {}

// streamCloudflareRow streams one capture row through Models (the row's
// stored credential) or compat's Stream, and reports its outcome as the
// capture records it.
func streamCloudflareRow(t *testing.T, row cloudflareStreamRow) cloudflareStreamRow {
	t.Helper()
	setCloudflareEnv(t, cloudflareCaptureEnvs[row.Env])
	models := cloudflareModels(t, cloudflareCaptureCredentials[row.Credential])
	model := models.GetModel(row.Provider, row.ModelID)
	if model == nil {
		t.Fatalf("%s/%s is not in the catalog", row.Provider, row.ModelID)
	}
	recorder := &cloudflareRecorder{}
	options := cloudflareCaptureOpts[row.Options]()
	options.HTTPClient = recorder
	var result *ai.AssistantMessage
	if row.Entry == "models" {
		result = models.Stream(context.Background(), model, ai.Context{}, &ai.ModelsStreamOptions{StreamOptions: options}).Result()
	} else {
		result = ai.Stream(context.Background(), model, ai.Context{}, &options).Result()
	}
	got := row
	got.cloudflareOutcome = recordedOutcome(recorder, result)
	return got
}

func recordedOutcome(recorder *cloudflareRecorder, result *ai.AssistantMessage) cloudflareOutcome {
	got := cloudflareOutcome{Requests: recorder.requests, StopReason: result.StopReason}
	if got.Requests == nil {
		got.Requests = []cloudflareRecord{}
	}
	if len(got.Requests) == 0 {
		message := result.ErrorMessage
		got.ErrorMessage = &message
	}
	return got
}

func (o cloudflareOutcome) matches(want cloudflareOutcome) bool {
	if want.Requests == nil {
		want.Requests = []cloudflareRecord{}
	}
	return reflect.DeepEqual(o.Requests, want.Requests) && o.StopReason == want.StopReason &&
		reflect.DeepEqual(o.ErrorMessage, want.ErrorMessage)
}

func describeCloudflareOutcome(row cloudflareOutcome) string {
	if len(row.Requests) > 0 {
		raw, _ := json.Marshal(row.Requests)
		return string(raw)
	}
	if row.ErrorMessage == nil {
		return fmt.Sprintf("%s with no message", row.StopReason)
	}
	return fmt.Sprintf("%s %q", row.StopReason, *row.ErrorMessage)
}

// TestCloudflareRequestsMatchPi: a Cloudflare model streamed through Models,
// or through compat's Stream (which routes a request without auth of its own
// through the built-in Models), reaches the URL and carries the auth headers pi's
// does — or fails with pi's setup error — for every ambient environment,
// stored credential and request option set of the capture. A compat request
// with auth of its own (an explicit apiKey or a cf-aig-authorization header)
// streams through the built-in provider: its endpoint placeholders resolve
// from the request's env alone, and the adapter sends the key as it would for
// any provider.
func TestCloudflareRequestsMatchPi(t *testing.T) {
	// Record the registry's api implementations as the built-ins, as pi's
	// compat has them after it loads: other tests in this package register
	// implementations of their own (TestAnthropicRegisterAndStreamSimple).
	RegisterBuiltins()
	capture := loadCloudflareAuthCapture(t)
	mismatches := 0
	for _, want := range capture.Stream {
		got := streamCloudflareRow(t, want)
		if got.matches(want.cloudflareOutcome) {
			continue
		}
		mismatches++
		t.Errorf("%s %s/%s %s %s options:\n go %s\n pi %s", want.Entry, want.Env, want.Credential, want.ModelID, want.Options,
			describeCloudflareOutcome(got.cloudflareOutcome), describeCloudflareOutcome(want.cloudflareOutcome))
	}
	if mismatches > 0 {
		t.Logf("%d of %d rows differ", mismatches, len(capture.Stream))
	}
}

// TestAdapterAuthHeadersMatchPi: an adapter called on its own, for a model of
// no Cloudflare provider, sends a request whose auth headers the options own
// as pi's does, and refuses one that carries neither of its SDK's auth
// headers, nor a marker deleting one, as the SDK's validateHeaders does.
func TestAdapterAuthHeadersMatchPi(t *testing.T) {
	capture := loadCloudflareAuthCapture(t)
	directOptions := map[string]func() ai.StreamOptions{
		"gateway-header": func() ai.StreamOptions {
			return cloudflareRequestOptions("", nil, ai.ProviderHeaders{"cf-aig-authorization": strPtr("Bearer request-header")})
		},
		"gateway-header-no-authorization": func() ai.StreamOptions {
			return cloudflareRequestOptions("", nil, ai.ProviderHeaders{"cf-aig-authorization": strPtr("Bearer request-header"), "Authorization": nil})
		},
		"gateway-header-no-x-api-key": func() ai.StreamOptions {
			return cloudflareRequestOptions("", nil, ai.ProviderHeaders{"cf-aig-authorization": strPtr("Bearer request-header"), "x-api-key": nil})
		},
		"key-empty-x-api-key": func() ai.StreamOptions {
			return cloudflareRequestOptions("request-key", nil, ai.ProviderHeaders{"x-api-key": strPtr("")})
		},
		"key-no-x-api-key": func() ai.StreamOptions {
			return cloudflareRequestOptions("request-key", nil, ai.ProviderHeaders{"X-Api-Key": nil})
		},
		"key-empty-authorization": func() ai.StreamOptions {
			return cloudflareRequestOptions("request-key", nil, ai.ProviderHeaders{"Authorization": strPtr("")})
		},
		"key-no-authorization": func() ai.StreamOptions {
			return cloudflareRequestOptions("request-key", nil, ai.ProviderHeaders{"authorization": nil})
		},
	}
	for _, want := range capture.Direct {
		model := &ai.Model{
			ID: "direct-model", Name: "Direct Model", Api: want.Api, Provider: "direct",
			BaseURL: "https://example.test/v1", Input: []string{"text"}, ContextWindow: 10000, MaxTokens: 1000,
		}
		provider, ok := ai.GetApiProvider(want.Api)
		if !ok {
			t.Fatalf("no api provider for %s", want.Api)
		}
		recorder := &cloudflareRecorder{}
		options := directOptions[want.Options]()
		options.HTTPClient = recorder
		result := provider.Stream(context.Background(), model, ai.NormalizeContext(ai.Context{}), &options).Result()
		if got := recordedOutcome(recorder, result); !got.matches(want.cloudflareOutcome) {
			t.Errorf("%s %s:\n go %s\n pi %s", want.Api, want.Options, describeCloudflareOutcome(got), describeCloudflareOutcome(want.cloudflareOutcome))
		}
	}
}
