package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// runtimeKeyRequest is one request the capture recorded: its phase (a
// prompt's turn or the compaction's summary), and the URL and the headers
// that carry Cloudflare auth.
type runtimeKeyRequest struct {
	Phase   string            `json:"phase"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// runtimeKeyCapture is testdata/runtimekey/runtimekey-1.3.0.json.
type runtimeKeyCapture struct {
	Version   string `json:"version"`
	Scenarios []struct {
		Name        string                   `json:"name"`
		Env         map[string]string        `json:"env"`
		Stored      map[string]ai.Credential `json:"stored"`
		RuntimeKeys map[string]string        `json:"runtimeKeys"`
		Models      [][2]string              `json:"models"`
		Compact     bool                     `json:"compact"`
		Requests    []runtimeKeyRequest      `json:"requests"`
		Errors      []string                 `json:"errors"`
	} `json:"scenarios"`
}

// cloudflareCompletionsSSE is the chat-completions stream the capture's
// fetch answers with.
func cloudflareCompletionsSSE(text string) string {
	chunk := func(body string) string {
		return `data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m",` + body + "}\n\n"
	}
	content, _ := json.Marshal(text)
	return chunk(`"choices":[{"index":0,"delta":{"role":"assistant","content":`+string(content)+`},"finish_reason":null}]`) +
		chunk(`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`) +
		"data: [DONE]\n\n"
}

// cloudflareServer answers every request as the capture's fetch does and
// records it. Models are pointed at it by moving their base URL's host into
// the path (https://api.cloudflare.com/x → <server>/api.cloudflare.com/x),
// placeholders and all, so a recorded request maps back to the URL pi sent.
type cloudflareServer struct {
	*httptest.Server
	mu       sync.Mutex
	phase    string
	requests []runtimeKeyRequest
}

func newCloudflareServer(t *testing.T) *cloudflareServer {
	t.Helper()
	s := &cloudflareServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		headers := map[string]string{}
		for _, name := range []string{"authorization", "x-api-key", "cf-aig-authorization"} {
			if values := r.Header.Values(name); len(values) > 0 {
				headers[name] = values[0]
			}
		}
		s.mu.Lock()
		phase := s.phase
		s.requests = append(s.requests, runtimeKeyRequest{
			Phase: phase, Method: r.Method, URL: "https://" + strings.TrimPrefix(r.URL.RequestURI(), "/"), Headers: headers,
		})
		s.mu.Unlock()
		text := "ok"
		if phase == "compaction" {
			text = "## Goal\nsummary"
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cloudflareCompletionsSSE(text))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *cloudflareServer) setPhase(phase string) {
	s.mu.Lock()
	s.phase = phase
	s.mu.Unlock()
}

// model returns the catalog model with its base URL moved onto the server.
func (s *cloudflareServer) model(t *testing.T, provider, id string) *ai.Model {
	t.Helper()
	model := ai.GetModel(provider, id)
	if model == nil {
		t.Fatalf("%s/%s is not in the catalog", provider, id)
	}
	clone := *model
	clone.BaseURL = s.URL + "/" + strings.TrimPrefix(model.BaseURL, "https://")
	return &clone
}

// TestSessionRequestsResolveAuthAsPi drives a Session through the capture's
// scenarios — a prompt per model (SetModel between them), then a compaction
// summary — and compares every request's URL and auth headers with what pi's
// AgentSession sent. pi's session streams through its model runtime, which
// resolves each request's auth from the runtime's credentials (a runtime API
// key masking a stored credential) and the environment, and fills the
// Cloudflare endpoint from the env that auth resolves; the session holds no
// key. The port's session streams through SessionOptions.Models built over a
// RuntimeCredentials; a scenario with nothing stored and no runtime key also
// runs with no Models at all, through compat, the session a host gets by
// default.
//
// pi summarizes a split turn in two requests, the port in one; every
// compaction request pi sent is identical, so the port's must equal that one.
func TestSessionRequestsResolveAuthAsPi(t *testing.T) {
	data, err := os.ReadFile("testdata/runtimekey/runtimekey-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture runtimeKeyCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Scenarios) == 0 {
		t.Fatal("runtimekey capture has no scenarios")
	}
	for _, scenario := range capture.Scenarios {
		withModels := []bool{true}
		if len(scenario.Stored) == 0 && len(scenario.RuntimeKeys) == 0 {
			withModels = append(withModels, false)
		}
		for _, useModels := range withModels {
			name := scenario.Name
			if !useModels {
				name += "/compat"
			}
			t.Run(name, func(t *testing.T) {
				for _, variable := range []string{"CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"} {
					t.Setenv(variable, scenario.Env[variable])
				}
				server := newCloudflareServer(t)
				var models ai.Models
				if useModels {
					store := ai.NewInMemoryCredentialStore()
					for provider, credential := range scenario.Stored {
						stored := credential
						if _, err := store.Modify(context.Background(), provider, func(*ai.Credential) (*ai.Credential, error) { return &stored, nil }); err != nil {
							t.Fatal(err)
						}
					}
					credentials := NewRuntimeCredentials(store)
					for provider, key := range scenario.RuntimeKeys {
						credentials.SetRuntimeApiKey(provider, key)
					}
					models = ai.BuiltinModels(&ai.CreateModelsOptions{Credentials: credentials})
				}
				first := server.model(t, scenario.Models[0][0], scenario.Models[0][1])
				sess := newTestSession(t, SessionOptions{Model: first, Cwd: t.TempDir(), NoTools: NoToolsAll, Models: models})

				var errs []string
				prompt := func(text string) {
					server.setPhase("prompt")
					res, err := sess.Run(context.Background(), text)
					if err != nil {
						errs = append(errs, err.Error())
					} else if res.ErrorMessage != "" {
						errs = append(errs, res.ErrorMessage)
					}
				}
				prompt("q1")
				for _, next := range scenario.Models[1:] {
					sess.SetModel(server.model(t, next[0], next[1]))
					prompt("q2")
				}
				if scenario.Compact {
					prompt("q3")
					server.setPhase("compaction")
					if summary := sess.summarize(context.Background(), sess.History()); summary == "" {
						errs = append(errs, "compact: no summary")
					}
				}

				var want, got struct{ prompts, compaction []runtimeKeyRequest }
				for _, request := range scenario.Requests {
					if request.Phase == "compaction" {
						want.compaction = append(want.compaction, request)
					} else {
						want.prompts = append(want.prompts, request)
					}
				}
				for _, request := range server.requests {
					if request.Phase == "compaction" {
						got.compaction = append(got.compaction, request)
					} else {
						got.prompts = append(got.prompts, request)
					}
				}
				if !reflect.DeepEqual(got.prompts, want.prompts) {
					t.Errorf("prompt requests:\n go %s\n pi %s", describeRequests(got.prompts), describeRequests(want.prompts))
				}
				if len(want.compaction) > 0 {
					for _, request := range want.compaction[1:] {
						if !reflect.DeepEqual(request, want.compaction[0]) {
							t.Fatalf("pi's compaction requests differ: %s", describeRequests(want.compaction))
						}
					}
					if len(got.compaction) == 0 || !reflect.DeepEqual(got.compaction, want.compaction[:1]) {
						t.Errorf("compaction requests:\n go %s\n pi %s", describeRequests(got.compaction), describeRequests(want.compaction[:1]))
					}
				}
				if len(errs) != len(scenario.Errors) {
					t.Errorf("errors: go %q, pi %q", errs, scenario.Errors)
				}
			})
		}
	}
}

func describeRequests(requests []runtimeKeyRequest) string {
	var parts []string
	for _, r := range requests {
		parts = append(parts, fmt.Sprintf("%s %s %v", r.Method, r.URL, r.Headers))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}
