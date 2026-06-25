package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func captureOpenAIBody(t *testing.T, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) map[string]any {
	t.Helper()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model.BaseURL = server.URL
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	opts.APIKey = "k"
	StreamSimpleOpenAICompletions(context.Background(), model, req, opts).Result()
	return gotBody
}

func TestOpenAICompatReasoningModel(t *testing.T) {
	model := &ai.Model{ID: "gpt-5-mini", Api: ai.APIOpenAICompletions, Provider: "openai", Reasoning: true, MaxTokens: 2048}
	mt := 2048
	body := captureOpenAIBody(t, model,
		ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&ai.SimpleStreamOptions{Reasoning: ai.ThinkingHigh, StreamOptions: ai.StreamOptions{MaxTokens: &mt}})

	// Reasoning OpenAI model: developer role, max_completion_tokens, store:false, reasoning_effort.
	msgs, _ := body["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("expected developer role for reasoning model, got %v", first["role"])
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Fatalf("expected max_completion_tokens, got keys %v", keysOf(body))
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("should not send max_tokens for OpenAI")
	}
	if body["store"] != false {
		t.Fatalf("expected store:false, got %v", body["store"])
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort high, got %v", body["reasoning_effort"])
	}
}

func TestOpenAICompatNonReasoningUsesSystemRole(t *testing.T) {
	model := &ai.Model{ID: "gpt-4.1", Api: ai.APIOpenAICompletions, Provider: "openai", Reasoning: false, MaxTokens: 2048}
	body := captureOpenAIBody(t, model,
		ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}, nil)
	msgs, _ := body["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("expected system role for non-reasoning model, got %v", first["role"])
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("non-reasoning model must not send reasoning_effort")
	}
}

func TestOpenAICompatTogetherUsesMaxTokensAndReasoningObject(t *testing.T) {
	model := &ai.Model{ID: "x", Api: ai.APIOpenAICompletions, Provider: "together", Reasoning: true, MaxTokens: 1000}
	mt := 1000
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		// Match pi: a max-token field is only sent when explicitly requested; this
		// test pins the *field choice* (max_tokens vs max_completion_tokens).
		&ai.SimpleStreamOptions{Reasoning: ai.ThinkingMedium, StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if _, ok := body["max_tokens"]; !ok {
		t.Fatalf("together should use max_tokens, got %v", keysOf(body))
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("together must not use max_completion_tokens")
	}
	if _, ok := body["reasoning"]; !ok {
		t.Fatalf("together should send a reasoning object, got %v", keysOf(body))
	}
	// together does not support reasoning_effort.
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("together must not send reasoning_effort")
	}
}

func TestOpenAICompatDeepSeekReasoningContent(t *testing.T) {
	model := &ai.Model{ID: "deepseek-reasoner", Api: ai.APIOpenAICompletions, Provider: "deepseek", Reasoning: true, MaxTokens: 1000}
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "earlier"}}, Provider: "deepseek", Api: ai.APIOpenAICompletions, Model: "deepseek-reasoner", StopReason: ai.StopStop, Timestamp: 2},
			ai.NewUserText("again", 3),
		}},
		&ai.SimpleStreamOptions{Reasoning: ai.ThinkingHigh})
	// deepseek thinkingFormat -> thinking:{type:enabled}, and assistant messages carry reasoning_content.
	if _, ok := body["thinking"]; !ok {
		t.Fatalf("deepseek should send thinking, got %v", keysOf(body))
	}
	msgs, _ := body["messages"].([]any)
	foundReasoningContent := false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			if _, ok := mm["reasoning_content"]; ok {
				foundReasoningContent = true
			}
		}
	}
	if !foundReasoningContent {
		t.Fatalf("deepseek assistant messages should include reasoning_content")
	}
}

// Mirrors the upstream openai-completions-empty-tools.test.ts additions
// (09f10595): streamSimple now sends a context-clamped default maxTokens.
func TestOpenAICompatStreamSimpleSendsClampedDefaultMaxTokens(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", MaxTokens: 8000, ContextWindow: 10000}
	// 8000 'x' user chars -> estimate ceil(8000/4)=2000; available = 10000-2000-4096 = 3904.
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText(strings.Repeat("x", 8000), 1)}}, nil)
	if v, _ := body["max_completion_tokens"].(float64); v != 3904 {
		t.Fatalf("default max_completion_tokens = %v, want 3904", body["max_completion_tokens"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("OpenAI must not send max_tokens")
	}
}

func TestOpenAICompatStreamSimpleClampsExplicitMaxTokens(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", MaxTokens: 8000, ContextWindow: 10000}
	mt := 7000
	body := captureOpenAIBody(t, model,
		ai.Context{Messages: []ai.Message{ai.NewUserText(strings.Repeat("x", 8000), 1)}},
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{MaxTokens: &mt}})
	if v, _ := body["max_completion_tokens"].(float64); v != 3904 {
		t.Fatalf("explicit clamped max_completion_tokens = %v, want 3904", body["max_completion_tokens"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
