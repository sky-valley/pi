package providers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Ports packages/ai/test/cloudflare-ai-binding.test.ts at d2e7d8b7c
// (upstream 55adba4f2).

const aiBindingPrefix = "https://workers-binding.ai/ai-gateway/gateways/my-gateway"

// fakeAIBinding records every request and answers with one response, as the
// suite's fakeBinding does.
type fakeAIBinding struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	response *http.Response
}

func (b *fakeAIBinding) Fetch(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = string(data)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, req)
	b.bodies = append(b.bodies, body)
	if b.response != nil {
		return b.response, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func TestAIBindingFetchPassesRequestsUntouched(t *testing.T) {
	bindingResponse := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "Cf-Aig-Log-Id": []string{"log-1"}},
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}
	binding := &fakeAIBinding{response: bindingResponse}
	doer, err := CreateAIBindingFetch(binding)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, aiBindingPrefix+"/anthropic/v1/messages?beta=true", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cf-aig-authorization", "Bearer "+CloudflareGatewayBindingAuthSentinel)
	req.Header.Set("anthropic-version", "2023-06-01")

	response, err := doer.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if len(binding.requests) != 1 {
		t.Fatalf("binding saw %d requests, want 1", len(binding.requests))
	}
	got := binding.requests[0]
	if got.URL.String() != aiBindingPrefix+"/anthropic/v1/messages?beta=true" || got.Method != http.MethodPost {
		t.Errorf("binding request = %s %s, want POST %s/anthropic/v1/messages?beta=true", got.Method, got.URL, aiBindingPrefix)
	}
	// The gateway recognizes and strips the sentinel itself; nothing here
	// touches headers.
	want := http.Header{
		"Content-Type":         {"application/json"},
		"Cf-Aig-Authorization": {"Bearer " + CloudflareGatewayBindingAuthSentinel},
		"Anthropic-Version":    {"2023-06-01"},
	}
	if !reflect.DeepEqual(got.Header, want) {
		t.Errorf("binding headers = %v, want %v", got.Header, want)
	}
	if binding.bodies[0] != body {
		t.Errorf("binding body = %q, want %q", binding.bodies[0], body)
	}
	if response != bindingResponse {
		t.Fatal("the binding's response must come back as it is")
	}
	if data, _ := io.ReadAll(response.Body); string(data) != "data: {}\n\n" {
		t.Errorf("response body = %q", data)
	}
}

// pi rejects a binding with no fetch() at construction, not on first
// request; a nil AIBinding is Go's binding without fetch.
func TestCreateAIBindingFetchRejectsMissingBinding(t *testing.T) {
	doer, err := CreateAIBindingFetch(nil)
	if err == nil || doer != nil {
		t.Fatalf("CreateAIBindingFetch(nil) = %v, %v; want an error", doer, err)
	}
	if !strings.Contains(err.Error(), "does not expose fetch()") {
		t.Fatalf("error = %q, want pi's %q", err, "does not expose fetch()")
	}
}

// The full header contract from the module docs, end to end through a real
// API implementation: the sentinel satisfies the request-auth check, and the
// nil entries keep the SDK's placeholder auth headers off the wire.
func TestAIBindingFetchKeepsPlaceholderAuthOffTheWire(t *testing.T) {
	binding := &fakeAIBinding{response: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"bad_request","message":"stubbed"}}`)),
	}}
	doer, err := CreateAIBindingFetch(binding)
	if err != nil {
		t.Fatal(err)
	}
	model := &ai.Model{
		ID: "test-model", Name: "Test Model", Api: ai.APIOpenAICompletions, Provider: "openai",
		BaseURL: aiBindingPrefix + "/openai", Input: []string{"text"}, ContextWindow: 10_000, MaxTokens: 1_000,
	}
	sentinel := "Bearer " + CloudflareGatewayBindingAuthSentinel
	result := StreamSimpleOpenAICompletions(context.Background(), model,
		ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hello", 1)}}),
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
			Headers:    ai.ProviderHeaders{"cf-aig-authorization": &sentinel, "Authorization": nil, "x-api-key": nil},
			HTTPClient: doer,
			MaxRetries: 0,
		}}}).Result()

	if result.StopReason != ai.StopError {
		t.Fatalf("stopReason = %q, want error", result.StopReason)
	}
	if len(binding.requests) != 1 {
		t.Fatalf("binding saw %d requests, want 1", len(binding.requests))
	}
	if got := binding.requests[0].URL.String(); got != aiBindingPrefix+"/openai/chat/completions" {
		t.Errorf("request URL = %s, want %s/openai/chat/completions", got, aiBindingPrefix)
	}
	for _, banned := range []string{"Authorization", "X-Api-Key"} {
		if _, ok := binding.requests[0].Header[banned]; ok {
			t.Errorf("header %s reached the binding; the nil entry must suppress it", banned)
		}
	}
}
