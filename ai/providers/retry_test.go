package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestProviderRetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, openAISSE)
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	final := StreamOpenAICompletions(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()

	if final.StopReason == ai.StopError {
		t.Fatalf("expected success after retry, got error: %s", final.ErrorMessage)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts (1 retry), got %d", calls)
	}
}

func TestProviderStopsRetryingPastLimit(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"unavailable"}`)
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	maxRetries := 1
	final := StreamOpenAICompletions(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: maxRetries}}).Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("expected error after exhausting retries, got %s", final.StopReason)
	}
	// maxRetries=1 => 2 attempts total.
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

func TestRetryAfterExceedingCapSurfacesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600") // 1 hour, exceeds the 60s cap
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"long wait"}`)
	}))
	defer server.Close()

	model := &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	final := StreamOpenAICompletions(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	// Should fail fast with the 429 surfaced rather than blocking for an hour.
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error surfaced, got %s", final.StopReason)
	}
}

func TestResponsesPromptCacheKey(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = parseJSONWithRepair(string(body), &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"r","status":"completed"}}`+"\n\n")
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	StreamOpenAIResponses(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "sess-123", CacheRetention: ai.CacheShort}}).Result()

	if gotBody["prompt_cache_key"] != "sess-123" {
		t.Fatalf("prompt_cache_key not sent: %v", gotBody["prompt_cache_key"])
	}
}
