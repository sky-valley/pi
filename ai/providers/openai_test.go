package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

const openAISSE = `data: {"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_time","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}

data: [DONE]

`

func TestOpenAIProviderParsesStream(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer sk-test" {
			t.Errorf("bad auth header: %q", r.Header.Get("authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, openAISSE)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL,
		MaxTokens: 1024, Cost: ai.ModelCost{Input: 1, Output: 2},
	}
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools:    []ai.Tool{{Name: "get_time", Description: "time", Parameters: ai.Object()}},
	}
	final := StreamOpenAICompletions(context.Background(), model, req,
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-test"}}).Result()

	if final.StopReason != ai.StopToolUse {
		t.Fatalf("expected toolUse, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
	var gotText string
	var gotTool *ai.ToolCall
	for _, c := range final.Content {
		switch v := c.(type) {
		case ai.TextContent:
			gotText = v.Text
		case ai.ToolCall:
			tc := v
			gotTool = &tc
		}
	}
	if gotText != "Hello" {
		t.Fatalf("text wrong: %q", gotText)
	}
	if gotTool == nil || gotTool.Name != "get_time" {
		t.Fatalf("tool call wrong: %#v", gotTool)
	}
	// pi parseChunkUsage: input excludes cache-read/write tokens.
	// prompt=12, cacheRead=2 => input=10, output=4, total=10+4+2=16.
	if final.Usage.Input != 10 || final.Usage.Output != 4 || final.Usage.CacheRead != 2 {
		t.Fatalf("usage wrong: %+v", final.Usage)
	}
	if final.Usage.TotalTokens != 16 {
		t.Fatalf("total wrong: got %d want 16 (prompt 12 + completion 4)", final.Usage.TotalTokens)
	}
	if gotBody["model"] != "gpt-test" {
		t.Fatalf("request model wrong: %v", gotBody["model"])
	}
}

// runOpenAIStream serves the given SSE body and returns the final assistant
// message. Optional model mutator lets a test tweak the model.
func runOpenAIStream(t *testing.T, sse string, mutate func(*ai.Model)) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	model := &ai.Model{
		ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL,
		Reasoning: true, Cost: ai.ModelCost{Input: 1, Output: 2},
	}
	if mutate != nil {
		mutate(model)
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	return StreamOpenAICompletions(context.Background(), model, req,
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-test"}}).Result()
}

func thinkingSig(m *ai.AssistantMessage) (string, string) {
	for _, c := range m.Content {
		if tc, ok := c.(ai.ThinkingContent); ok {
			return tc.Thinking, tc.ThinkingSignature
		}
	}
	return "", ""
}

func TestOpenAIReasoningFieldVariants(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		wantSig string
	}{
		{"reasoning", "reasoning", "reasoning"},
		{"reasoning_text", "reasoning_text", "reasoning_text"},
		{"reasoning_content", "reasoning_content", "reasoning_content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse := `data: {"choices":[{"delta":{"` + tc.field + `":"thinking..."}}]}

data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
			final := runOpenAIStream(t, sse, nil)
			if final.StopReason != ai.StopStop {
				t.Fatalf("stop reason: %s (%s)", final.StopReason, final.ErrorMessage)
			}
			think, sig := thinkingSig(final)
			if think != "thinking..." {
				t.Fatalf("thinking text wrong: %q", think)
			}
			if sig != tc.wantSig {
				t.Fatalf("thinking signature: got %q want %q", sig, tc.wantSig)
			}
		})
	}
}

func TestOpenAIReasoningFirstNonEmptyWins(t *testing.T) {
	// Both reasoning_content and reasoning present: reasoning_content wins.
	sse := `data: {"choices":[{"delta":{"reasoning_content":"a","reasoning":"a"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	_, sig := thinkingSig(final)
	if sig != "reasoning_content" {
		t.Fatalf("signature: got %q want reasoning_content", sig)
	}
}

func TestOpenAIFinishReasons(t *testing.T) {
	mk := func(reason string) string {
		fr := ""
		if reason != "" {
			fr = `,"finish_reason":"` + reason + `"`
		}
		return `data: {"choices":[{"delta":{"content":"hi"}` + fr + `}]}

data: [DONE]

`
	}
	t.Run("end", func(t *testing.T) {
		final := runOpenAIStream(t, mk("end"), nil)
		if final.StopReason != ai.StopStop {
			t.Fatalf("end: got %s (%s)", final.StopReason, final.ErrorMessage)
		}
	})
	t.Run("content_filter", func(t *testing.T) {
		final := runOpenAIStream(t, mk("content_filter"), nil)
		if final.StopReason != ai.StopError {
			t.Fatalf("content_filter stop: %s", final.StopReason)
		}
		if final.ErrorMessage != "Provider finish_reason: content_filter" {
			t.Fatalf("content_filter msg: %q", final.ErrorMessage)
		}
	})
	t.Run("network_error", func(t *testing.T) {
		final := runOpenAIStream(t, mk("network_error"), nil)
		if final.StopReason != ai.StopError {
			t.Fatalf("network_error stop: %s", final.StopReason)
		}
		if final.ErrorMessage != "Provider finish_reason: network_error" {
			t.Fatalf("network_error msg: %q", final.ErrorMessage)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		final := runOpenAIStream(t, mk("weird_reason"), nil)
		if final.StopReason != ai.StopError {
			t.Fatalf("unknown stop: %s", final.StopReason)
		}
		if final.ErrorMessage != "Provider finish_reason: weird_reason" {
			t.Fatalf("unknown msg: %q", final.ErrorMessage)
		}
	})
	t.Run("missing", func(t *testing.T) {
		final := runOpenAIStream(t, mk(""), nil)
		if final.StopReason != ai.StopError {
			t.Fatalf("missing stop: %s", final.StopReason)
		}
		if final.ErrorMessage != "Stream ended without finish_reason" {
			t.Fatalf("missing msg: %q", final.ErrorMessage)
		}
	})
}

func TestOpenAIChoiceUsageFallbackAndResponseFields(t *testing.T) {
	// Moonshot-style: usage carried in choice.usage; id/model on the chunk.
	sse := `data: {"id":"chatcmpl-xyz","model":"moonshot-v1","choices":[{"delta":{"content":"hi"},"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	if final.ResponseID != "chatcmpl-xyz" {
		t.Fatalf("responseId: %q", final.ResponseID)
	}
	if final.ResponseModel != "moonshot-v1" {
		t.Fatalf("responseModel: %q", final.ResponseModel)
	}
	// input = 20 - 4 = 16, total = 16 + 5 + 4 = 25.
	if final.Usage.Input != 16 || final.Usage.Output != 5 || final.Usage.CacheRead != 4 || final.Usage.TotalTokens != 25 {
		t.Fatalf("usage fallback wrong: %+v", final.Usage)
	}
}

func TestOpenAIOffTriState(t *testing.T) {
	body := func(model *ai.Model) map[string]any {
		var got map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer server.Close()
		model.BaseURL = server.URL
		req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
		StreamOpenAICompletions(context.Background(), model, req,
			&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-test"}}).Result()
		return got
	}
	strp := func(s string) *string { return &s }

	t.Run("off present-null omits reasoning (openrouter)", func(t *testing.T) {
		model := &ai.Model{
			ID: "anthropic/claude", Api: ai.APIOpenAICompletions, Provider: "openrouter",
			Reasoning: true, ThinkingLevelMap: ai.ThinkingLevelMap{"off": nil},
		}
		got := body(model)
		if _, ok := got["reasoning"]; ok {
			t.Fatalf("expected reasoning omitted, got %v", got["reasoning"])
		}
	})
	t.Run("off absent defaults to none (openrouter)", func(t *testing.T) {
		model := &ai.Model{
			ID: "anthropic/claude", Api: ai.APIOpenAICompletions, Provider: "openrouter",
			Reasoning: true,
		}
		got := body(model)
		r, ok := got["reasoning"].(map[string]any)
		if !ok || r["effort"] != "none" {
			t.Fatalf("expected reasoning.effort=none, got %v", got["reasoning"])
		}
	})
	t.Run("off mapped string is sent (openrouter)", func(t *testing.T) {
		model := &ai.Model{
			ID: "anthropic/claude", Api: ai.APIOpenAICompletions, Provider: "openrouter",
			Reasoning: true, ThinkingLevelMap: ai.ThinkingLevelMap{"off": strp("minimal")},
		}
		got := body(model)
		r, ok := got["reasoning"].(map[string]any)
		if !ok || r["effort"] != "minimal" {
			t.Fatalf("expected reasoning.effort=minimal, got %v", got["reasoning"])
		}
	})
}
