package coding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// TestSDKOptionsReachProvider drives a real Session → Agent → OpenAI provider
// against a mock server and asserts temperature/maxTokens/headers/onPayload
// all make it onto the wire.
func TestSDKOptionsReachProvider(t *testing.T) {
	providers.RegisterOpenAICompletions()

	var gotBody map[string]any
	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("OpenAI-Organization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-4.1", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 4096}
	temp := 0.3
	maxTok := 256
	onPayloadCalled := false

	sess := NewSession(SessionOptions{
		Model:       model,
		Cwd:         t.TempDir(),
		NoTools:     NoToolsAll,
		APIKey:      "k",
		Temperature: &temp,
		MaxTokens:   &maxTok,
		Headers:     map[string]string{"OpenAI-Organization": "org-123"},
		OnPayload: func(payload any, m *ai.Model) (any, error) {
			onPayloadCalled = true
			return payload, nil
		},
	})

	if _, err := sess.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	if v, _ := gotBody["temperature"].(float64); v != 0.3 {
		t.Fatalf("temperature not sent: %v", gotBody["temperature"])
	}
	if v, _ := gotBody["max_completion_tokens"].(float64); v != 256 {
		t.Fatalf("max_completion_tokens not sent: %v keys=%v", gotBody["max_completion_tokens"], keysOfCoding(gotBody))
	}
	if gotHeader != "org-123" {
		t.Fatalf("custom header not sent: %q", gotHeader)
	}
	if !onPayloadCalled {
		t.Fatal("OnPayload hook not invoked")
	}
}

func keysOfCoding(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestProviderErrorFormatting(t *testing.T) {
	providers.RegisterOpenAICompletions()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"Invalid 'model' parameter","code":"model_not_found"}}`)
	}))
	defer server.Close()

	model := &ai.Model{ID: "bad", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, APIKey: "k", MaxRetries: 1})
	_, err := sess.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "Invalid 'model' parameter") || !strings.Contains(err.Error(), "model_not_found") {
		t.Fatalf("expected parsed provider error, got: %v", err)
	}
}
