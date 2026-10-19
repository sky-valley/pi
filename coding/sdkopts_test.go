package coding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	sess := newTestSession(t, SessionOptions{
		Model:       model,
		Cwd:         t.TempDir(),
		NoTools:     NoToolsAll,
		APIKey:      "k",
		Temperature: &temp,
		MaxTokens:   &maxTok,
		Headers:     ai.ProviderHeaders{"OpenAI-Organization": ai.HeaderValue("org-123")},
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

// TestSDKOptionsForwardProviderStreamEvents transliterates upstream's 'forwards
// provider stream events to extensions' (sdk-stream-options.test.ts,
// 002fc8385) onto the native hook: the stream options carry an observer, and
// what a provider hands it reaches SessionOptions.OnProviderStreamEvent with
// the model whose Provider/Api/ID are the extension event's
// provider/api/model.
func TestSDKOptionsForwardProviderStreamEvents(t *testing.T) {
	providerEvent := map[string]any{"openrouter_metadata": map[string]any{"strategy": "direct"}}
	model := &ai.Model{ID: "capture-model", Api: ai.APIOpenAICompletions, Provider: "capture-provider", MaxTokens: 4096}

	type event struct {
		data                   any
		provider, api, modelID string
	}
	var events []event
	var hadObserver bool
	sess := newTestSession(t, SessionOptions{
		Model:   model,
		Cwd:     t.TempDir(),
		NoTools: NoToolsAll,
		APIKey:  "k",
		OnProviderStreamEvent: func(data any, m *ai.Model) error {
			events = append(events, event{data: data, provider: m.Provider, api: string(m.Api), modelID: m.ID})
			return nil
		},
		StreamFn: func(ctx context.Context, m *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			s := ai.NewAssistantMessageEventStream()
			go func() {
				hadObserver = opts.OnProviderStreamEvent != nil
				if hadObserver {
					if err := opts.OnProviderStreamEvent(providerEvent, m); err != nil {
						t.Errorf("observer: %v", err)
					}
				}
				msg := &ai.AssistantMessage{
					Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Api: m.Api, Provider: m.Provider,
					Model: m.ID, StopReason: ai.StopStop, Timestamp: 1,
				}
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ai.StopStop, Message: msg})
				s.End()
			}()
			return s
		},
	})

	if _, err := sess.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !hadObserver {
		t.Fatal("the stream options carry no OnProviderStreamEvent")
	}
	want := []event{{data: providerEvent, provider: "capture-provider", api: "openai-completions", modelID: "capture-model"}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
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
	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, APIKey: "k", MaxRetries: 1})
	_, err := sess.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "Invalid 'model' parameter") || !strings.Contains(err.Error(), "model_not_found") {
		t.Fatalf("expected parsed provider error, got: %v", err)
	}
}
