package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
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
	final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}}).
		Result()

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
	return StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}}).
		Result()
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
		StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
			&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}}).
			Result()
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
	stream := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}})

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

func TestOpenAIMissingFinishReasonInferredWhenCompatDisablesIt(t *testing.T) {
	// upstream 2c3041242: providers that never emit finish_reason opt out via
	// compat.supportsFinishReason:false, and pi infers stop / toolUse instead of
	// failing.
	noFinishReason := func(m *ai.Model) { m.Compat = json.RawMessage(`{"supportsFinishReason":false}`) }

	for _, tc := range []struct {
		name       string
		sse        string
		mutate     func(*ai.Model)
		wantStop   ai.StopReason
		wantErr    string
		wantBlocks int
	}{
		{
			name: "text only infers stop",
			sse: `data: {"choices":[{"delta":{"content":"complete answer"},"finish_reason":null}]}

data: [DONE]

`,
			mutate:     noFinishReason,
			wantStop:   ai.StopStop,
			wantBlocks: 1,
		},
		{
			// A tool call anywhere in the content infers toolUse (pi: content.some).
			name: "tool call infers toolUse",
			sse: `data: {"choices":[{"delta":{"content":"calling"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}

data: [DONE]

`,
			mutate:   noFinishReason,
			wantStop: ai.StopToolUse,
		},
		{
			name: "arrived finish_reason wins over inference",
			sse: `data: {"choices":[{"delta":{"content":"cut"},"finish_reason":"length"}]}

data: [DONE]

`,
			mutate:   noFinishReason,
			wantStop: ai.StopLength,
		},
		{
			// Default is true: without the compat opt-out the same stream errors.
			name: "defaults to true without the opt-out",
			sse: `data: {"choices":[{"delta":{"content":"partial"}}]}

data: [DONE]

`,
			wantStop: ai.StopError,
			wantErr:  "Stream ended without finish_reason",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, final := collectOpenAIEvents(t, tc.sse, tc.mutate)
			if final.StopReason != tc.wantStop || final.ErrorMessage != tc.wantErr {
				t.Fatalf("got %s / %q, want %s / %q", final.StopReason, final.ErrorMessage, tc.wantStop, tc.wantErr)
			}
			if tc.wantBlocks > 0 && len(final.Content) != tc.wantBlocks {
				t.Fatalf("want %d content blocks, got %#v", tc.wantBlocks, final.Content)
			}
		})
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
		return mustBuildOpenAIParams(t, model, ai.Context{Messages: msgs}, &OpenAIOptions{})
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

	t.Run("pipe id sanitized and combined", func(t *testing.T) {
		// upstream d9f7f814: both halves are sanitized and joined as
		// {call_id}_{item_id} (was: keep only the call_id) so item-level uniqueness
		// survives replay into Chat Completions.
		model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1"}
		body := captureBody(model, mkTurn("call_abc+/=123|ENCRYPTEDPAYLOAD", "github-copilot", "gpt-5"))
		callID, resultID := findToolCallIDs(body)
		want := "call_abc___123_ENCRYPTEDPAYLOAD"
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

// ---- C2: streamed reasoning_details ----
//
// Upstream b7bb00b93 moved the sequence off the tool calls' thoughtSignature and
// onto the thinking block's signature; the tests live in
// openai_reasoning_details_test.go.

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
	final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
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
	final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).
		Result()
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
	body := mustBuildOpenAIParams(t, openAIModel(nil), baseReq(),
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

// pi d3ab2af96: Kimi documents cache hits as top-level usage.cached_tokens on
// the final usage chunk — the third arm of the fallback chain
// (prompt_tokens_details.cached_tokens ?? prompt_cache_hit_tokens ?? cached_tokens ?? 0).
func TestOpenAICachedTokensTopLevelKimiFallback(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"cached_tokens":3}}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	if final.Usage.CacheRead != 3 {
		t.Fatalf("top-level cached_tokens must be the cache-read fallback, got cacheRead=%d", final.Usage.CacheRead)
	}
	if final.Usage.Input != 7 {
		t.Fatalf("input = %d, want 7", final.Usage.Input)
	}
}

// The middle arm is nullish, not falsy: an explicit prompt_cache_hit_tokens of
// 0 (DeepSeek's no-hit shape) must NOT fall through to top-level cached_tokens.
func TestOpenAICachedTokensExplicitZeroHitBeatsTopLevel(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":0,"cached_tokens":3}}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	if final.Usage.CacheRead != 0 {
		t.Fatalf("explicit prompt_cache_hit_tokens:0 must not fall through to cached_tokens, got cacheRead=%d", final.Usage.CacheRead)
	}
	if final.Usage.Input != 10 {
		t.Fatalf("input = %d, want 10", final.Usage.Input)
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
	run := func(msgs []ai.Message, optHeaders ai.ProviderHeaders) http.Header {
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
		StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: msgs}),
			&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: optHeaders}}}).
			Result()
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
		h := run([]ai.Message{ai.NewUserText("hi", 1)}, ai.ProviderHeaders{"X-Initiator": strPtr("override")})
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
		body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
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
		body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
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
		body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
		msgs, _ := body["messages"].([]map[string]any)
		if len(msgs) != 1 {
			t.Fatalf("empty user message must be skipped entirely, got %d messages", len(msgs))
		}
	})
	// pi issue #9797 (upstream 1b6ddca87): some OpenAI-compatible providers
	// reject an image-only message carrying an empty text part.
	t.Run("empty text parts are omitted", func(t *testing.T) {
		req := ai.Context{Messages: []ai.Message{
			ai.UserMessage{Content: ai.ContentList{
				ai.TextContent{Text: ""},
				ai.ImageContent{Data: "ZmFrZQ==", MimeType: "image/png"},
			}, Timestamp: 1},
		}}
		body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
		msgs, _ := body["messages"].([]map[string]any)
		got, _ := json.Marshal(msgs[0]["content"])
		if want := `[{"image_url":{"url":"data:image/png;base64,ZmFrZQ=="},"type":"image_url"}]`; string(got) != want {
			t.Fatalf("content = %s, want %s", got, want)
		}
	})
	t.Run("only empty text skips the message", func(t *testing.T) {
		req := ai.Context{Messages: []ai.Message{
			ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: ""}}, Timestamp: 1},
			ai.NewUserText("hi", 2),
		}}
		body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
		msgs, _ := body["messages"].([]map[string]any)
		if len(msgs) != 1 {
			t.Fatalf("a user message of only empty text must be skipped, got %d messages", len(msgs))
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
	final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(baseReq()), &OpenAIOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", OnPayload: func(payload any, m *ai.Model) (any, error) {
				return nil, fmt.Errorf("payload rejected")
			}},
		},
	}).
		Result()
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
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(baseReq()), &OpenAIOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", OnPayload: func(payload any, m *ai.Model) (any, error) {
				p, _ := payload.(map[string]any)
				p["marker"] = "replaced"
				return p, nil
			}},
		},
	}).
		Result()
	if gotBody["marker"] != "replaced" {
		t.Fatalf("onPayload replacement not honored: %v", gotBody["marker"])
	}
}

// ---- C11 sweep ----

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
		m.Headers = ai.ProviderHeaders{"session_id": strPtr("from-model"), "X-Custom": strPtr("from-model")}
		m.Compat = json.RawMessage(`{"sendSessionAffinityHeaders":true}`)
	})
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(baseReq()), &OpenAIOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: ai.ProviderHeaders{"x-session-affinity": strPtr("from-opts")}},
			SessionID:              "sess-1",
		},
	}).
		Result()
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

func TestOpenAISessionAffinityOpenRouterFormat(t *testing.T) {
	// pi openai-completions.ts:519-530 (upstream 298665cf): an OpenRouter model
	// with session affinity enabled sends x-session-id instead of the openai
	// session_id / x-client-request-id / x-session-affinity trio.
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := openAIModel(func(m *ai.Model) {
		m.Provider = "openrouter"
		m.BaseURL = server.URL
		m.Compat = json.RawMessage(`{"sendSessionAffinityHeaders":true}`)
	})
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(baseReq()),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}, SessionID: "sess-1"}}).
		Result()
	if gotHeaders.Get("x-session-id") != "sess-1" {
		t.Fatalf("openrouter format must send x-session-id, got %q", gotHeaders.Get("x-session-id"))
	}
	if gotHeaders.Get("Session_id") != "" || gotHeaders.Get("x-client-request-id") != "" || gotHeaders.Get("x-session-affinity") != "" {
		t.Fatalf("openrouter format must not send openai session headers, got session_id=%q x-client-request-id=%q x-session-affinity=%q",
			gotHeaders.Get("Session_id"), gotHeaders.Get("x-client-request-id"), gotHeaders.Get("x-session-affinity"))
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
	body := mustBuildOpenAIParams(t, model, baseReq(), &OpenAIOptions{})
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
	body2 := mustBuildOpenAIParams(t, model2, baseReq(), &OpenAIOptions{})
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
	body := mustBuildOpenAIParams(t, model, baseReq(), &OpenAIOptions{})
	if body["reasoning_effort"] != "minimal" {
		t.Fatalf("ant-ling without effort must reach the generic off branch, got %v", body["reasoning_effort"])
	}
	if has(body, "reasoning") {
		t.Fatalf("ant-ling without effort must not send reasoning, got %v", body["reasoning"])
	}
	// With an effort but no thinkingLevelMap entry, the ant-ling branch is
	// taken and sends nothing (typeof effort !== "string").
	model.ThinkingLevelMap = nil
	body2 := mustBuildOpenAIParams(t, model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
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
	body := mustBuildOpenAIParams(t, openAIModel(nil),
		ai.Context{Messages: []ai.Message{stringMsg}}, &OpenAIOptions{})
	msgs := body["messages"].([]map[string]any)
	if c, ok := msgs[0]["content"].(string); !ok || c != "hi" {
		t.Fatalf("string-form user content must be a plain string, got %#v", msgs[0]["content"])
	}

	arrayMsg := ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, Timestamp: 1}
	body = mustBuildOpenAIParams(t, openAIModel(nil),
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
		body := mustBuildOpenAIParams(t, glm52(), baseReq(), &OpenAIOptions{ReasoningEffort: c.level})
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
	bMin := mustBuildOpenAIParams(t, glm52(), baseReq(), &OpenAIOptions{ReasoningEffort: "minimal"})
	if tm, _ := bMin["thinking"].(map[string]any); tm["type"] != "enabled" {
		t.Fatalf("minimal: thinking = %v, want {type:enabled}", bMin["thinking"])
	} else if ct, ok := tm["clear_thinking"]; !ok || ct != false {
		t.Fatalf("minimal: thinking = %v, want clear_thinking:false", bMin["thinking"])
	}
	if has(bMin, "reasoning_effort") {
		t.Fatalf("minimal maps to null -> reasoning_effort must be omitted, got %v", bMin["reasoning_effort"])
	}
	// no effort -> thinking:disabled, no reasoning_effort.
	bOff := mustBuildOpenAIParams(t, glm52(), baseReq(), &OpenAIOptions{})
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
	b46 := mustBuildOpenAIParams(t, glm46, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
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
		k, err := clientAPIKey("custom", "", ai.ProviderHeaders{name: strPtr("Bearer x")})
		if err != nil || k != "unused" {
			t.Fatalf("%s header must yield unused sentinel: %q %v", name, k, err)
		}
	}
	if _, err := clientAPIKey("custom", "", ai.ProviderHeaders{"x-foo": strPtr("y")}); err == nil ||
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
	return StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}}).
		Result()
}

// TestCompletionsHTTPErrorMatchesPi drives every captured body through the
// completions adapter and compares the terminal ErrorMessage with pi's byte
// for byte. pi's catch (openai-completions.ts) is the Responses composition
// with no prefix — `${status}: ${body}` when the APIError's `error` is a
// non-empty plain object whose JSON.stringify form (capped at 4000 UTF-16
// units) the SDK's message does not already contain, the SDK's message
// otherwise — and then error.metadata.raw, read off the APIError's `error`,
// on a line of its own unless the message already contains its String()
// (upstream 6fbeba51's guard).
func TestCompletionsHTTPErrorMatchesPi(t *testing.T) {
	for _, row := range loadHTTPErrorOracle(t) {
		t.Run(row.Name, func(t *testing.T) {
			want := row.ErrorMessage["openai-completions"]
			if got := runOpenAIHTTPError(t, row.Status, row.Body).ErrorMessage; got != want {
				t.Fatalf("error message = %q, want pi's %q", got, want)
			}
		})
	}
}

// pi #6290 (upstream 279f53b0): an empty tool result with no image content must
// send "(no tool output)", not "(see attached image)", so the model doesn't
// hallucinate an attachment for commands that produce no output.
func TestOpenAIEmptyToolResultNoImagePlaceholder(t *testing.T) {
	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1"}
	body := mustBuildOpenAIParams(t, model, ai.Context{Messages: []ai.Message{
		ai.NewUserText("run it", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "true"}}},
			Api:     ai.APIOpenAICompletions, Provider: "openai", Model: "gpt-test", StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: ""}}, Timestamp: 2},
	}}, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	var toolContent any
	found := false
	for _, m := range msgs {
		if m["role"] == "tool" {
			toolContent, found = m["content"], true
		}
	}
	if !found {
		t.Fatalf("no tool message emitted: %#v", msgs)
	}
	if toolContent != "(no tool output)" {
		t.Fatalf("empty no-image tool result content = %#v, want %q", toolContent, "(no tool output)")
	}
}

// TestNormalizeOpenAIToolCallID locks pi's normalizeToolCallId (openai-completions.ts
// convertMessages, upstream d9f7f814): pipe-separated Responses-API ids preserve
// item-level uniqueness as {call_id}_{item_id}, sharing a call_id but differing by
// item_id yields distinct ids, and an over-40 combination falls back to
// {call_id}_{hash8}. Expected values were derived independently from pi's algorithm.
func TestNormalizeOpenAIToolCallID(t *testing.T) {
	openaiModel := &ai.Model{Provider: "openai"}
	otherModel := &ai.Model{Provider: "github-copilot"}

	cases := []struct {
		name  string
		model *ai.Model
		id    string
		want  string
	}{
		{"combined", otherModel, "call_abc|item_xyz", "call_abc_item_xyz"},
		{"shared call_id A", otherModel, "call_1|A", "call_1_A"},
		{"shared call_id B", otherModel, "call_1|B", "call_1_B"},
		{"empty item keeps call_id", otherModel, "call_abc|", "call_abc"},
		{"sanitize both halves", otherModel, "call+ab|it/em", "call_ab_it_em"},
		{"boundary 40 no hash", otherModel, strings.Repeat("c", 20) + "|" + strings.Repeat("d", 19), strings.Repeat("c", 20) + "_" + strings.Repeat("d", 19)},
		{"over 40 hash fallback", otherModel, strings.Repeat("c", 20) + "|" + strings.Repeat("d", 20), strings.Repeat("c", 20) + "_srm525dv"},
		{"long call+item hash fallback", otherModel, strings.Repeat("a", 50) + "|" + strings.Repeat("b", 50), strings.Repeat("a", 31) + "_1bex2pkm"},
		{"non-pipe openai truncates", openaiModel, strings.Repeat("z", 45), strings.Repeat("z", 40)},
		{"non-pipe other passes through", otherModel, strings.Repeat("z", 45), strings.Repeat("z", 45)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeOpenAIToolCallID(tc.model, tc.id); got != tc.want {
				t.Errorf("normalizeOpenAIToolCallID(%q) = %q, want %q", tc.id, got, tc.want)
			}
			if len(tc.want) > 40 && strings.Contains(tc.id, "|") {
				t.Errorf("pipe id %q produced over-40 result %q", tc.id, tc.want)
			}
		})
	}
	// Shared call_id with distinct item_id must not collide.
	if a, b := normalizeOpenAIToolCallID(otherModel, "call_1|A"), normalizeOpenAIToolCallID(otherModel, "call_1|B"); a == b {
		t.Errorf("shared call_id ids collided: %q == %q", a, b)
	}
}

// mustBuildOpenAIParams builds a chat-completions request body, failing the test
// on the errors constrained sampling can raise.
func mustBuildOpenAIParams(t *testing.T, model *ai.Model, req ai.Context, opts *OpenAIOptions) map[string]any {
	t.Helper()
	params, err := buildOpenAIParams(model, ai.NormalizeContext(req), opts)
	if err != nil {
		t.Fatalf("buildOpenAIParams: %v", err)
	}
	return params
}

// ---- Upstream 24bace27: constrained sampling (chat completions) ----

// grammarSamplingTool is the tool used by the constrained-sampling tests; its
// pi-side counterpart is the `gram` tool in the recorded probes.
func grammarSamplingTool() ai.Tool {
	return ai.Tool{
		Name: "gram", Description: "gram tool",
		Parameters: ai.Object(ai.Prop("query", ai.String())),
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{
			Type: ai.ConstrainedSamplingGrammar, Variants: ai.GrammarVariants{OpenAILark: "start: /.+/"},
		},
	}
}

// pi 0.82.0 tools payload for a lark grammar tool on openai-completions:
//
//	[{"type":"custom","custom":{"name":"gram","description":"gram tool",
//	  "format":{"type":"grammar","grammar":{"syntax":"lark","definition":"start: /.+/"}}}}]
func TestOpenAIGrammarToolSerialization(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}

	model := openAIModel(func(m *ai.Model) { m.Compat = json.RawMessage(`{"supportsOpenAIGrammarTools":true}`) })
	body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
	if got := mustJSON(t, body["tools"]); got != `[{"custom":{"description":"gram tool","format":{"grammar":{"definition":"start: /.+/","syntax":"lark"},"type":"grammar"},"name":"gram"},"type":"custom"}]` {
		t.Fatalf("grammar tool payload: %s", got)
	}

	// Without grammar support the tool degrades to a plain function tool.
	plain := mustBuildOpenAIParams(t, openAIModel(nil), req, &OpenAIOptions{})
	tools, _ := plain["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["type"] != "function" {
		t.Fatalf("grammar tool must fall back to a function tool: %#v", plain["tools"])
	}
}

// pi: a json_schema-constrained tool sends strict:true where strict is supported
// and fails outright where "require" cannot be honored.
func TestOpenAIJSONSchemaStrictSampling(t *testing.T) {
	tool := func(strict ai.ConstrainedSamplingStrictness) ai.Tool {
		return ai.Tool{
			Name: "js", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.Integer())),
			ConstrainedSampling: &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingJSONSchema, Strict: strict},
		}
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{tool(ai.ConstrainedSamplingPrefer)}}
	// Strict support is opt-in since 890f92088: the catalog stamps it on capable
	// models, so a hand-built model must carry the key the catalog would.
	strictModel := openAIModel(func(m *ai.Model) { m.Compat = json.RawMessage(`{"supportsStrictMode":true}`) })
	body := mustBuildOpenAIParams(t, strictModel, req, &OpenAIOptions{})
	tools, _ := body["tools"].([]map[string]any)
	fn, _ := tools[0]["function"].(map[string]any)
	if fn["strict"] != true {
		t.Fatalf("json_schema constrained tool must send strict:true, got %#v", fn)
	}

	// A model that does not opt in has no strict support.
	noStrict := openAIModel(nil)
	reqRequire := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{tool(ai.ConstrainedSamplingRequire)}}
	_, err := buildOpenAIParams(noStrict, ai.NormalizeContext(reqRequire), &OpenAIOptions{})
	assertErrString(t, err, `Tool "js" requires JSON-schema constrained sampling, but strict tools are unsupported.`)

	// "prefer" simply drops the constraint.
	reqPrefer := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{tool(ai.ConstrainedSamplingPrefer)}}
	if body, err := buildOpenAIParams(noStrict, ai.NormalizeContext(reqPrefer), &OpenAIOptions{}); err != nil {
		t.Fatalf("prefer must not fail: %v", err)
	} else if tools, _ := body["tools"].([]map[string]any); len(tools) != 1 {
		t.Fatalf("tools: %#v", body["tools"])
	} else if fn, _ := tools[0]["function"].(map[string]any); fn["strict"] != nil {
		t.Fatalf("strict must stay omitted when the provider rejects it: %#v", fn)
	}
}

// pi 0.82.0 assistant replay for a grammar tool call:
//
//	"tool_calls":[{"id":"ctc_abc|ctc_item","type":"custom","custom":{"name":"gram","input":"SELECT 1"}}]
func TestOpenAIGrammarToolCallReplay(t *testing.T) {
	model := openAIModel(func(m *ai.Model) { m.Compat = json.RawMessage(`{"supportsOpenAIGrammarTools":true}`) })
	req := ai.Context{
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{
				Content:  ai.ContentList{ai.ToolCall{ID: "ctc_abc|ctc_item", Name: "gram", Arguments: map[string]any{"query": "SELECT 1"}}},
				Model:    model.ID,
				Provider: model.Provider,
				Api:      model.Api,
			},
		},
		Tools: []ai.Tool{grammarSamplingTool()},
	}
	body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	var assistant map[string]any
	for _, m := range msgs {
		if m["role"] == "assistant" {
			assistant = m
		}
	}
	if got := mustJSON(t, assistant["tool_calls"]); got != `[{"custom":{"input":"SELECT 1","name":"gram"},"id":"ctc_abc|ctc_item","type":"custom"}]` {
		t.Fatalf("grammar tool call replay: %s", got)
	}

	// A grammar tool call whose stored arguments lost the string property fails.
	req.Messages[1] = ai.AssistantMessage{
		Content:  ai.ContentList{ai.ToolCall{ID: "ctc_abc|ctc_item", Name: "gram", Arguments: map[string]any{}}},
		Model:    model.ID,
		Provider: model.Provider,
		Api:      model.Api,
	}
	_, err := buildOpenAIParams(model, ai.NormalizeContext(req), &OpenAIOptions{})
	assertErrString(t, err, `Grammar tool call "gram" requires argument "query" to be a string.`)
}

// Streaming custom-tool input is re-synthesized into JSON deltas. Event sequence
// captured from pi 0.82.0 driving the same SSE:
//
//	start, delta {"query":"SEL, delta ECT \"a\", delta "}, end args {"query":"SELECT \"a\""}
func TestOpenAIGrammarToolCallStreaming(t *testing.T) {
	sse := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"ctc_1","type":"custom","custom":{"name":"gram","input":"SEL"}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"ECT \"a\""}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := openAIModel(func(m *ai.Model) {
		m.BaseURL = server.URL
		m.Compat = json.RawMessage(`{"supportsOpenAIGrammarTools":true}`)
	})
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}
	stream := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}})

	var deltas []string
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	want := []string{`{"query":"SEL`, `ECT \"a\"`, `"}`}
	if len(deltas) != len(want) {
		t.Fatalf("deltas = %#v, want %#v", deltas, want)
	}
	for i := range want {
		if deltas[i] != want[i] {
			t.Fatalf("delta %d = %q, want %q", i, deltas[i], want[i])
		}
	}
	final := stream.Result()
	tc, ok := final.Content[0].(ai.ToolCall)
	if !ok || tc.ID != "ctc_1" || tc.Name != "gram" || tc.Arguments["query"] != `SELECT "a"` {
		t.Fatalf("final tool call = %#v", final.Content)
	}
}

// mustJSON marshals v with sorted keys for stable byte comparison.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestOpenAICompletionsRawStopReason mirrors pi's
// openai-completions-raw-stop-reason.test.ts (d7b02636): the wire finish_reason
// is preserved verbatim on rawStopReason, whether it maps to a stop or an error.
func TestOpenAICompletionsRawStopReason(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		wantStop     ai.StopReason
		wantErrMsg   string
	}{
		{name: "stop", finishReason: "stop", wantStop: ai.StopStop},
		{name: "length", finishReason: "length", wantStop: ai.StopLength},
		{
			name:         "content_filter",
			finishReason: "content_filter",
			wantStop:     ai.StopError,
			wantErrMsg:   "Provider finish_reason: content_filter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"` + tt.finishReason + `"}]}` + "\n\n" +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, sse)
			}))
			defer server.Close()

			model := &ai.Model{ID: "test-model", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 4096}
			final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(
				ai.Context{Messages: []ai.Message{ai.NewUserText("hello", 1)}}),

				&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test"}}}).
				Result()

			if final.StopReason != tt.wantStop {
				t.Fatalf("stopReason = %s, want %s", final.StopReason, tt.wantStop)
			}
			if final.RawStopReason != tt.finishReason {
				t.Fatalf("rawStopReason = %q, want %q", final.RawStopReason, tt.finishReason)
			}
			if final.ErrorMessage != tt.wantErrMsg {
				t.Fatalf("errorMessage = %q, want %q", final.ErrorMessage, tt.wantErrMsg)
			}
		})
	}
}

// TestOpenAIEmptyCustomOnFunctionToolCall mirrors pi's
// openai-completions-tool-choice.test.ts "ignores empty custom objects on
// function tool call deltas" (34239180): a delta carrying BOTH custom and
// function is a function call, so its arguments must survive rather than being
// replaced by an empty grammar input.
func TestOpenAIEmptyCustomOnFunctionToolCall(t *testing.T) {
	tests := []struct {
		name     string
		toolCall string
		wantArgs map[string]any
	}{
		{
			name:     "empty custom alongside function",
			toolCall: `{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"},"custom":{}}`,
			wantArgs: map[string]any{"path": "README.md"},
		},
		{
			name:     "populated custom alongside function still loses to function",
			toolCall: `{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"},"custom":{"name":"read","input":"ignored"}}`,
			wantArgs: map[string]any{"path": "README.md"},
		},
		{
			name:     "custom with no function is still a grammar call",
			toolCall: `{"index":0,"id":"call_1","type":"custom","custom":{"name":"read","input":"raw text"}}`,
			wantArgs: map[string]any{"input": "raw text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"id":"chatcmpl-empty-custom","choices":[{"index":0,"delta":{"tool_calls":[` + tt.toolCall + `]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, sse)
			}))
			defer server.Close()

			model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 4096}
			req := ai.Context{
				Messages: []ai.Message{ai.NewUserText("Read README.md", 1)},
				Tools:    []ai.Tool{{Name: "read", Description: "Read a file", Parameters: ai.Object()}},
			}
			final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
				&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test"}}}).
				Result()

			if final.StopReason != ai.StopToolUse {
				t.Fatalf("stopReason = %s (%s)", final.StopReason, final.ErrorMessage)
			}
			if len(final.Content) != 1 {
				t.Fatalf("content = %#v, want one tool call", final.Content)
			}
			tc, ok := final.Content[0].(ai.ToolCall)
			if !ok {
				t.Fatalf("content[0] = %#v, want a tool call", final.Content[0])
			}
			if tc.ID != "call_1" || tc.Name != "read" {
				t.Fatalf("tool call id/name = %q/%q", tc.ID, tc.Name)
			}
			if mustJSON(t, tc.Arguments) != mustJSON(t, tt.wantArgs) {
				t.Fatalf("arguments = %v, want %v", tc.Arguments, tt.wantArgs)
			}
		})
	}
}

// TestOpenAIToolCallNameNullishCoalescing pins pi's
// `toolCall.function?.name ?? toolCall.custom?.name ?? ""`: `??` falls through
// only on null/undefined, so an EXPLICIT empty function.name wins over a
// populated custom.name, while an ABSENT function.name falls through to it.
// 34239180 made both-payloads-present a first-class case, which is what makes
// the distinction reachable.
func TestOpenAIToolCallNameNullishCoalescing(t *testing.T) {
	tests := []struct {
		name     string
		toolCall string
		wantName string
	}{
		{
			name:     "explicit empty function name beats custom name",
			toolCall: `{"index":0,"id":"call_1","function":{"name":"","arguments":"{}"},"custom":{"name":"read","input":"ignored"}}`,
			wantName: "",
		},
		{
			name:     "absent function name falls through to custom name",
			toolCall: `{"index":0,"id":"call_1","function":{"arguments":"{}"},"custom":{"name":"read","input":"ignored"}}`,
			wantName: "read",
		},
		{
			name:     "null function name falls through to custom name",
			toolCall: `{"index":0,"id":"call_1","function":{"name":null,"arguments":"{}"},"custom":{"name":"read","input":"ignored"}}`,
			wantName: "read",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"id":"chatcmpl-name","choices":[{"index":0,"delta":{"tool_calls":[` + tt.toolCall + `]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, sse)
			}))
			defer server.Close()

			model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 4096}
			req := ai.Context{
				Messages: []ai.Message{ai.NewUserText("Read README.md", 1)},
				Tools:    []ai.Tool{{Name: "read", Description: "Read a file", Parameters: ai.Object()}},
			}
			final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
				&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test"}}}).
				Result()

			if len(final.Content) != 1 {
				t.Fatalf("content = %#v, want one tool call", final.Content)
			}
			tc, ok := final.Content[0].(ai.ToolCall)
			if !ok {
				t.Fatalf("content[0] = %#v, want a tool call", final.Content[0])
			}
			if tc.Name != tt.wantName {
				t.Fatalf("tool call name = %q, want %q", tc.Name, tt.wantName)
			}
		})
	}
}

// TestOpenAIGrammarToolCallStartsWithEmptyInputProperty pins pi 8b5899dce,
// which REVERSED 5c6655e76: a custom (grammar) tool call is born with
// `arguments: {[inputProperty]: ""}` again, not `{}`, at both sites that switch
// a block to the custom-tool shape — the freshly created block, and a later
// delta that reveals a block already created as an ordinary function call is
// really a custom one. Consumers read the partial's arguments by property, so
// the property has to be present (as the empty string) from the toolcall_start
// event onwards rather than appearing with the first input delta.
//
// The property is the grammar tool's single required string property, falling
// back to "input" for a custom call naming a tool the request never declared.
func TestOpenAIGrammarToolCallStartsWithEmptyInputProperty(t *testing.T) {
	tests := []struct {
		name string
		// deltas are the tool_call delta objects, one per SSE chunk.
		deltas []string
		// inspect names the event whose partial carries the just-switched block,
		// by type and 1-based occurrence: the start event when the block is
		// custom from birth, the delta event for the switching chunk otherwise.
		// Not simply the last event of its type — a later input delta
		// legitimately fills the property in.
		inspect    ai.EventType
		occurrence int
		// want is the arguments JSON the inspected event must carry.
		want string
	}{
		{
			name:       "custom from the first delta",
			deltas:     []string{`{"index":0,"id":"ctc_1","type":"custom","custom":{"name":"gram","input":"SEL"}}`},
			inspect:    ai.EventToolCallStart,
			occurrence: 1,
			want:       `{"query":""}`,
		},
		{
			// A function delta creates the block with real arguments; the custom
			// delta that follows switches it and must replace them with the
			// placeholder property, not clear them to {}.
			name: "custom revealed by a later delta",
			deltas: []string{
				`{"index":0,"id":"ctc_1","type":"function","function":{"name":"gram","arguments":"{\"query\":\"x\"}"}}`,
				`{"index":0,"custom":{"name":"gram"}}`,
			},
			inspect:    ai.EventToolCallDelta,
			occurrence: 2,
			want:       `{"query":""}`,
		},
		{
			// pi's `grammarToolInputProperties.get(name) ?? "input"`: a made-up
			// custom tool still gets somewhere to stash its input.
			name:       "unknown custom tool falls back to input",
			deltas:     []string{`{"index":0,"id":"ctc_1","type":"custom","custom":{"name":"ghost","input":"SEL"}}`},
			inspect:    ai.EventToolCallStart,
			occurrence: 1,
			want:       `{"input":""}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := ""
			for _, d := range tt.deltas {
				sse += `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[` + d + `]}}]}` + "\n\n"
			}
			sse += `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, sse)
			}))
			defer server.Close()

			model := openAIModel(func(m *ai.Model) {
				m.BaseURL = server.URL
				m.Compat = json.RawMessage(`{"supportsOpenAIGrammarTools":true}`)
			})
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}
			stream := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(req),
				&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}})

			args, seen := "", 0
			for ev := range stream.Events() {
				if ev.Type != tt.inspect {
					continue
				}
				seen++
				if seen != tt.occurrence {
					continue
				}
				tc, ok := ev.Partial.Content[ev.ContentIndex].(ai.ToolCall)
				if !ok {
					t.Fatalf("%s block = %#v", ev.Type, ev.Partial.Content[ev.ContentIndex])
				}
				args = mustJSON(t, tc.Arguments)
			}
			if seen < tt.occurrence {
				t.Fatalf("saw %d %s events, want at least %d", seen, tt.inspect, tt.occurrence)
			}
			if args != tt.want {
				t.Fatalf("arguments at %s #%d = %s, want %s", tt.inspect, tt.occurrence, args, tt.want)
			}
		})
	}
}

// TestOpenAIStreamSimpleMissingKeySurfacesOnTheStream pins pi 8b5899dce, which
// restored the eager API-key assertion at the top of streamSimple in the
// openai-completions and openai-responses adapters:
//
//	getClientApiKey(model.provider, options?.apiKey, options?.headers);
//
// a call kept only for its throw. Upstream raises it SYNCHRONOUSLY, before a
// stream exists. Under the port's G3 ruling (ai/stream.go:90) a setup failure pi
// throws is rendered as a terminal "error" event on the returned stream instead,
// so StreamSimple* keeps its single return value; what carries over from the
// eager throw is its PRECEDENCE — a missing key preempts every other setup
// failure on the path, and still arrives as one terminal error event with no
// "start" before it.
//
// The precedence sub-case supplies a tool whose grammar config is unusable.
// Grammar tools resolve before the request body (openai.go:145,
// openai_responses.go:312), so with a key present that request fails with the
// grammar message — asserted here as a control, so the precedence assertion
// cannot pass vacuously. Without a key the key error must win.
func TestOpenAIStreamSimpleMissingKeySurfacesOnTheStream(t *testing.T) {
	// badGrammarTool asks for grammar-constrained sampling without offering any
	// variant the provider could use, which fails grammarToolInputProperties.
	badGrammarTool := ai.Tool{
		Name: "bad", Description: "bad tool",
		Parameters:          ai.Object(ai.Prop("query", ai.String())),
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingGrammar},
	}
	const grammarErr = `Tool "bad" cannot use grammar constrained sampling: no supported grammar variant was provided.`

	apis := []struct {
		name   string
		api    ai.Api
		stream func(*ai.Model, ai.Context, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream
	}{
		{name: "openai-completions", api: ai.APIOpenAICompletions, stream: func(m *ai.Model, req ai.Context, o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return StreamSimpleOpenAICompletions(context.Background(), m, ai.NormalizeContext(req), o)
		}},
		{name: "openai-responses", api: ai.APIOpenAIResponses, stream: func(m *ai.Model, req ai.Context, o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return StreamSimpleOpenAIResponses(context.Background(), m, ai.NormalizeContext(req), o)
		}},
	}
	scenarios := []struct {
		name string
		req  ai.Context
		// control is the error the same request reports when a key IS supplied,
		// empty when nothing else can fail before the request goes out.
		control string
	}{
		{
			name: "no other setup failure",
			req:  ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		},
		{
			name:    "preempts a grammar tool failure",
			req:     ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{badGrammarTool}},
			control: grammarErr,
		},
	}

	// drain returns the event types seen and the final message.
	drain := func(stream *ai.AssistantMessageEventStream) ([]ai.EventType, *ai.AssistantMessage) {
		var types []ai.EventType
		for ev := range stream.Events() {
			types = append(types, ev.Type)
		}
		return types, stream.Result()
	}

	for _, api := range apis {
		for _, sc := range scenarios {
			t.Run(api.name+"/"+sc.name, func(t *testing.T) {
				newModel := func() *ai.Model {
					return &ai.Model{
						ID: "m", Api: api.api, Provider: "custom", MaxTokens: 16,
						Compat: json.RawMessage(`{"supportsOpenAIGrammarTools":true}`),
					}
				}

				if sc.control != "" {
					// Control: with a key, the other setup failure is reachable.
					_, final := drain(api.stream(newModel(), sc.req, &ai.SimpleStreamOptions{
						StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}},
					}))
					if final.ErrorMessage != sc.control {
						t.Fatalf("with a key: error = %q, want %q (the precedence check would be vacuous)", final.ErrorMessage, sc.control)
					}
				}

				types, final := drain(api.stream(newModel(), sc.req, &ai.SimpleStreamOptions{}))
				if len(types) != 1 || types[0] != ai.EventError {
					t.Fatalf("events = %v, want a single error event", types)
				}
				if final.StopReason != ai.StopError || final.ErrorMessage != "No API key for provider: custom" {
					t.Fatalf("final = stop %s, err %q", final.StopReason, final.ErrorMessage)
				}
			})
		}
	}
}

// A replayed tool call's arguments string is JSON.stringify's text
// (openai-completions.ts convertMessages): &, < and > stay as themselves, where
// json.Marshal would put \u0026-style escapes into the model's history.
func TestOpenAIReplayedArgumentsAreJSONStringify(t *testing.T) {
	model := openAIModel(nil)
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "make && ./run <in >out"}}},
			Model:   model.ID, Provider: model.Provider, Api: model.Api, StopReason: ai.StopToolUse, Timestamp: 2,
		},
	}}
	body := mustBuildOpenAIParams(t, model, req, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	for _, m := range msgs {
		calls, _ := m["tool_calls"].([]map[string]any)
		for _, c := range calls {
			fn, _ := c["function"].(map[string]any)
			if got, want := fn["arguments"], `{"command":"make && ./run <in >out"}`; got != want {
				t.Fatalf("arguments = %v, want %s", got, want)
			}
			return
		}
	}
	t.Fatalf("no replayed tool call in %v", body["messages"])
}

// Upstream openai-completions-provider-stream-event.test.ts (002fc8385),
// "exposes provider chunks including OpenRouter metadata" (pi #9784):
// completeSimple on an OpenRouter model hands the observer each chunk as
// parsed, with the fields pi's normalization drops (usage.cost, is_byok,
// openrouter_metadata), and never the [DONE] sentinel. pi deep-equals the
// objects; the port's are compared as JSON.stringify writes them, which also
// holds them to the key order JSON.parse's objects have.
func TestOpenAICompletionsProviderStreamEventsIncludeOpenRouterMetadata(t *testing.T) {
	firstChunk := `{"id":"chatcmpl-1","model":"anthropic/claude-sonnet-4.6","choices":[{"index":0,"delta":{"content":"hello"}}]}`
	finalChunk := `{"id":"chatcmpl-1","model":"anthropic/claude-sonnet-4.6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0.0012,"is_byok":false},` +
		`"openrouter_metadata":{"strategy":"direct","region":"iad"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("x-request-id", "req-1")
		_, _ = io.WriteString(w, "data: "+firstChunk+"\n\ndata: "+finalChunk+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := &ai.Model{
		ID: "openrouter/auto", Name: "OpenRouter Auto", Api: ai.APIOpenAICompletions, Provider: "openrouter",
		BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 200_000, MaxTokens: 8192,
	}
	var events []any
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "test"
	opts.OnProviderStreamEvent = func(data any, _ *ai.Model) error {
		events = append(events, data)
		return nil
	}

	message := ai.CompleteSimple(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts)

	if len(message.Content) != 1 || message.Content[0] != (ai.TextContent{Text: "hello"}) {
		t.Fatalf("content = %#v (%s %q), want [{text hello}]", message.Content, message.StopReason, message.ErrorMessage)
	}
	var got []string
	for _, event := range events {
		if _, ok := event.(ai.OrderedObject); !ok {
			t.Fatalf("event is %T, want an ai.OrderedObject", event)
		}
		text, err := jstext.Stringify(event)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, text)
	}
	if want := []string{firstChunk, finalChunk}; !slices.Equal(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
}
