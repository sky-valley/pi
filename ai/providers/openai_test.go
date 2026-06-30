package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestOpenAICompletionsReasoningTokens checks that completion_tokens_details.
// reasoning_tokens populates Usage.Reasoning, and that absence yields 0 (pi's
// `reasoning: ... || 0`).
func TestOpenAICompletionsReasoningTokens(t *testing.T) {
	withReasoning := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":40,\"completion_tokens_details\":{\"reasoning_tokens\":33}}}\n\n" +
		"data: [DONE]\n\n"
	final := runOpenAIStream(t, withReasoning, nil)
	if final.Usage.Reasoning != 33 {
		t.Fatalf("expected reasoning 33, got %+v", final.Usage)
	}
	if final.Usage.Output != 40 {
		t.Fatalf("output wrong: %+v", final.Usage)
	}

	withoutReasoning := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":40}}\n\n" +
		"data: [DONE]\n\n"
	final2 := runOpenAIStream(t, withoutReasoning, nil)
	if final2.Usage.Reasoning != 0 {
		t.Fatalf("expected reasoning 0 when absent, got %+v", final2.Usage)
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

// collectOpenAIEvents serves the SSE body and returns every event plus the
// final message.
func collectOpenAIEvents(t *testing.T, sse string, mutate func(*ai.Model)) ([]ai.AssistantMessageEvent, *ai.AssistantMessage) {
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
	stream := StreamOpenAICompletions(context.Background(), model, req,
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-test"}})
	var events []ai.AssistantMessageEvent
	for e := range stream.Events() {
		events = append(events, e)
	}
	return events, stream.Result()
}

func eventTypes(events []ai.AssistantMessageEvent) []ai.EventType {
	out := make([]ai.EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// ---- A3: delta processing order (pi openai-completions.ts:299-385) ----

func TestOpenAIDeltaOrderContentBeforeReasoning(t *testing.T) {
	// One chunk carrying BOTH content and reasoning: pi processes content first,
	// so the text block starts (and sits) before the thinking block.
	sse := `data: {"choices":[{"delta":{"content":"hi","reasoning_content":"think"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	events, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopStop {
		t.Fatalf("stop reason: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if len(final.Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(final.Content))
	}
	if _, ok := final.Content[0].(ai.TextContent); !ok {
		t.Fatalf("content[0] should be text, got %T", final.Content[0])
	}
	if _, ok := final.Content[1].(ai.ThinkingContent); !ok {
		t.Fatalf("content[1] should be thinking, got %T", final.Content[1])
	}
	textStart, thinkingStart := -1, -1
	for i, e := range events {
		if e.Type == ai.EventTextStart && textStart == -1 {
			textStart = i
		}
		if e.Type == ai.EventThinkingStart && thinkingStart == -1 {
			thinkingStart = i
		}
	}
	if textStart == -1 || thinkingStart == -1 || textStart > thinkingStart {
		t.Fatalf("text_start must precede thinking_start: %v", eventTypes(events))
	}
}

// ---- A4: finalize once, after the whole stream, in block order ----

func TestOpenAIFinalizeOnceAfterStreamWithUsage(t *testing.T) {
	// Repeated finish_reason chunks must not duplicate *_end events; ends come
	// after the usage-only final chunk so Partial snapshots carry final usage;
	// ends are in block (content) order: thinking first here.
	sse := `data: {"choices":[{"delta":{"reasoning_content":"think"}}]}

data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}

data: [DONE]

`
	events, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopStop {
		t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	var ends []ai.AssistantMessageEvent
	for _, e := range events {
		if e.Type == ai.EventTextEnd || e.Type == ai.EventThinkingEnd {
			ends = append(ends, e)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("expected exactly 2 end events (no dupes on repeated finish_reason), got %v", eventTypes(events))
	}
	if ends[0].Type != ai.EventThinkingEnd || ends[1].Type != ai.EventTextEnd {
		t.Fatalf("ends must follow block order (thinking,text), got %s,%s", ends[0].Type, ends[1].Type)
	}
	// Partial snapshots at end-time include the final usage (the usage-only
	// chunk arrives before pi finalizes blocks).
	for _, e := range ends {
		if e.Partial == nil || e.Partial.Usage.Input != 9 || e.Partial.Usage.Output != 3 {
			t.Fatalf("end-event partial must carry final usage, got %+v", e.Partial.Usage)
		}
	}
}

func TestOpenAIMissingFinishReasonEndsBlocksThenErrors(t *testing.T) {
	// pi finalizes blocks (:389-391) BEFORE throwing "Stream ended without
	// finish_reason" (:402-404): consumers see text_end, then the error event.
	sse := `data: {"choices":[{"delta":{"content":"partial"}}]}

data: [DONE]

`
	events, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopError || final.ErrorMessage != "Stream ended without finish_reason" {
		t.Fatalf("expected finish_reason error, got %s / %q", final.StopReason, final.ErrorMessage)
	}
	textEnd, errIdx := -1, -1
	for i, e := range events {
		if e.Type == ai.EventTextEnd {
			textEnd = i
		}
		if e.Type == ai.EventError {
			errIdx = i
		}
	}
	if textEnd == -1 || errIdx == -1 || textEnd > errIdx {
		t.Fatalf("text_end must precede the error event: %v", eventTypes(events))
	}
}

// ---- A5: zero-choice stream errors like pi (no sawChoice guard) ----

func TestOpenAIZeroChoiceStreamErrors(t *testing.T) {
	_, final := collectOpenAIEvents(t, "data: [DONE]\n\n", nil)
	if final.StopReason != ai.StopError || final.ErrorMessage != "Stream ended without finish_reason" {
		t.Fatalf("zero-choice stream must error like pi, got %s / %q", final.StopReason, final.ErrorMessage)
	}
}

// ---- C1: tool-call id normalizer (pi openai-completions.ts:753-768) ----

func TestOpenAIToolCallIDNormalization(t *testing.T) {
	captureBody := func(model *ai.Model, msgs []ai.Message) map[string]any {
		t.Helper()
		return buildOpenAIParams(model, ai.Context{Messages: msgs}, &OpenAIOptions{})
	}
	mkTurn := func(id, provider, modelID string) []ai.Message {
		return []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{
				Content:  ai.ContentList{ai.ToolCall{ID: id, Name: "f", Arguments: map[string]any{}}},
				Provider: ai.ProviderId(provider), Api: ai.APIOpenAICompletions, Model: modelID,
				StopReason: ai.StopToolUse,
			},
			ai.ToolResultMessage{ToolCallID: id, ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
		}
	}
	findToolCallIDs := func(body map[string]any) (callID, resultID string) {
		msgs, _ := body["messages"].([]map[string]any)
		for _, m := range msgs {
			if tcs, ok := m["tool_calls"].([]map[string]any); ok && len(tcs) > 0 {
				callID, _ = tcs[0]["id"].(string)
			}
			if m["role"] == "tool" {
				resultID, _ = m["tool_call_id"].(string)
			}
		}
		return
	}

	t.Run("pipe id sanitized and clamped", func(t *testing.T) {
		model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1"}
		body := captureBody(model, mkTurn("call_abc+/=123|ENCRYPTEDPAYLOAD", "github-copilot", "gpt-5"))
		callID, resultID := findToolCallIDs(body)
		want := "call_abc___123"
		if callID != want {
			t.Fatalf("tool call id = %q, want %q", callID, want)
		}
		if resultID != want {
			t.Fatalf("tool result id = %q, want %q (must follow the normalized id)", resultID, want)
		}
	})
	t.Run("long non-pipe id truncated for provider openai cross-model", func(t *testing.T) {
		model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1"}
		longID := strings.Repeat("a", 50)
		body := captureBody(model, mkTurn(longID, "openai", "gpt-other"))
		callID, _ := findToolCallIDs(body)
		if callID != strings.Repeat("a", 40) {
			t.Fatalf("long id should be truncated to 40, got %d chars", len(callID))
		}
	})
	t.Run("same-model id passes through untouched", func(t *testing.T) {
		model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1"}
		longID := strings.Repeat("a", 50)
		body := captureBody(model, mkTurn(longID, "openai", "gpt-test"))
		callID, _ := findToolCallIDs(body)
		if callID != longID {
			t.Fatalf("same-model id must pass through, got %q", callID)
		}
	})
	t.Run("non-pipe id untouched for non-openai provider", func(t *testing.T) {
		model := &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1"}
		longID := strings.Repeat("b", 50)
		body := captureBody(model, mkTurn(longID, "openai", "gpt-other"))
		callID, _ := findToolCallIDs(body)
		if callID != longID {
			t.Fatalf("non-openai provider must not truncate non-pipe ids, got %q", callID)
		}
	})
}

// ---- C2: streamed reasoning_details captured as thoughtSignature ----

func TestOpenAIReasoningDetailsStreamCapture(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","data":"ENC"}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	var tc *ai.ToolCall
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok {
			tc = &v
		}
	}
	wantSig := `{"type":"reasoning.encrypted","id":"call_1","data":"ENC"}`
	if tc == nil || tc.ThoughtSignature != wantSig {
		t.Fatalf("thoughtSignature = %#v, want %q", tc, wantSig)
	}

	// Round trip: replaying this assistant turn emits reasoning_details.
	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1"}
	body := buildOpenAIParams(model, ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		*final,
		ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	}}, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	var rd []any
	for _, m := range msgs {
		if v, ok := m["reasoning_details"].([]any); ok {
			rd = v
		}
	}
	if len(rd) != 1 {
		t.Fatalf("replay should emit 1 reasoning_details entry, got %#v", rd)
	}
	d, _ := rd[0].(map[string]any)
	if d["type"] != "reasoning.encrypted" || d["id"] != "call_1" || d["data"] != "ENC" {
		t.Fatalf("reasoning_details entry wrong: %v", d)
	}
}

// upstream 7d0497fd (#5114): an encrypted reasoning_detail can stream in a
// delta BEFORE the tool-call block carrying its id is created. It must be
// buffered and attached on block creation, not dropped.
func TestOpenAIReasoningDetailsEarlyArrival(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","data":"ENC"}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	var tc *ai.ToolCall
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok {
			tc = &v
		}
	}
	wantSig := `{"type":"reasoning.encrypted","id":"call_1","data":"ENC"}`
	if tc == nil || tc.ThoughtSignature != wantSig {
		t.Fatalf("early reasoning detail not attached: thoughtSignature = %#v, want %q", tc, wantSig)
	}
}

// upstream 7d0497fd tightened the guard to isEncryptedReasoningDetail: data
// must be a non-empty STRING. A numeric/object/null data is rejected even when
// type and id are valid.
func TestOpenAIReasoningDetailsNonStringDataIgnored(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","data":123},{"type":"reasoning.encrypted","id":"call_1","data":{"k":"v"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok && v.ThoughtSignature != "" {
			t.Fatalf("non-string reasoning_detail data must not set thoughtSignature, got %q", v.ThoughtSignature)
		}
	}
}

func TestOpenAIReasoningDetailsNoMatchIgnored(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f"}}]}}]}

data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"other","data":"ENC"},{"type":"reasoning.text","id":"call_1","data":"ENC"},{"type":"reasoning.encrypted","id":"call_1","data":""}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok && v.ThoughtSignature != "" {
			t.Fatalf("no matching/valid detail should set thoughtSignature, got %q", v.ThoughtSignature)
		}
	}
}

// ---- C3: tool-call tracking byIndex AND byId (pi :229-265) ----

func TestOpenAIToolCallsByIdWithoutIndex(t *testing.T) {
	// Two id-keyed deltas with no index must create two separate tool calls.
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"fa","arguments":"{\"x\":1}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"fb","arguments":"{\"y\":2}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	var calls []ai.ToolCall
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok {
			calls = append(calls, v)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 separate tool calls for id-keyed deltas without index, got %d", len(calls))
	}
	if calls[0].ID != "call_a" || calls[1].ID != "call_b" {
		t.Fatalf("ids wrong: %q, %q", calls[0].ID, calls[1].ID)
	}
}

func TestOpenAIToolCallIdFallbackMergesContinuation(t *testing.T) {
	// First delta carries index+id; continuation carries only the id (no index):
	// the byId fallback must merge it into the same block.
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"fa","arguments":"{\"x\""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":":1}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	var calls []ai.ToolCall
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok {
			calls = append(calls, v)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("continuation should merge by id, got %d calls", len(calls))
	}
	if v, _ := calls[0].Arguments["x"].(float64); v != 1 {
		t.Fatalf("arguments not merged: %#v", calls[0].Arguments)
	}
}

// ---- C4: cloudflare {VAR} baseURL resolution (pi :490) ----

func TestOpenAICloudflareBaseURLResolved(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct123")
	model := &ai.Model{
		ID: "m", Api: ai.APIOpenAICompletions, Provider: "cloudflare-workers-ai",
		BaseURL: server.URL + "/client/v4/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai/v1",
	}
	final := StreamOpenAICompletions(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if gotPath != "/client/v4/accounts/acct123/ai/v1/chat/completions" {
		t.Fatalf("placeholder not resolved, path = %q", gotPath)
	}
}

func TestOpenAICloudflareBaseURLMissingEnvFailsStream(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	model := &ai.Model{
		ID: "m", Api: ai.APIOpenAICompletions, Provider: "cloudflare-workers-ai",
		BaseURL: "https://api.cloudflare.com/client/v4/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai/v1",
	}
	final := StreamOpenAICompletions(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	want := "CLOUDFLARE_ACCOUNT_ID is required for provider cloudflare-workers-ai but is not set."
	if final.ErrorMessage != want {
		t.Fatalf("error message = %q, want %q", final.ErrorMessage, want)
	}
}

// ---- C5: prompt_cache_retention independent of sessionId (pi :515) ----

func TestOpenAICacheRetentionWithoutSessionID(t *testing.T) {
	body := buildOpenAIParams(openAIModel(nil), baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{CacheRetention: ai.CacheLong}})
	if body["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention must be sent without a sessionId, got %v", body["prompt_cache_retention"])
	}
	if has(body, "prompt_cache_key") {
		t.Fatalf("prompt_cache_key still requires a sessionId, got %v", body["prompt_cache_key"])
	}
}

// ---- C6: explicit cached_tokens:0 beats prompt_cache_hit_tokens fallback ----

func TestOpenAICachedTokensExplicitZero(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	if final.Usage.CacheRead != 0 {
		t.Fatalf("explicit cached_tokens:0 must beat prompt_cache_hit_tokens fallback, got cacheRead=%d", final.Usage.CacheRead)
	}
	if final.Usage.Input != 10 {
		t.Fatalf("input = %d, want 10", final.Usage.Input)
	}
}

func TestOpenAICachedTokensAbsentFallsBack(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":5}}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	if final.Usage.CacheRead != 5 {
		t.Fatalf("absent cached_tokens must fall back to prompt_cache_hit_tokens, got cacheRead=%d", final.Usage.CacheRead)
	}
	if final.Usage.Input != 5 {
		t.Fatalf("input = %d, want 5", final.Usage.Input)
	}
}

// ---- C7: toolcall_delta pushed for every delta entry; id/name first-wins ----

func TestOpenAIToolCallDeltaPushedForArglessDeltas(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	events, _ := collectOpenAIEvents(t, sse, nil)
	var deltas []ai.AssistantMessageEvent
	for _, e := range events {
		if e.Type == ai.EventToolCallDelta {
			deltas = append(deltas, e)
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("expected a toolcall_delta for EVERY delta entry, got %d (%v)", len(deltas), eventTypes(events))
	}
	if deltas[0].Delta != "" {
		t.Fatalf("argless delta must carry empty delta string, got %q", deltas[0].Delta)
	}
	if deltas[1].Delta != "{}" {
		t.Fatalf("second delta = %q, want {}", deltas[1].Delta)
	}
}

func TestOpenAIToolCallIdNameFirstWins(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_first","function":{"name":"name_first"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_second","function":{"name":"name_second","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	_, final := collectOpenAIEvents(t, sse, nil)
	var calls []ai.ToolCall
	for _, c := range final.Content {
		if v, ok := c.(ai.ToolCall); ok {
			calls = append(calls, v)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "call_first" || calls[0].Name != "name_first" {
		t.Fatalf("id/name must be first-wins, got %q/%q", calls[0].ID, calls[0].Name)
	}
}

// ---- C8: github-copilot dynamic headers (pi :459-466) ----

func TestOpenAICopilotDynamicHeaders(t *testing.T) {
	run := func(msgs []ai.Message, optHeaders map[string]string) http.Header {
		t.Helper()
		var gotHeaders http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer server.Close()
		model := &ai.Model{
			ID: "gpt-5", Api: ai.APIOpenAICompletions, Provider: "github-copilot", BaseURL: server.URL,
			Input: []string{"text", "image"},
		}
		StreamOpenAICompletions(context.Background(), model, ai.Context{Messages: msgs},
			&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", Headers: optHeaders}}).Result()
		return gotHeaders
	}

	t.Run("user initiated", func(t *testing.T) {
		h := run([]ai.Message{ai.NewUserText("hi", 1)}, nil)
		if h.Get("X-Initiator") != "user" {
			t.Fatalf("X-Initiator = %q, want user", h.Get("X-Initiator"))
		}
		if h.Get("Openai-Intent") != "conversation-edits" {
			t.Fatalf("Openai-Intent = %q", h.Get("Openai-Intent"))
		}
		if h.Get("Copilot-Vision-Request") != "" {
			t.Fatalf("Copilot-Vision-Request should be absent without images")
		}
	})
	t.Run("agent initiated with vision", func(t *testing.T) {
		msgs := []ai.Message{
			ai.UserMessage{Content: ai.ContentList{ai.ImageContent{MimeType: "image/png", Data: "B64"}}, Timestamp: 1},
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop},
		}
		h := run(msgs, nil)
		if h.Get("X-Initiator") != "agent" {
			t.Fatalf("X-Initiator = %q, want agent", h.Get("X-Initiator"))
		}
		if h.Get("Copilot-Vision-Request") != "true" {
			t.Fatalf("Copilot-Vision-Request = %q, want true", h.Get("Copilot-Vision-Request"))
		}
	})
	t.Run("options headers win over copilot headers", func(t *testing.T) {
		h := run([]ai.Message{ai.NewUserText("hi", 1)}, map[string]string{"X-Initiator": "override"})
		if h.Get("X-Initiator") != "override" {
			t.Fatalf("options headers must merge last, got %q", h.Get("X-Initiator"))
		}
	})
}

// ---- C9: user content always array-of-parts; empty content skipped ----

func TestOpenAIUserContentArrayParts(t *testing.T) {
	model := openAIModel(nil)
	t.Run("multi text stays separate parts", func(t *testing.T) {
		req := ai.Context{Messages: []ai.Message{
			ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "one"}, ai.TextContent{Text: "two"}}, Timestamp: 1},
		}}
		body := buildOpenAIParams(model, req, &OpenAIOptions{})
		msgs, _ := body["messages"].([]map[string]any)
		parts, ok := msgs[0]["content"].([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("multi-text user content must be 2 parts (never joined), got %#v", msgs[0]["content"])
		}
		p0, _ := parts[0].(map[string]any)
		p1, _ := parts[1].(map[string]any)
		if p0["text"] != "one" || p1["text"] != "two" {
			t.Fatalf("parts wrong: %v / %v", p0, p1)
		}
	})
	t.Run("single text is a one-element array", func(t *testing.T) {
		// Explicit array-form content (NewUserText is string-form and maps to a
		// plain string per pi :789-794 — pinned by TestOpenAIUserContentStringForm).
		req := ai.Context{Messages: []ai.Message{
			ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, Timestamp: 1},
		}}
		body := buildOpenAIParams(model, req, &OpenAIOptions{})
		msgs, _ := body["messages"].([]map[string]any)
		parts, ok := msgs[0]["content"].([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("single-text array content maps to a 1-element parts array (pi :796-816), got %#v", msgs[0]["content"])
		}
	})
	t.Run("empty content skips the message", func(t *testing.T) {
		req := ai.Context{Messages: []ai.Message{
			ai.UserMessage{Content: ai.ContentList{}, Timestamp: 1},
			ai.NewUserText("hi", 2),
		}}
		body := buildOpenAIParams(model, req, &OpenAIOptions{})
		msgs, _ := body["messages"].([]map[string]any)
		if len(msgs) != 1 {
			t.Fatalf("empty user message must be skipped entirely, got %d messages", len(msgs))
		}
	})
}

// ---- C10: onPayload error propagation + replacement ----

func TestOpenAIOnPayloadErrorFailsStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not be sent when onPayload errors")
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) { m.BaseURL = server.URL })
	final := StreamOpenAICompletions(context.Background(), model, baseReq(), &OpenAIOptions{
		StreamOptions: ai.StreamOptions{
			APIKey: "k",
			OnPayload: func(payload any, m *ai.Model) (any, error) {
				return nil, fmt.Errorf("payload rejected")
			},
		},
	}).Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "payload rejected" {
		t.Fatalf("onPayload error must fail the stream, got %s / %q", final.StopReason, final.ErrorMessage)
	}
}

func TestOpenAIOnPayloadReplacement(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) { m.BaseURL = server.URL })
	StreamOpenAICompletions(context.Background(), model, baseReq(), &OpenAIOptions{
		StreamOptions: ai.StreamOptions{
			APIKey: "k",
			OnPayload: func(payload any, m *ai.Model) (any, error) {
				p, _ := payload.(map[string]any)
				p["marker"] = "replaced"
				return p, nil
			},
		},
	}).Result()
	if gotBody["marker"] != "replaced" {
		t.Fatalf("onPayload replacement not honored: %v", gotBody["marker"])
	}
}

// ---- C11 sweep ----

func TestOpenAIErrorMetadataRawAppended(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad request","metadata":{"raw":"upstream detail"}}}`)
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) { m.BaseURL = server.URL })
	final := StreamOpenAICompletions(context.Background(), model, baseReq(),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if !strings.HasSuffix(final.ErrorMessage, "\nupstream detail") {
		t.Fatalf("metadata.raw must be appended after a newline, got %q", final.ErrorMessage)
	}
}

func TestOpenAIHeaderPrecedence(t *testing.T) {
	// pi createClient: model.headers first, session affinity OVERRIDES them,
	// options.headers merge last.
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) {
		m.BaseURL = server.URL
		m.Headers = map[string]string{"session_id": "from-model", "X-Custom": "from-model"}
		m.Compat = json.RawMessage(`{"sendSessionAffinityHeaders":true}`)
	})
	StreamOpenAICompletions(context.Background(), model, baseReq(), &OpenAIOptions{
		StreamOptions: ai.StreamOptions{
			APIKey: "k", SessionID: "sess-1",
			Headers: map[string]string{"x-session-affinity": "from-opts"},
		},
	}).Result()
	if gotHeaders.Get("Session_id") != "sess-1" {
		t.Fatalf("session affinity must override model headers, got %q", gotHeaders.Get("Session_id"))
	}
	if gotHeaders.Get("X-Custom") != "from-model" {
		t.Fatalf("model header lost: %q", gotHeaders.Get("X-Custom"))
	}
	if gotHeaders.Get("X-Session-Affinity") != "from-opts" {
		t.Fatalf("options headers must merge last, got %q", gotHeaders.Get("X-Session-Affinity"))
	}
}

func TestOpenAIOpenRouterRoutingEmptyObjectSent(t *testing.T) {
	// pi checks model.compat?.openRouterRouting for truthiness: {} is truthy in
	// JS, so an explicit empty object is still sent as `provider`.
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "openrouter"
		m.BaseURL = "https://openrouter.ai/api/v1"
		m.Compat = json.RawMessage(`{"openRouterRouting":{}}`)
	})
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	prov, ok := body["provider"].(map[string]any)
	if !ok || len(prov) != 0 {
		t.Fatalf("explicit empty openRouterRouting must be sent as provider:{}, got %#v", body["provider"])
	}
	// null is falsy -> omitted.
	model2 := openAIModel(func(m *ai.Model) {
		m.Provider = "openrouter"
		m.BaseURL = "https://openrouter.ai/api/v1"
		m.Compat = json.RawMessage(`{"openRouterRouting":null}`)
	})
	body2 := buildOpenAIParams(model2, baseReq(), &OpenAIOptions{})
	if has(body2, "provider") {
		t.Fatalf("null openRouterRouting must be omitted, got %v", body2["provider"])
	}
}

func TestOpenAIAntLingFallThrough(t *testing.T) {
	// pi's ant-ling else-if only matches when an effort was requested; without
	// one the chain falls through to the generic reasoning_effort branches.
	model := openAIModel(func(m *ai.Model) {
		m.ID = "ling-1t"
		m.Provider = "ant-ling"
		m.BaseURL = "https://api.ant-ling.com/v1"
		m.Reasoning = true
		m.ThinkingLevelMap = ai.ThinkingLevelMap{"off": strPtr("minimal")}
		m.Compat = json.RawMessage(`{"supportsReasoningEffort":true}`)
	})
	body := buildOpenAIParams(model, baseReq(), &OpenAIOptions{})
	if body["reasoning_effort"] != "minimal" {
		t.Fatalf("ant-ling without effort must reach the generic off branch, got %v", body["reasoning_effort"])
	}
	if has(body, "reasoning") {
		t.Fatalf("ant-ling without effort must not send reasoning, got %v", body["reasoning"])
	}
	// With an effort but no thinkingLevelMap entry, the ant-ling branch is
	// taken and sends nothing (typeof effort !== "string").
	model.ThinkingLevelMap = nil
	body2 := buildOpenAIParams(model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if has(body2, "reasoning") || has(body2, "reasoning_effort") {
		t.Fatalf("ant-ling with unmapped effort must send nothing, got %v / %v", body2["reasoning"], body2["reasoning_effort"])
	}
}

// TestOpenAIUserContentStringForm pins pi's string-vs-parts request shape
// (openai-completions.ts:789-816): string-form user content is sent as a plain
// string; array-form content (even single text) as an array of parts. This is
// the regression the 6-scenario differential diff caught after the C9 fix.
func TestOpenAIUserContentStringForm(t *testing.T) {
	stringMsg := ai.NewUserText("hi", 1)
	body := buildOpenAIParams(openAIModel(nil),
		ai.Context{Messages: []ai.Message{stringMsg}}, &OpenAIOptions{})
	msgs := body["messages"].([]map[string]any)
	if c, ok := msgs[0]["content"].(string); !ok || c != "hi" {
		t.Fatalf("string-form user content must be a plain string, got %#v", msgs[0]["content"])
	}

	arrayMsg := ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, Timestamp: 1}
	body = buildOpenAIParams(openAIModel(nil),
		ai.Context{Messages: []ai.Message{arrayMsg}}, &OpenAIOptions{})
	msgs = body["messages"].([]map[string]any)
	if _, ok := msgs[0]["content"].([]any); !ok {
		t.Fatalf("array-form user content must be an array of parts, got %#v", msgs[0]["content"])
	}
}

func TestDiffZaiGLM52ReasoningEffort(t *testing.T) {
	// pi 75b0d723: GLM-5.2 sends thinking:{type} AND a native reasoning_effort,
	// mapped through thinkingLevelMap (minimal:null, low/medium/high:"high",
	// xhigh:"max"). minimal maps to null -> omit reasoning_effort (thinking still
	// enabled); an unmapped level falls back to the raw level.
	glm52 := func() *ai.Model {
		return openAIModel(func(m *ai.Model) {
			m.ID = "glm-5.2"
			m.Provider = "zai"
			m.BaseURL = "https://api.z.ai/api/coding/paas/v4"
			m.Reasoning = true
			m.ThinkingLevelMap = ai.ThinkingLevelMap{
				"minimal": nil,
				"low":     strPtr("high"),
				"medium":  strPtr("high"),
				"high":    strPtr("high"),
				"xhigh":   strPtr("max"),
			}
			m.Compat = json.RawMessage(`{"thinkingFormat":"zai","supportsReasoningEffort":true,"zaiToolStream":true}`)
		})
	}
	if !getOpenAICompat(glm52()).SupportsReasoningEffort {
		t.Fatalf("expected supportsReasoningEffort override to apply for zai glm-5.2")
	}
	// effort -> thinking:enabled + mapped reasoning_effort.
	for _, c := range []struct{ level, want string }{
		{"low", "high"},
		{"high", "high"},
		{"xhigh", "max"},
	} {
		body := buildOpenAIParams(glm52(), baseReq(), &OpenAIOptions{ReasoningEffort: c.level})
		if tm, _ := body["thinking"].(map[string]any); tm["type"] != "enabled" {
			t.Fatalf("%s: thinking = %v, want {type:enabled}", c.level, body["thinking"])
		} else if ct, ok := tm["clear_thinking"]; !ok || ct != false {
			// pi (b91bdd5a / #6083): enabled payload carries clear_thinking:false.
			t.Fatalf("%s: thinking = %v, want clear_thinking:false", c.level, body["thinking"])
		}
		if body["reasoning_effort"] != c.want {
			t.Fatalf("%s: reasoning_effort = %v, want %q", c.level, body["reasoning_effort"], c.want)
		}
	}
	// minimal maps to null -> thinking:enabled, reasoning_effort omitted.
	bMin := buildOpenAIParams(glm52(), baseReq(), &OpenAIOptions{ReasoningEffort: "minimal"})
	if tm, _ := bMin["thinking"].(map[string]any); tm["type"] != "enabled" {
		t.Fatalf("minimal: thinking = %v, want {type:enabled}", bMin["thinking"])
	} else if ct, ok := tm["clear_thinking"]; !ok || ct != false {
		t.Fatalf("minimal: thinking = %v, want clear_thinking:false", bMin["thinking"])
	}
	if has(bMin, "reasoning_effort") {
		t.Fatalf("minimal maps to null -> reasoning_effort must be omitted, got %v", bMin["reasoning_effort"])
	}
	// no effort -> thinking:disabled, no reasoning_effort.
	bOff := buildOpenAIParams(glm52(), baseReq(), &OpenAIOptions{})
	if tm, _ := bOff["thinking"].(map[string]any); tm["type"] != "disabled" {
		t.Fatalf("off: thinking = %v, want {type:disabled}", bOff["thinking"])
	} else if _, ok := tm["clear_thinking"]; ok {
		// pi (b91bdd5a): disabled payload stays bare {type:"disabled"}.
		t.Fatalf("off: thinking = %v, must not carry clear_thinking", bOff["thinking"])
	}
	if has(bOff, "reasoning_effort") {
		t.Fatalf("off must not send reasoning_effort, got %v", bOff["reasoning_effort"])
	}
	// A zai model WITHOUT supportsReasoningEffort never sends reasoning_effort.
	glm46 := openAIModel(func(m *ai.Model) {
		m.ID = "glm-4.6"
		m.Provider = "zai"
		m.BaseURL = "https://api.z.ai/api/coding/paas/v4"
		m.Reasoning = true
		m.Compat = json.RawMessage(`{"thinkingFormat":"zai"}`)
	})
	b46 := buildOpenAIParams(glm46, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if has(b46, "reasoning_effort") {
		t.Fatalf("zai without supportsReasoningEffort must not send reasoning_effort, got %v", b46["reasoning_effort"])
	}
}

// TestClientAPIKeySentinel locks pi's getClientApiKey (129eb460): header-only
// auth yields the "unused" placeholder; no key and no auth header fails with
// pi's exact message.
func TestClientAPIKeySentinel(t *testing.T) {
	if k, err := clientAPIKey("openai", "real", nil); err != nil || k != "real" {
		t.Fatalf("explicit key must pass through: %q %v", k, err)
	}
	for _, name := range []string{"Authorization", "cf-aig-authorization"} {
		k, err := clientAPIKey("custom", "", map[string]string{name: "Bearer x"})
		if err != nil || k != "unused" {
			t.Fatalf("%s header must yield unused sentinel: %q %v", name, k, err)
		}
	}
	if _, err := clientAPIKey("custom", "", map[string]string{"x-foo": "y"}); err == nil ||
		err.Error() != "No API key for provider: custom" {
		t.Fatalf("missing key+auth must fail with pi's message, got %v", err)
	}
}

// runOpenAIHTTPError serves a non-2xx with the given body and returns the final
// assistant message (the HTTP-error branch builds output.ErrorMessage).
func runOpenAIHTTPError(t *testing.T, status int, body string) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	model := &ai.Model{
		ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL,
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	return StreamOpenAICompletions(context.Background(), model, req,
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "sk-test"}}).Result()
}

// TestOpenRouterMetadataRawDedup locks upstream 6fbeba51's guard: error.metadata.raw
// is appended only when it is not already present in the surfaced message, to
// avoid double-printing.
func TestOpenRouterMetadataRawDedup(t *testing.T) {
	// Distinct raw vs message → appended on its own line.
	got := runOpenAIHTTPError(t, 502,
		`{"error":{"message":"upstream failed","metadata":{"raw":"backend timeout"}}}`).ErrorMessage
	if got != "OpenAI API error 502: upstream failed\nbackend timeout" {
		t.Fatalf("distinct raw not appended cleanly: %q", got)
	}
	// raw is a substring of the surfaced message → NOT re-appended (the guard).
	got = runOpenAIHTTPError(t, 502,
		`{"error":{"message":"backend timeout occurred","metadata":{"raw":"backend timeout"}}}`).ErrorMessage
	if strings.Count(got, "backend timeout") != 1 {
		t.Fatalf("raw double-printed despite guard: %q", got)
	}
	if got != "OpenAI API error 502: backend timeout occurred" {
		t.Fatalf("guard mutated message: %q", got)
	}
}
