package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// anthropicFederationCaptureFile is written by
// testdata/anthropic-federation/capture.mjs, which streams the published pi-ai
// 0.99.2 anthropic-messages adapter, with its real @anthropic-ai/sdk 0.124.0
// client, against a loopback server.
const anthropicFederationCaptureFile = "testdata/anthropic-federation/anthropic-federation-0.99.2.json"

// anthropicFederationEnvVars are the variables pi's federation reads, cleared
// from the ambient environment so a row's env is the only source.
var anthropicFederationEnvVars = []string{
	"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE",
	"ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_WORKSPACE_ID", ai.AnthropicAuthTokenEnv,
}

func clearAnthropicFederationEnv(t *testing.T) {
	t.Helper()
	for _, name := range anthropicFederationEnvVars {
		t.Setenv(name, "")
	}
}

type federationExchange struct {
	Method  string             `json:"method"`
	Path    string             `json:"path"`
	Headers map[string]*string `json:"headers"`
	Body    string             `json:"body"`
}

type federationMessage struct {
	Method  string             `json:"method"`
	Path    string             `json:"path"`
	Headers map[string]*string `json:"headers"`
}

type federationResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type federationResult struct {
	StopReason   ai.StopReason `json:"stopReason"`
	ErrorMessage *string       `json:"errorMessage"`
}

// federationServer is the capture's loopback server: POST /v1/oauth/token
// answers with the row's exchange responses in turn (the last repeated), any
// other request with the row's statuses in turn (200, an SSE body, after them).
type federationServer struct {
	*httptest.Server
	mu        sync.Mutex
	responses []federationResponse
	statuses  []int
	exchanges []federationExchange
	messages  []federationMessage
}

var federationSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n"

func headerOrNil(h http.Header, name string) *string {
	if values := h.Values(name); len(values) > 0 {
		v := strings.Join(values, ", ")
		return &v
	}
	return nil
}

func pickHeaders(h http.Header, names ...string) map[string]*string {
	out := make(map[string]*string, len(names))
	for _, name := range names {
		out[name] = headerOrNil(h, name)
	}
	return out
}

func newFederationServer(t *testing.T, responses []federationResponse, statuses []int) *federationServer {
	t.Helper()
	s := &federationServer{responses: responses, statuses: statuses}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/v1/oauth/token") {
			s.exchanges = append(s.exchanges, federationExchange{
				Method:  r.Method,
				Path:    r.URL.RequestURI(),
				Headers: pickHeaders(r.Header, "content-type", "anthropic-beta", "user-agent", "authorization", "x-api-key"),
				Body:    string(body),
			})
			resp := s.responses[min(len(s.exchanges)-1, len(s.responses)-1)]
			for k, v := range resp.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(resp.Status)
			_, _ = io.WriteString(w, resp.Body)
			return
		}
		s.messages = append(s.messages, federationMessage{
			Method:  r.Method,
			Path:    r.URL.RequestURI(),
			Headers: pickHeaders(r.Header, "authorization", "x-api-key", "anthropic-beta"),
		})
		status := 200
		if i := len(s.messages) - 1; i < len(s.statuses) {
			status = s.statuses[i]
		}
		if status != 200 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"token rejected"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, federationSSE)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *federationServer) recorded() ([]federationExchange, []federationMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]federationExchange(nil), s.exchanges...), append([]federationMessage(nil), s.messages...)
}

// federationTestModel is the model upstream's anthropic-federation tests (and
// the capture) stream.
func federationTestModel(provider, baseURL string) *ai.Model {
	return &ai.Model{
		ID: "claude-test", Name: "Claude Test", Api: ai.APIAnthropicMessages, Provider: provider,
		BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 4096,
	}
}

func federationContext() ai.TranscriptContext {
	return ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}})
}

func streamFederation(t *testing.T, ctx context.Context, model *ai.Model, opts *AnthropicOptions) *ai.AssistantMessage {
	t.Helper()
	stream := StreamAnthropic(ctx, model, federationContext(), opts)
	return stream.Result()
}

// closedLoopbackURL is a loopback base URL nothing listens on.
func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

// TestAnthropicFederationMatchesPi replays each captured row: the same env,
// identity token file and token endpoint responses, the same number of
// streams the same time apart, and requires pi's exchanges (method, path, the
// headers the SDK sets, the body byte for byte), the auth headers of every
// messages request, and each stream's outcome.
func TestAnthropicFederationMatchesPi(t *testing.T) {
	data, err := os.ReadFile(anthropicFederationCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/anthropic-federation/capture.mjs)", anthropicFederationCaptureFile, err)
	}
	var capture struct {
		SDK  string `json:"@anthropic-ai/sdk"`
		Rows []struct {
			Name         string               `json:"name"`
			Env          map[string]string    `json:"env"`
			TokenFile    json.RawMessage      `json:"tokenFile"`
			Requests     int                  `json:"requests"`
			WaitMs       int                  `json:"waitMs"`
			Statuses     []int                `json:"statuses"`
			Base         string               `json:"base"`
			BaseSuffix   string               `json:"baseSuffix"`
			Provider     string               `json:"provider"`
			APIKey       string               `json:"apiKey"`
			Headers      map[string]string    `json:"headers"`
			PayloadBetas []string             `json:"payloadBetas"`
			Responses    []federationResponse `json:"responses"`
			Exchanges    []federationExchange `json:"exchanges"`
			Messages     []federationMessage  `json:"messages"`
			Results      []federationResult   `json:"results"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", anthropicFederationCaptureFile, err)
	}
	if capture.SDK != "0.124.0" || len(capture.Rows) == 0 {
		t.Fatalf("%s: sdk %q, %d rows", anthropicFederationCaptureFile, capture.SDK, len(capture.Rows))
	}
	clearAnthropicFederationEnv(t)
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			// The token file's text: a string, null for no file, or
			// {repeat, count} for a long one, which recorded strings name
			// <tokenFile>.
			tokenFile := filepath.Join(t.TempDir(), "identity.jwt")
			var text *string
			repeated := ""
			if string(row.TokenFile) != "null" {
				var s string
				if json.Unmarshal(row.TokenFile, &s) != nil {
					var r struct {
						Repeat string `json:"repeat"`
						Count  int    `json:"count"`
					}
					if err := json.Unmarshal(row.TokenFile, &r); err != nil {
						t.Fatalf("tokenFile %s: %v", row.TokenFile, err)
					}
					repeated = strings.Repeat(r.Repeat, r.Count)
					s = repeated
				}
				text = &s
			}
			if text != nil {
				if err := os.WriteFile(tokenFile, []byte(*text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			responses := row.Responses
			if len(responses) == 0 {
				// pi made no exchange; one here is recorded, and fails the row.
				responses = []federationResponse{{Status: 599}}
			}
			srv := newFederationServer(t, responses, row.Statuses)
			base := srv.URL
			switch {
			case row.Base == "closed":
				base = closedLoopbackURL(t)
			case row.Base != "":
				base = row.Base
			}
			env := map[string]string{}
			for k, v := range row.Env {
				if v == "<token-file>" {
					v = tokenFile
				}
				env[k] = v
			}
			scrub := func(s string) string {
				s = strings.ReplaceAll(s, base, "<base>")
				s = strings.ReplaceAll(s, tokenFile, "<token-file>")
				if repeated != "" {
					s = strings.ReplaceAll(s, repeated, "<tokenFile>")
				}
				return s
			}
			provider := row.Provider
			if provider == "" {
				provider = "anthropic"
			}
			model := federationTestModel(provider, base+row.BaseSuffix)

			requests := max(row.Requests, 1)
			var results []federationResult
			for i := range requests {
				if i > 0 && row.WaitMs > 0 {
					time.Sleep(time.Duration(row.WaitMs) * time.Millisecond)
				}
				opts := &AnthropicOptions{}
				opts.Env = env
				opts.APIKey = row.APIKey
				if row.Headers != nil {
					opts.Headers = ai.ProviderHeaders{}
					for k, v := range row.Headers {
						opts.Headers[k] = strHeader(v)
					}
				}
				if row.PayloadBetas != nil {
					betas := make([]any, len(row.PayloadBetas))
					for i, b := range row.PayloadBetas {
						betas[i] = b
					}
					opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
						next := map[string]any{}
						for k, v := range payload.(map[string]any) {
							next[k] = v
						}
						next["betas"] = betas
						return next, nil
					}
				}
				final := streamFederation(t, context.Background(), model, opts)
				r := federationResult{StopReason: final.StopReason}
				if final.ErrorMessage != "" {
					msg := scrub(final.ErrorMessage)
					r.ErrorMessage = &msg
				}
				results = append(results, r)
			}
			// A background refresh may still be in flight.
			time.Sleep(100 * time.Millisecond)

			exchanges, messages := srv.recorded()
			for i := range exchanges {
				exchanges[i].Body = scrub(exchanges[i].Body)
			}
			assertJSONEqual(t, "exchanges", exchanges, row.Exchanges)
			assertJSONEqual(t, "messages", messages, row.Messages)
			// A messages request that got an error status fails with the
			// adapter's non-2xx message, which the port does not yet word as
			// pi does (docs/UPSTREAM.md: the queued per-adapter formatter
			// slice, `Anthropic API error 401: ...` for pi's `401 {...}`).
			// This row is about the token, so only its outcome is compared.
			want := append([]federationResult(nil), row.Results...)
			for i, status := range row.Statuses {
				if status != 200 && i < len(results) && i < len(want) {
					results[i].ErrorMessage, want[i].ErrorMessage = nil, nil
				}
			}
			assertJSONEqual(t, "results", results, want)
		})
	}
}

func assertJSONEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	// nil and empty slices are the same recording.
	if string(g) == "null" {
		g = []byte("[]")
	}
	if string(w) == "null" {
		w = []byte("[]")
	}
	if string(g) != string(w) {
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}

// writeIdentityToken writes an identity token file and returns the federation
// env that names it.
func writeIdentityToken(t *testing.T, token string) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.jwt")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"ANTHROPIC_FEDERATION_RULE_ID":  "fdrl_test",
		"ANTHROPIC_ORGANIZATION_ID":     "org-test",
		"ANTHROPIC_IDENTITY_TOKEN_FILE": path,
	}
}

func grantedToken(token string) federationResponse {
	return federationResponse{
		Status:  200,
		Headers: map[string]string{"content-type": "application/json"},
		Body:    `{"access_token":"` + token + `","expires_in":3600}`,
	}
}

// TestAnthropicFederationCoalescesConcurrentExchanges pins the SDK's
// TokenCache: concurrent requests that all find no cached token join one
// in-flight exchange rather than each exchanging.
func TestAnthropicFederationCoalescesConcurrentExchanges(t *testing.T) {
	clearAnthropicFederationEnv(t)
	release := make(chan struct{})
	var mu sync.Mutex
	exchanges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/v1/oauth/token") {
			mu.Lock()
			exchanges++
			mu.Unlock()
			<-release
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"shared","expires_in":3600}`)
			return
		}
		if got := r.Header.Get("authorization"); got != "Bearer shared" {
			t.Errorf("authorization = %q, want the shared token", got)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, federationSSE)
	}))
	defer srv.Close()
	env := writeIdentityToken(t, "jwt")
	model := federationTestModel("anthropic", srv.URL)

	const n = 5
	var wg sync.WaitGroup
	finals := make([]*ai.AssistantMessage, n)
	for i := range n {
		wg.Go(func() {
			opts := &AnthropicOptions{}
			opts.Env = env
			finals[i] = streamFederation(t, context.Background(), model, opts)
		})
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, final := range finals {
		if final.StopReason != ai.StopStop {
			t.Errorf("stream %d: %s %q", i, final.StopReason, final.ErrorMessage)
		}
	}
	if exchanges != 1 {
		t.Fatalf("%d token exchanges, want 1 shared by every request", exchanges)
	}
}

// TestAnthropicFederationClientFollowsTheConfig pins pi's one federation
// client: keyed on the base URL and the config, so a change of either starts a
// fresh token cache, while the same pair keeps the cached token.
func TestAnthropicFederationClientFollowsTheConfig(t *testing.T) {
	clearAnthropicFederationEnv(t)
	srv := newFederationServer(t, []federationResponse{grantedToken("t1"), grantedToken("t2"), grantedToken("t3")}, nil)
	env := writeIdentityToken(t, "jwt")
	model := federationTestModel("anthropic", srv.URL)
	run := func(env map[string]string) {
		opts := &AnthropicOptions{}
		opts.Env = env
		if final := streamFederation(t, context.Background(), model, opts); final.StopReason != ai.StopStop {
			t.Fatalf("stream: %s %q", final.StopReason, final.ErrorMessage)
		}
	}
	run(env)
	run(env)
	workspace := map[string]string{"ANTHROPIC_WORKSPACE_ID": "wrkspc_test"}
	for k, v := range env {
		workspace[k] = v
	}
	run(workspace)
	run(env)
	_, messages := srv.recorded()
	var got []string
	for _, m := range messages {
		got = append(got, *m.Headers["authorization"])
	}
	want := []string{"Bearer t1", "Bearer t1", "Bearer t2", "Bearer t3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("authorization per request = %q, want %q", got, want)
	}
}

// TestAnthropicFederationThroughModels threads the federation variables
// through the Models runtime, as upstream's "threads authContext federation
// variables through Models" does: the anthropic resolver must report the
// provider configured (no key, no header) and hand the ids on in env.
func TestAnthropicFederationThroughModels(t *testing.T) {
	clearAnthropicFederationEnv(t)
	for _, name := range []string{"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "")
	}
	srv := newFederationServer(t, []federationResponse{grantedToken("federated-token")}, nil)
	for k, v := range writeIdentityToken(t, "jwt") {
		t.Setenv(k, v)
	}
	m := ai.BuiltinModels()
	model := federationTestModel("anthropic", srv.URL)
	final := m.StreamSimple(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}}, nil).Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("stream: %s %q", final.StopReason, final.ErrorMessage)
	}
	exchanges, messages := srv.recorded()
	if len(exchanges) != 1 || len(messages) != 1 || *messages[0].Headers["authorization"] != "Bearer federated-token" {
		t.Fatalf("exchanges %+v, messages %+v", exchanges, messages)
	}
}

// TestAnthropicTokenCacheRefreshPolicy pins the SDK TokenCache's thresholds
// on a held clock: more than 120s left serves the cached token; 120s down to
// just over 30s serves it and refreshes in the background, a failed
// background refresh holding off the next for 5s; 30s or less exchanges
// before answering, and a failure then fails the request; invalidate forces
// the next call to exchange.
func TestAnthropicTokenCacheRefreshPolicy(t *testing.T) {
	now := 1000.0
	var clockMu sync.Mutex
	old := anthropicNowSeconds
	anthropicNowSeconds = func() float64 { clockMu.Lock(); defer clockMu.Unlock(); return now }
	t.Cleanup(func() { anthropicNowSeconds = old })
	setNow := func(v float64) { clockMu.Lock(); now = v; clockMu.Unlock() }

	type outcome struct {
		token string
		err   error
	}
	var mu sync.Mutex
	calls := 0
	next := []outcome{{token: "t1"}}
	exchanged := make(chan struct{}, 16)
	cache := &anthropicTokenCache{exchange: func() (anthropicAccessToken, error) {
		mu.Lock()
		o := next[min(calls, len(next)-1)]
		calls++
		mu.Unlock()
		defer func() { exchanged <- struct{}{} }()
		if o.err != nil {
			return anthropicAccessToken{}, o.err
		}
		return anthropicAccessToken{token: o.token, tokenOK: true, expiresAt: anthropicNowSeconds() + 300}, nil
	}}
	get := func(want string, wantErr string) {
		t.Helper()
		got, err := cache.getToken(context.Background())
		if wantErr != "" {
			if err == nil || err.Error() != wantErr {
				t.Fatalf("getToken = %q, %v; want error %q", got, err, wantErr)
			}
			return
		}
		if err != nil || got != want {
			t.Fatalf("getToken = %q, %v; want %q", got, err, want)
		}
	}
	callCount := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	waitExchange := func() {
		t.Helper()
		select {
		case <-exchanged:
		case <-time.After(2 * time.Second):
			t.Fatal("no exchange")
		}
		// Let the refresh settle into the cache.
		time.Sleep(20 * time.Millisecond)
	}

	get("t1", "") // no cached token: exchange
	<-exchanged
	setNow(1000 + 300 - 121) // 121s left: cached, no exchange
	get("t1", "")
	if callCount() != 1 {
		t.Fatalf("%d exchanges at 121s left, want 1", callCount())
	}

	mu.Lock()
	next = []outcome{{err: errors.New("boom")}}
	mu.Unlock()
	setNow(1000 + 300 - 120) // 120s left: advisory, stale token now, refresh behind
	get("t1", "")
	waitExchange()
	if callCount() != 2 {
		t.Fatalf("%d exchanges after the advisory refresh, want 2", callCount())
	}
	setNow(1000 + 300 - 120 + 4) // inside the 5s backoff: no refresh
	get("t1", "")
	time.Sleep(20 * time.Millisecond)
	if callCount() != 2 {
		t.Fatalf("%d exchanges inside the advisory backoff, want 2", callCount())
	}
	setNow(1000 + 300 - 120 + 5) // backoff over: refresh again
	mu.Lock()
	next = []outcome{{token: "t2"}}
	mu.Unlock()
	get("t1", "")
	waitExchange()
	get("t2", "") // the background refresh landed

	// t2 expires at 1185+300 = 1485; 30s left is mandatory.
	mu.Lock()
	next = []outcome{{err: errors.New("down")}}
	mu.Unlock()
	setNow(1485 - 30)
	get("", "down")
	<-exchanged
	mu.Lock()
	next = []outcome{{token: "t3"}}
	mu.Unlock()
	get("t3", "")
	<-exchanged

	cache.invalidate()
	mu.Lock()
	next = []outcome{{token: "t4"}}
	mu.Unlock()
	get("t4", "")
	<-exchanged
}
