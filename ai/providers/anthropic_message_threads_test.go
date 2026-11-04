package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Ported from upstream packages/ai/test/anthropic-message-threads.test.ts
// (a758f3ae4): a fake Anthropic endpoint that, like the real one, rejects a
// `continue` whose fixed fields differ from its thread's.

const threadsBeta = "message-threads-2026-08-12"

const threadGrammarError = "The compiled grammar is too large, which would cause performance issues. Simplify your tool schemas or reduce the number of strict tools."

// threadWireRequest is one request as the fake endpoint received it.
type threadWireRequest struct {
	raw   []byte
	body  map[string]any
	betas []string
}

func (r threadWireRequest) thread() map[string]any {
	thread, _ := r.body["thread"].(map[string]any)
	return thread
}

func (r threadWireRequest) threadType() string {
	typ, _ := r.thread()["type"].(string)
	return typ
}

func (r threadWireRequest) messages() []any {
	messages, _ := r.body["messages"].([]any)
	return messages
}

type threadReplySpec struct {
	id, model, text, thinking, stopReason string
	toolCalls                             []threadToolCall
	// end is "error_event" or "no_message_stop" to end the stream abnormally.
	end string
}

type threadToolCall struct {
	id, name string
	input    map[string]any
}

type threadResponder func(r threadWireRequest) *http.Response

func threadEvents(events []map[string]any) string {
	var b strings.Builder
	for _, event := range events {
		data, _ := json.Marshal(event)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event["type"], data)
	}
	return b.String()
}

func threadHTTPResponse(status int, contentType, body string, headers map[string]string) *http.Response {
	h := http.Header{"Content-Type": {contentType}}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func threadReply(r threadWireRequest, spec threadReplySpec) *http.Response {
	id := spec.id
	if id == "" {
		id = "msg_x"
	}
	model := spec.model
	if model == "" {
		model, _ = r.body["model"].(string)
	}
	events := []map[string]any{{
		"type":    "message_start",
		"message": map[string]any{"id": id, "model": model, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}},
	}}
	index := 0
	if spec.thinking != "" {
		events = append(events,
			map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}},
			map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": spec.thinking}},
			map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "signature_delta", "signature": "sig"}},
			map[string]any{"type": "content_block_stop", "index": index},
		)
		index++
	}
	text := spec.text
	if text == "" {
		text = "answer"
	}
	events = append(events,
		map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}},
		map[string]any{"type": "content_block_stop", "index": index},
	)
	index++
	for _, call := range spec.toolCalls {
		input, _ := json.Marshal(call.input)
		events = append(events,
			map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call.id, "name": call.name, "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}},
			map[string]any{"type": "content_block_stop", "index": index},
		)
		index++
	}
	if spec.end == "error_event" {
		body := threadEvents(events) + `event: error` + "\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"
		return threadHTTPResponse(http.StatusOK, "text/event-stream", body, nil)
	}
	stopReason := spec.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
		if spec.toolCalls != nil {
			stopReason = "tool_use"
		}
	}
	events = append(events, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason}, "usage": map[string]any{"output_tokens": 1}})
	if spec.end != "no_message_stop" {
		events = append(events, map[string]any{"type": "message_stop"})
	}
	return threadHTTPResponse(http.StatusOK, "text/event-stream", threadEvents(events), nil)
}

func threadErrorResponse(status int, message, errorCode string, headers map[string]string) *http.Response {
	typ := "invalid_request_error"
	switch status {
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	}
	inner := map[string]any{"type": typ, "message": message}
	if errorCode != "" {
		inner["details"] = map[string]any{"error_code": errorCode}
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": inner})
	return threadHTTPResponse(status, "application/json", string(body), headers)
}

// threadFingerprint is the fields Anthropic requires to stay the same within
// a thread (observed live upstream).
func threadFingerprint(r threadWireRequest) string {
	_, hasFallbacks := r.body["fallbacks"]
	data, _ := json.Marshal([]any{r.body["model"], r.body["thinking"], r.body["output_config"], r.body["tool_choice"], hasFallbacks, r.betas})
	return string(data)
}

var threadSessionCounter atomic.Int64

// threadHarness is the fake endpoint: it records request bodies and beta
// lists, and answers from its queue or with a plain reply.
type threadHarness struct {
	mu           sync.Mutex
	requests     []threadWireRequest
	queue        []threadResponder
	fingerprints map[string]string
	sessionID    string
}

func newThreadHarness(t *testing.T) *threadHarness {
	h := &threadHarness{
		fingerprints: map[string]string{},
		sessionID:    fmt.Sprintf("thread-test-%d", threadSessionCounter.Add(1)),
	}
	t.Cleanup(func() {
		if err := ai.CleanupSessionResources(h.sessionID); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *threadHarness) push(responders ...threadResponder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queue = append(h.queue, responders...)
}

func (h *threadHarness) Do(req *http.Request) (*http.Response, error) {
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	r := threadWireRequest{raw: data, body: body}
	if beta := req.Header.Get("anthropic-beta"); beta != "" {
		r.betas = strings.Split(beta, ",")
	}
	h.mu.Lock()
	h.requests = append(h.requests, r)
	n := len(h.requests)
	var next threadResponder
	if len(h.queue) > 0 {
		next, h.queue = h.queue[0], h.queue[1:]
	}
	h.mu.Unlock()
	var resp *http.Response
	if next != nil {
		resp = next(r)
	} else {
		id := fmt.Sprintf("msg_%d", n)
		h.mu.Lock()
		expected := threadFingerprint(r)
		if previous, ok := r.thread()["previous_message_id"].(string); ok {
			expected = h.fingerprints[previous]
		}
		mismatch := expected != threadFingerprint(r)
		if !mismatch && r.thread() != nil {
			h.fingerprints[id] = expected
		}
		h.mu.Unlock()
		if mismatch {
			resp = threadErrorResponse(http.StatusBadRequest, "fixed field changed", "thread_fingerprint_mismatch", nil)
		} else {
			resp = threadReply(r, threadReplySpec{id: id})
		}
	}
	resp.Request = req
	return resp, nil
}

func (h *threadHarness) sent() []threadWireRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]threadWireRequest(nil), h.requests...)
}

func (h *threadHarness) options() AnthropicOptions {
	return AnthropicOptions{StreamOptions: ai.StreamOptions{
		SessionID:              h.sessionID,
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", HTTPClient: h},
	}}
}

func threadBaseModel() ai.Model {
	return ai.Model{
		ID: "claude-test", Name: "Test Claude", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		BaseURL: "https://api.anthropic.com", Input: []string{"text"},
		ContextWindow: 200000, MaxTokens: 100,
	}
}

func threadModelWithCompat(compat string) ai.Model {
	model := threadBaseModel()
	model.Compat = json.RawMessage(compat)
	return model
}

func threadTool(name string) ai.Tool {
	return ai.Tool{Name: name, Description: "Tool " + name, Parameters: ai.Object(ai.Prop("value", ai.String()))}
}

func threadUser(text string) ai.Message { return ai.NewUserText(text, 1) }

func threadToolResult(id, name string) ai.Message {
	return ai.ToolResultMessage{ToolCallID: id, ToolName: name, Content: ai.ContentList{ai.TextContent{Text: "result"}}, Timestamp: 1}
}

// threadTurn is one turn's inputs; the zero value is upstream's defaults.
type threadTurn struct {
	model        *ai.Model
	systemPrompt string
	tools        []ai.Tool
	options      func(*AnthropicOptions)
	ctx          context.Context
}

func (h *threadHarness) turn(t *testing.T, messages []ai.Message, in threadTurn) *ai.AssistantMessage {
	t.Helper()
	systemPrompt := in.systemPrompt
	if systemPrompt == "" {
		systemPrompt = "You are a test assistant."
	}
	tools := in.tools
	if tools == nil {
		tools = []ai.Tool{threadTool("lookup")}
	}
	model := threadBaseModel()
	if in.model != nil {
		model = *in.model
	}
	opts := h.options()
	if in.options != nil {
		in.options(&opts)
	}
	ctx := in.ctx
	if ctx == nil {
		ctx = t.Context()
	}
	req := ai.NormalizeContext(ai.Context{SystemPrompt: systemPrompt, Tools: tools, Messages: messages})
	return StreamAnthropic(ctx, &model, req, &opts).Result()
}

// say runs one turn and appends the reply to the conversation.
func (h *threadHarness) say(t *testing.T, messages *[]ai.Message, in threadTurn) *ai.AssistantMessage {
	t.Helper()
	result := h.turn(t, *messages, in)
	*messages = append(*messages, result)
	return result
}

func wantThread(t *testing.T, label string, r threadWireRequest, want map[string]any) {
	t.Helper()
	if got := r.thread(); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: thread = %v, want %v", label, got, want)
	}
}

var threadCreate = map[string]any{"type": "create"}

func threadContinue(id string) map[string]any {
	return map[string]any{"type": "continue", "previous_message_id": id}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAnthropicThreadCreatesThenContinuesWithNewMessages(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{})
	messages = append(messages, threadUser("second"))
	result := h.say(t, &messages, threadTurn{})

	if result.StopReason != ai.StopStop {
		t.Fatalf("stopReason %q: %s", result.StopReason, result.ErrorMessage)
	}
	requests := h.sent()
	create, next := requests[0], requests[1]
	wantThread(t, "create", create, threadCreate)
	if create.body["system"] == nil || create.body["tools"] == nil {
		t.Errorf("create lacks system or tools: %v", create.body)
	}
	wantThread(t, "next", next, threadContinue("msg_1"))
	// The thread field's keys go out in pi's order.
	if want := `"thread":{"type":"continue","previous_message_id":"msg_1"}`; !strings.Contains(string(next.raw), want) {
		t.Errorf("continue body %s lacks %s", next.raw, want)
	}
	for _, key := range []string{"system", "tools"} {
		if _, ok := next.body[key]; ok {
			t.Errorf("continue carries %s", key)
		}
	}
	// The first user message went out as a cached block on request 1 and as a
	// plain string on request 2; the cache marker moved to the new message.
	if got, want := jsonOf(t, create.messages()[0].(map[string]any)["content"]), `[{"cache_control":{"type":"ephemeral"},"text":"first","type":"text"}]`; got != want {
		t.Errorf("create content %s, want %s", got, want)
	}
	if got, want := jsonOf(t, next.messages()), `[{"content":[{"cache_control":{"type":"ephemeral"},"text":"second","type":"text"}],"role":"user"}]`; got != want {
		t.Errorf("continue messages %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(create.betas, ","), threadsBeta) {
		t.Errorf("create betas %v lack %s", create.betas, threadsBeta)
	}
	if !reflect.DeepEqual(next.betas, create.betas) {
		t.Errorf("continue betas %v, create %v", next.betas, create.betas)
	}
}

func TestAnthropicThreadContinuesAfterToolCalls(t *testing.T) {
	h := newThreadHarness(t)
	h.push(func(r threadWireRequest) *http.Response {
		return threadReply(r, threadReplySpec{id: "msg_tool", toolCalls: []threadToolCall{{"toolu_1", "lookup", map[string]any{"value": "x"}}}})
	})
	messages := []ai.Message{threadUser("look it up")}
	if first := h.say(t, &messages, threadTurn{}); first.StopReason != ai.StopToolUse {
		t.Fatalf("stopReason %q", first.StopReason)
	}
	messages = append(messages, threadToolResult("toolu_1", "lookup"))
	h.say(t, &messages, threadTurn{})

	next := h.sent()[1]
	wantThread(t, "next", next, threadContinue("msg_tool"))
	if len(next.messages()) != 1 {
		t.Fatalf("continue sent %d messages: %v", len(next.messages()), next.messages())
	}
	msg := next.messages()[0].(map[string]any)
	block := msg["content"].([]any)[0].(map[string]any)
	if msg["role"] != "user" || block["type"] != "tool_result" || block["tool_use_id"] != "toolu_1" {
		t.Errorf("continue message %v", msg)
	}
}

func TestAnthropicThreadContinuesWhenEffortChanges(t *testing.T) {
	model := threadModelWithCompat(`{"forceAdaptiveThinking":true,"supportsMidConvoEffort":true}`)
	model.Reasoning = true
	effort := func(level string) func(*AnthropicOptions) {
		return func(o *AnthropicOptions) { o.ThinkingProvided, o.ThinkingEnabled, o.Effort = true, true, level }
	}
	h := newThreadHarness(t)
	h.push(func(r threadWireRequest) *http.Response {
		return threadReply(r, threadReplySpec{id: "msg_a", thinking: "thought"})
	})
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{model: &model, options: effort("high")})
	messages = append(messages, threadUser("second"))
	h.say(t, &messages, threadTurn{model: &model, options: effort("low")})

	shape := func(r threadWireRequest) []string {
		var out []string
		for _, m := range r.messages() {
			m := m.(map[string]any)
			if config, ok := m["output_config"].(map[string]any); ok {
				out = append(out, config["effort"].(string))
			} else {
				out = append(out, m["role"].(string))
			}
		}
		return out
	}
	requests := h.sent()
	if got := shape(requests[0]); !reflect.DeepEqual(got, []string{"user", "high"}) {
		t.Errorf("create messages %v", got)
	}
	wantThread(t, "next", requests[1], threadContinue("msg_a"))
	if got := shape(requests[1]); !reflect.DeepEqual(got, []string{"user", "low"}) {
		t.Errorf("continue messages %v", got)
	}
}

func TestAnthropicThreadContinuesAcrossNativeToolChange(t *testing.T) {
	model := threadModelWithCompat(`{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true}`)
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{model: &model})
	update := ai.NewSystemText("Update", 2)
	update.ToolsAdded = []ai.Tool{threadTool("other")}
	messages = append(messages, update, threadUser("second"))
	h.say(t, &messages, threadTurn{model: &model})

	next := h.sent()[1]
	if next.threadType() != "continue" {
		t.Fatalf("thread %v", next.thread())
	}
	var roles []string
	for _, m := range next.messages() {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "system"}) {
		t.Errorf("continue roles %v", roles)
	}
	if !strings.Contains(jsonOf(t, next.messages()[1].(map[string]any)["content"]), "tool_addition") {
		t.Errorf("system message lacks tool_addition: %v", next.messages()[1])
	}
}

func TestAnthropicThreadCreatesNewThreadWhenSettingsChange(t *testing.T) {
	reasoning := threadBaseModel()
	reasoning.Reasoning = true
	other := threadBaseModel()
	other.ID = "claude-other"
	budget := func(tokens int) func(*AnthropicOptions) {
		return func(o *AnthropicOptions) {
			o.ThinkingProvided, o.ThinkingEnabled, o.ThinkingBudgetTokens = true, true, tokens
		}
	}
	for _, tc := range []struct {
		name          string
		before, after threadTurn
	}{
		{"system prompt", threadTurn{}, threadTurn{systemPrompt: "Different prompt."}},
		{"tools", threadTurn{}, threadTurn{tools: []ai.Tool{threadTool("lookup"), threadTool("other")}}},
		{"model", threadTurn{}, threadTurn{model: &other}},
		{"tool choice", threadTurn{}, threadTurn{options: func(o *AnthropicOptions) { o.ToolChoice = "any" }}},
		{"cache retention", threadTurn{}, threadTurn{options: func(o *AnthropicOptions) { o.CacheRetention = ai.CacheLong }}},
		{"thinking level", threadTurn{model: &reasoning, options: budget(1024)}, threadTurn{model: &reasoning, options: budget(2048)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newThreadHarness(t)
			messages := []ai.Message{threadUser("first")}
			h.say(t, &messages, tc.before)
			messages = append(messages, threadUser("second"))
			h.say(t, &messages, tc.after)
			next := h.sent()[1]
			wantThread(t, "next", next, threadCreate)
			if len(next.messages()) != 3 {
				t.Errorf("sent %d messages, want 3", len(next.messages()))
			}
		})
	}
}

func TestAnthropicThreadCreatesNewThreadWithoutNewMessages(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{})
	h.turn(t, messages, threadTurn{})
	wantThread(t, "next", h.sent()[1], threadCreate)
}

func TestAnthropicThreadStartsNewThreadAfterRejectedContinue(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"thread_not_found", 404},
		{"thread_already_continued", 400},
		{"thread_continue_in_flight", 400},
		{"thread_fingerprint_mismatch", 400},
	} {
		t.Run(tc.code, func(t *testing.T) {
			h := newThreadHarness(t)
			messages := []ai.Message{threadUser("first")}
			h.say(t, &messages, threadTurn{})
			messages = append(messages, threadUser("second"))
			h.push(func(threadWireRequest) *http.Response {
				return threadErrorResponse(tc.status, "thread rejected", tc.code, nil)
			})
			onPayload := 0
			result := h.say(t, &messages, threadTurn{options: func(o *AnthropicOptions) {
				o.OnPayload = func(any, *ai.Model) (any, error) { onPayload++; return nil, nil }
			}})

			if result.StopReason != ai.StopStop {
				t.Fatalf("stopReason %q: %s", result.StopReason, result.ErrorMessage)
			}
			if onPayload != 1 {
				t.Errorf("onPayload ran %d times", onPayload)
			}
			requests := h.sent()
			if len(requests) != 3 {
				t.Fatalf("%d requests, want 3", len(requests))
			}
			rejected, resend := requests[1], requests[2]
			if rejected.threadType() != "continue" {
				t.Errorf("rejected thread %v", rejected.thread())
			}
			wantThread(t, "resend", resend, threadCreate)
			if len(resend.messages()) != 3 || resend.body["system"] == nil {
				t.Errorf("resend is not the full request: %v", resend.body)
			}
			if !reflect.DeepEqual(resend.betas, rejected.betas) {
				t.Errorf("resend betas %v, rejected %v", resend.betas, rejected.betas)
			}
			if len(result.Diagnostics) != 1 {
				t.Fatalf("diagnostics %v", result.Diagnostics)
			}
			d := result.Diagnostics[0]
			if reason, _ := d.Details.Get("reason"); d.Type != "anthropic_thread_fallback" || reason != tc.code || d.Error == nil {
				t.Errorf("diagnostic %+v", d)
			}

			messages = append(messages, threadUser("third"))
			h.say(t, &messages, threadTurn{})
			wantThread(t, "after resend", h.sent()[3], threadContinue("msg_3"))
		})
	}
}

func TestAnthropicThreadTurnsOffAfterUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, reason string
	}{
		{"thread_unsupported_request", "thread_unsupported_request", "rejected", "thread_unsupported_request"},
		{"400 without error code about the thread field", "", "thread: Extra inputs are not permitted", "400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newThreadHarness(t)
			messages := []ai.Message{threadUser("first")}
			h.say(t, &messages, threadTurn{})
			messages = append(messages, threadUser("second"))
			h.push(func(threadWireRequest) *http.Response {
				return threadErrorResponse(http.StatusBadRequest, tc.message, tc.code, nil)
			})
			result := h.say(t, &messages, threadTurn{})
			if result.StopReason != ai.StopStop {
				t.Fatalf("stopReason %q: %s", result.StopReason, result.ErrorMessage)
			}
			if len(result.Diagnostics) == 0 {
				t.Fatal("no diagnostic")
			}
			d := result.Diagnostics[0]
			if reason, _ := d.Details.Get("reason"); d.Type != "anthropic_thread_fallback" || reason != tc.reason {
				t.Errorf("diagnostic %+v", d)
			}
			if resend := h.sent()[2]; resend.thread() != nil {
				t.Errorf("resend carries thread %v", resend.thread())
			}
			messages = append(messages, threadUser("third"))
			h.say(t, &messages, threadTurn{})
			requests := h.sent()
			later := requests[3]
			if _, ok := later.body["thread"]; ok {
				t.Errorf("later request carries thread %v", later.thread())
			}
			// The resend was accepted with the threads beta, so the list stays.
			if !reflect.DeepEqual(later.betas, requests[0].betas) {
				t.Errorf("later betas %v, first %v", later.betas, requests[0].betas)
			}
		})
	}
}

func TestAnthropicThreadResendFailureKeepsThreadsOn(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{})
	messages = append(messages, threadUser("second"))
	h.push(
		func(threadWireRequest) *http.Response {
			return threadErrorResponse(http.StatusBadRequest, "thread: Extra inputs are not permitted", "", nil)
		},
		func(threadWireRequest) *http.Response {
			return threadErrorResponse(http.StatusBadRequest, "Real problem", "", nil)
		},
	)
	failed := h.turn(t, messages, threadTurn{})
	if failed.StopReason != ai.StopError || !strings.Contains(failed.ErrorMessage, "Real problem") {
		t.Fatalf("stopReason %q: %s", failed.StopReason, failed.ErrorMessage)
	}
	if n := len(h.sent()); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}
	h.turn(t, messages, threadTurn{})
	wantThread(t, "after failure", h.sent()[3], threadCreate)
}

func TestAnthropicThreadPassesOtherErrorsOn(t *testing.T) {
	for _, tc := range []struct {
		status  int
		message string
	}{
		{400, "prompt is too long: 250000 tokens > 200000 maximum"},
		{404, "model: claude-test"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			h := newThreadHarness(t)
			messages := []ai.Message{threadUser("first")}
			h.say(t, &messages, threadTurn{})
			messages = append(messages, threadUser("second"))
			h.push(func(threadWireRequest) *http.Response {
				return threadErrorResponse(tc.status, tc.message, "", nil)
			})
			failed := h.turn(t, messages, threadTurn{})
			if failed.StopReason != ai.StopError || !strings.Contains(failed.ErrorMessage, tc.message) {
				t.Fatalf("stopReason %q: %s", failed.StopReason, failed.ErrorMessage)
			}
			if n := len(h.sent()); n != 2 {
				t.Fatalf("%d requests, want 2", n)
			}
			if failed.Diagnostics != nil {
				t.Errorf("diagnostics %v", failed.Diagnostics)
			}
			// Threads stay on: after a fix such as compaction, the next request
			// starts a new thread.
			h.turn(t, messages, threadTurn{})
			wantThread(t, "after error", h.sent()[2], threadCreate)
		})
	}
}

func TestAnthropicThreadLeavesStrictToolRejectionToStrictFallback(t *testing.T) {
	model := threadModelWithCompat(`{"supportsStrictTools":true}`)
	strict := threadTool("lookup")
	strict.ConstrainedSampling = &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer}
	h := newThreadHarness(t)
	h.push(func(threadWireRequest) *http.Response {
		return threadErrorResponse(http.StatusBadRequest, threadGrammarError, "", nil)
	})
	result := h.turn(t, []ai.Message{threadUser("first")}, threadTurn{model: &model, tools: []ai.Tool{strict}})
	if result.StopReason != ai.StopStop {
		t.Fatalf("stopReason %q: %s", result.StopReason, result.ErrorMessage)
	}
	var types []string
	for _, r := range h.sent() {
		types = append(types, r.threadType())
	}
	if !reflect.DeepEqual(types, []string{"create", "create"}) {
		t.Errorf("threads %v", types)
	}
	var diagnostics []string
	for _, d := range result.Diagnostics {
		diagnostics = append(diagnostics, d.Type)
	}
	if !reflect.DeepEqual(diagnostics, []string{"anthropic_strict_tools_fallback"}) {
		t.Errorf("diagnostics %v", diagnostics)
	}
}

func TestAnthropicThreadRetriedContinueInFlightStartsNewThread(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{})
	messages = append(messages, threadUser("second"))
	h.push(
		func(threadWireRequest) *http.Response {
			return threadErrorResponse(http.StatusServiceUnavailable, "overloaded", "", map[string]string{"retry-after-ms": "0"})
		},
		func(threadWireRequest) *http.Response {
			return threadErrorResponse(http.StatusBadRequest, "in flight", "thread_continue_in_flight", nil)
		},
	)
	result := h.say(t, &messages, threadTurn{options: func(o *AnthropicOptions) { o.MaxRetries = 1 }})
	if result.StopReason != ai.StopStop {
		t.Fatalf("stopReason %q: %s", result.StopReason, result.ErrorMessage)
	}
	var types []string
	for _, r := range h.sent() {
		types = append(types, r.threadType())
	}
	if !reflect.DeepEqual(types, []string{"create", "continue", "continue", "create"}) {
		t.Errorf("threads %v", types)
	}
}

func TestAnthropicThreadExplicitBetaHeaderTurnsThreadsOff(t *testing.T) {
	header := "fine-grained-tool-streaming-2025-05-14"
	for _, tc := range []struct {
		name   string
		header *string
	}{
		{"without the threads beta", &header},
		{"set to null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newThreadHarness(t)
			respond := func(r threadWireRequest) *http.Response {
				if r.thread() != nil && !strings.Contains(strings.Join(r.betas, ","), threadsBeta) {
					return threadErrorResponse(http.StatusBadRequest, "thread: Extra inputs are not permitted", "", nil)
				}
				return threadReply(r, threadReplySpec{id: fmt.Sprintf("msg_%d", len(h.sent()))})
			}
			h.push(respond, respond, respond)
			withHeader := func(o *AnthropicOptions) { o.Headers = ai.ProviderHeaders{"anthropic-beta": tc.header} }
			messages := []ai.Message{threadUser("first")}
			if result := h.say(t, &messages, threadTurn{options: withHeader}); result.StopReason != ai.StopStop {
				t.Fatalf("stopReason %q: %s", result.StopReason, result.ErrorMessage)
			}
			messages = append(messages, threadUser("second"))
			h.say(t, &messages, threadTurn{options: withHeader})
			var types []string
			for _, r := range h.sent() {
				types = append(types, r.threadType())
			}
			if !reflect.DeepEqual(types, []string{"create", "", ""}) {
				t.Errorf("threads %q", types)
			}
		})
	}
}

func TestAnthropicThreadDoesNotRecordUnfinishedReplies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  *threadReplySpec // nil aborts the reply
		abort bool
	}{
		{name: "an aborted reply", abort: true},
		{name: "a stream error", spec: &threadReplySpec{end: "error_event"}},
		{name: "a stream that ends early", spec: &threadReplySpec{end: "no_message_stop"}},
		{name: "a refusal", spec: &threadReplySpec{stopReason: "refusal"}},
		{name: "a fallback model reply", spec: &threadReplySpec{model: "claude-fallback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newThreadHarness(t)
			first := threadTurn{}
			if tc.abort {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				first.ctx = ctx
				first.options = func(o *AnthropicOptions) {
					o.OnProviderStreamEvent = func(data any, _ *ai.Model) error {
						if event, ok := data.(ai.OrderedObject); ok {
							if typ, _ := event.Get("type"); typ == "message_start" {
								cancel()
							}
						}
						return nil
					}
				}
			} else {
				spec := *tc.spec
				spec.id = "msg_bad"
				h.push(func(r threadWireRequest) *http.Response { return threadReply(r, spec) })
			}
			messages := []ai.Message{threadUser("first")}
			reply := h.turn(t, messages, first)
			messages = append(messages, reply, threadUser("second"))
			h.turn(t, messages, threadTurn{})
			wantThread(t, "next", h.sent()[1], threadCreate)
		})
	}
}

func TestAnthropicThreadNotUsedWithCacheRetentionNone(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{options: func(o *AnthropicOptions) { o.CacheRetention = ai.CacheNone }})
	first := h.sent()[0]
	if _, ok := first.body["thread"]; ok {
		t.Errorf("thread sent: %v", first.thread())
	}
	if strings.Contains(strings.Join(first.betas, ","), threadsBeta) {
		t.Errorf("betas %v carry %s", first.betas, threadsBeta)
	}
}

func TestAnthropicThreadMatchesClaudeCodeToolNamesWithOAuth(t *testing.T) {
	h := newThreadHarness(t)
	h.push(func(r threadWireRequest) *http.Response {
		return threadReply(r, threadReplySpec{id: "msg_oauth", toolCalls: []threadToolCall{{"toolu_1", "Read", map[string]any{"value": "x"}}}})
	})
	oauth := func(o *AnthropicOptions) { o.APIKey = "sk-ant-oat-test" }
	tools := []ai.Tool{threadTool("read")}
	messages := []ai.Message{threadUser("read it")}
	first := h.say(t, &messages, threadTurn{tools: tools, options: oauth})
	var name string
	for _, block := range first.Content {
		if call, ok := block.(ai.ToolCall); ok {
			name = call.Name
		}
	}
	if name != "read" {
		t.Fatalf("tool call name %q, want read", name)
	}
	messages = append(messages, threadToolResult("toolu_1", "read"))
	h.say(t, &messages, threadTurn{tools: tools, options: oauth})
	wantThread(t, "next", h.sent()[1], threadContinue("msg_oauth"))
}

func TestAnthropicThreadDisableContinuationKeepsConversationThread(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{})
	h.turn(t, messages[:1], threadTurn{options: func(o *AnthropicOptions) {
		one := 1
		o.MaxTokens, o.DisableContinuation = &one, true
	}})
	messages = append(messages, threadUser("second"))
	h.say(t, &messages, threadTurn{})

	requests := h.sent()
	real, warm, next := requests[0], requests[1], requests[2]
	if _, ok := warm.body["thread"]; ok {
		t.Errorf("warm request carries thread %v", warm.thread())
	}
	if warm.body["max_tokens"] != float64(1) {
		t.Errorf("warm max_tokens %v", warm.body["max_tokens"])
	}
	if !reflect.DeepEqual(warm.messages(), real.messages()) {
		t.Errorf("warm messages %v, real %v", warm.messages(), real.messages())
	}
	if !reflect.DeepEqual(warm.betas, real.betas) {
		t.Errorf("warm betas %v, real %v", warm.betas, real.betas)
	}
	wantThread(t, "next", next, threadContinue("msg_1"))
}

// Go-side additions beyond the upstream file.

// The default is on only for the anthropic provider on api.anthropic.com, and
// compat.supportsMessageThreads overrides it either way.
func TestAnthropicThreadCompatDefault(t *testing.T) {
	for _, tc := range []struct {
		name, provider, baseURL, compat string
		want                            bool
	}{
		{"anthropic on api.anthropic.com", "anthropic", "https://api.anthropic.com", "", true},
		{"anthropic elsewhere", "anthropic", "https://gateway.example.com", "", false},
		{"other provider on api.anthropic.com", "proxy", "https://api.anthropic.com", "", false},
		{"opted in", "proxy", "https://gateway.example.com", `{"supportsMessageThreads":true}`, true},
		{"opted out", "anthropic", "https://api.anthropic.com", `{"supportsMessageThreads":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newThreadHarness(t)
			model := threadBaseModel()
			model.Provider, model.BaseURL = tc.provider, tc.baseURL
			if tc.compat != "" {
				model.Compat = json.RawMessage(tc.compat)
			}
			h.turn(t, []ai.Message{threadUser("first")}, threadTurn{model: &model})
			first := h.sent()[0]
			if got := first.thread() != nil; got != tc.want {
				t.Errorf("thread sent = %v, want %v", got, tc.want)
			}
			if got := strings.Contains(strings.Join(first.betas, ","), threadsBeta); got != tc.want {
				t.Errorf("threads beta sent = %v, want %v", got, tc.want)
			}
		})
	}
}

// A request without a session id never uses threads.
func TestAnthropicThreadNeedsSessionID(t *testing.T) {
	h := newThreadHarness(t)
	h.turn(t, []ai.Message{threadUser("first")}, threadTurn{options: func(o *AnthropicOptions) { o.SessionID = "" }})
	first := h.sent()[0]
	if first.thread() != nil || strings.Contains(strings.Join(first.betas, ","), threadsBeta) {
		t.Errorf("thread %v, betas %v", first.thread(), first.betas)
	}
}

// CleanupSessionResources(sessionID) forgets that session's thread, so its
// next request starts a new one; CleanupSessionResources("") forgets all.
func TestAnthropicThreadCleanupSessionResources(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clean func(sessionID string) string
	}{
		{"this session", func(id string) string { return id }},
		{"all sessions", func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newThreadHarness(t)
			messages := []ai.Message{threadUser("first")}
			h.say(t, &messages, threadTurn{})
			if err := ai.CleanupSessionResources(tc.clean(h.sessionID)); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, threadUser("second"))
			h.say(t, &messages, threadTurn{})
			wantThread(t, "after cleanup", h.sent()[1], threadCreate)
		})
	}
}

// The store keeps at most max sessions, dropping the least recently UPDATED
// one: reading a session does not refresh it, recording it again does (pi's
// delete-and-set on a Map, with MAX_THREAD_STATE_ENTRIES = 10000).
func TestAnthropicThreadStoreEvictsLeastRecentlyUpdated(t *testing.T) {
	if maxAnthropicThreadSessions != 10_000 {
		t.Errorf("cap %d, pi's MAX_THREAD_STATE_ENTRIES is 10000", maxAnthropicThreadSessions)
	}
	s := newAnthropicThreadStore(2)
	a, b, c := &anthropicThreadState{replyID: "a"}, &anthropicThreadState{replyID: "b"}, &anthropicThreadState{replyID: "c"}
	s.set("a", a)
	s.set("b", b)
	s.get("a")    // a read does not refresh a
	s.set("b", b) // an update refreshes b
	s.set("c", c)
	if s.get("a") != nil || s.get("b") != b || s.get("c") != c {
		t.Errorf("after the cap: a=%v b=%v c=%v, want a evicted", s.get("a"), s.get("b"), s.get("c"))
	}
	s.set("b", anthropicThreadOff)
	s.set("a", a)
	if s.get("c") != nil || s.get("b") != anthropicThreadOff || s.get("a") != a {
		t.Errorf("after re-recording b: a=%v b=%v c=%v, want c evicted", s.get("a"), s.get("b"), s.get("c"))
	}
}

// A reply can be continued once: of two requests that both found it, only the
// first takes it, and the other starts a new thread.
func TestAnthropicThreadStoreTakeIsOnce(t *testing.T) {
	s := newAnthropicThreadStore(10)
	state := &anthropicThreadState{replyID: "r"}
	s.set("s", state)
	if !s.take("s", state) {
		t.Fatal("first take failed")
	}
	if s.take("s", state) {
		t.Error("second take succeeded")
	}
	newer := &anthropicThreadState{replyID: "newer"}
	s.set("s", newer)
	if s.take("s", state) || s.get("s") != newer {
		t.Error("a stale take removed a newer state")
	}
}

// Concurrent requests of one session never both continue the same reply.
// Characterization and -race coverage: the window it guards is too narrow to
// hit reliably, so a non-atomic take can pass it; TestAnthropicThreadStoreTakeIsOnce
// locks the primitive itself.
func TestAnthropicThreadConcurrentRequestsContinueOnce(t *testing.T) {
	h := newThreadHarness(t)
	messages := []ai.Message{threadUser("first")}
	h.say(t, &messages, threadTurn{})
	messages = append(messages, threadUser("second"))
	model := threadBaseModel()
	opts := h.options()
	req := ai.NormalizeContext(ai.Context{SystemPrompt: "You are a test assistant.", Tools: []ai.Tool{threadTool("lookup")}, Messages: messages})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { StreamAnthropic(t.Context(), &model, req, &opts).Result() })
	}
	wg.Wait()
	continues := 0
	for _, r := range h.sent()[1:] {
		if r.threadType() == "continue" {
			continues++
		}
	}
	if continues != 1 {
		t.Errorf("%d requests continued the reply, want 1", continues)
	}
}
