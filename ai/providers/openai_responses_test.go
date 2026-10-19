package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

const responsesSSE = `data: {"type":"response.created","response":{"id":"resp_1"}}

data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}

data: {"type":"response.reasoning_summary_text.delta","delta":"pondering"}

data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"pondering"}]}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"Answer: "}

data: {"type":"response.output_text.delta","delta":"42"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"Answer: 42"}]}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":""}}

data: {"type":"response.function_call_arguments.delta","delta":"{\"x\":1}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":"{\"x\":1}"}}

data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28,"input_tokens_details":{"cached_tokens":5}}}}

`

func TestOpenAIResponsesProviderParsesStream(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, responsesSSE)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: server.URL,
		Reasoning: true, MaxTokens: 4096, Cost: ai.ModelCost{Input: 1.25, Output: 10},
	}
	req := ai.Context{
		SystemPrompt: "be terse",
		Messages:     []ai.Message{ai.NewUserText("what is 6*7?", 1)},
		Tools:        []ai.Tool{{Name: "calc", Description: "calc", Parameters: ai.Object(ai.Prop("x", ai.Integer()))}},
	}
	final := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}, ReasoningEffort: "medium"}).Result()

	if final.StopReason != ai.StopToolUse {
		t.Fatalf("expected toolUse, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
	var thinking, text string
	var tool *ai.ToolCall
	for _, c := range final.Content {
		switch v := c.(type) {
		case ai.ThinkingContent:
			thinking = v.Thinking
			if v.ThinkingSignature == "" {
				t.Errorf("reasoning signature not captured")
			}
		case ai.TextContent:
			text = v.Text
		case ai.ToolCall:
			tc := v
			tool = &tc
		}
	}
	if thinking != "pondering" {
		t.Fatalf("thinking wrong: %q", thinking)
	}
	if text != "Answer: 42" {
		t.Fatalf("text wrong: %q", text)
	}
	if tool == nil || tool.Name != "calc" || tool.ID != "call_1|fc_1" {
		t.Fatalf("tool wrong: %#v", tool)
	}
	if v, _ := tool.Arguments["x"].(float64); v != 1 {
		t.Fatalf("tool args wrong: %#v", tool.Arguments)
	}
	if final.Usage.Input != 15 || final.Usage.CacheRead != 5 || final.Usage.Output != 8 {
		t.Fatalf("usage wrong: %+v", final.Usage)
	}
	// Request must use developer role for reasoning model + input array.
	if _, ok := gotBody["input"]; !ok {
		t.Fatalf("input not sent: %v", gotBody)
	}
	if _, ok := gotBody["reasoning"]; !ok {
		t.Fatalf("reasoning param not sent: %v", gotBody)
	}
}

// Regression (found via live reasoning round-trip, 2026-06-08): with store:false,
// a reasoning request must set include:["reasoning.encrypted_content"] so the
// reasoning item can be replayed inline on the next turn without a 404.
func TestResponsesReasoningIncludesEncryptedContent(t *testing.T) {
	model := &ai.Model{ID: "gpt-5-mini", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, MaxTokens: 1024}
	body := mustBuildResponsesParams(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIResponsesOptions{ReasoningEffort: "medium"})

	inc, ok := body["include"].([]any)
	if !ok || len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("expected include=[reasoning.encrypted_content], got %v", body["include"])
	}
	r, _ := body["reasoning"].(map[string]any)
	if r["effort"] != "medium" || r["summary"] != "auto" {
		t.Fatalf("reasoning block wrong: %v", r)
	}
	if body["store"] != false {
		t.Fatalf("store should be false")
	}
}

// Non-reasoning requests must not send include/reasoning.
func TestResponsesNonReasoningNoInclude(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: false, MaxTokens: 1024}
	body := mustBuildResponsesParams(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, &OpenAIResponsesOptions{})
	if _, ok := body["include"]; ok {
		t.Fatalf("non-reasoning model must not send include")
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("non-reasoning model must not send reasoning")
	}
}

// A set ToolChoice is forwarded verbatim as the tool_choice request param
// alongside the tools; unset (nil) omits it so the API defaults to "auto".
// Mirrors pi's "forwards required tool choice" case (upstream eacaa130).
func TestResponsesToolChoiceForwarded(t *testing.T) {
	model := &ai.Model{ID: "gpt-5.4", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, MaxTokens: 4096}
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("Do not call ping. Respond with text instead.", 1)},
		Tools:    []ai.Tool{{Name: "ping", Description: "Ping", Parameters: ai.Object(ai.Prop("value", ai.String()))}},
	}

	with := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{ToolChoice: "required"})
	if with["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", with["tool_choice"])
	}
	if names := responsesToolNames(with); len(names) != 1 || names[0] != "ping" {
		t.Fatalf("tools = %v, want [ping]", names)
	}

	without := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})
	if _, ok := without["tool_choice"]; ok {
		t.Fatalf("tool_choice must be omitted when ToolChoice is unset, got %v", without["tool_choice"])
	}
}

// responsesToolNames reads the names of a built body's top-level tools.
func responsesToolNames(body map[string]any) []string {
	tools, _ := body["tools"].([]map[string]any)
	out := make([]string, len(tools))
	for i, tool := range tools {
		out[i], _ = tool["name"].(string)
	}
	return out
}

// mustResponsesInput converts messages, failing the test on conversion errors.
// The context is normalized first, as every stream entry point does.
func mustResponsesInput(t *testing.T, model *ai.Model, req ai.Context) []any {
	t.Helper()
	in, err := responsesInput(model, ai.NormalizeContext(req), "")
	if err != nil {
		t.Fatalf("responsesInput: %v", err)
	}
	return in
}

// mustBuildResponsesParams builds request params, failing the test on errors.
func mustBuildResponsesParams(t *testing.T, model *ai.Model, req ai.Context, opts *OpenAIResponsesOptions) map[string]any {
	t.Helper()
	params, err := buildResponsesParams(model, ai.NormalizeContext(req), opts)
	if err != nil {
		t.Fatalf("buildResponsesParams: %v", err)
	}
	return params
}

// runResponsesSSE streams a raw SSE body through the provider and returns the
// final assistant message.
func runResponsesSSE(t *testing.T, model *ai.Model, req ai.Context, sse string) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	m := *model
	m.BaseURL = server.URL
	return StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(req),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}).
		Result()
}

func reasoningModel() *ai.Model {
	return &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, MaxTokens: 4096}
}

// Multi-part reasoning summary parts must be joined by "\n\n".
func TestResponsesMultiPartReasoningSummary(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}

data: {"type":"response.reasoning_summary_text.delta","delta":"first"}

data: {"type":"response.reasoning_summary_part.done"}

data: {"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}

data: {"type":"response.reasoning_summary_text.delta","delta":"second"}

data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	var thinking string
	for _, c := range final.Content {
		if tc, ok := c.(ai.ThinkingContent); ok {
			thinking = tc.Thinking
		}
	}
	if thinking != "first\n\nsecond" {
		t.Fatalf("summary join wrong: %q", thinking)
	}
}

// Azure OpenAI can omit reasoning.encrypted_content from
// response.output_item.done and provide it only in
// response.completed.response.output. The persisted reasoning signature must be
// backfilled from the terminal response so store:false replay stays stateless
// (port of upstream 1f0dbc00, https://github.com/earendil-works/pi/issues/6409).
func TestResponsesBackfillReasoningEncryptedContent(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_summary_text.delta","delta":"think"}

data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"ENC-123"}]}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	var sig string
	for _, c := range final.Content {
		if tc, ok := c.(ai.ThinkingContent); ok {
			sig = tc.ThinkingSignature
		}
	}
	if sig == "" {
		t.Fatalf("no thinking signature captured")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(sig), &parsed); err != nil {
		t.Fatalf("signature is not valid JSON: %q (%v)", sig, err)
	}
	if parsed["encrypted_content"] != "ENC-123" {
		t.Fatalf("encrypted_content not backfilled onto signature: %q", sig)
	}
	// The original reasoning fields must survive the backfill.
	if parsed["id"] != "rs_1" || parsed["type"] != "reasoning" {
		t.Fatalf("backfill dropped original reasoning fields: %q", sig)
	}
}

// A refusal must surface as text via response.refusal.delta.
func TestResponsesRefusalDelta(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"refusal","refusal":""}}

data: {"type":"response.refusal.delta","delta":"I cannot "}

data: {"type":"response.refusal.delta","delta":"help with that"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"refusal","refusal":"I cannot help with that"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	var text, sig string
	for _, c := range final.Content {
		if tc, ok := c.(ai.TextContent); ok {
			text = tc.Text
			sig = tc.TextSignature
		}
	}
	if text != "I cannot help with that" {
		t.Fatalf("refusal text wrong: %q", text)
	}
	if sig != `{"v":1,"id":"msg_1"}` {
		t.Fatalf("text signature wrong: %q", sig)
	}
}

// pi 2d597f02: a message item whose `content` is null must not crash and yields
// empty text (pi guards with `item.content?.map(...) ?? "" `). In Go this is
// structurally safe — ranging a nil slice is a no-op, so no guard is needed; the
// rebuild produces "". This test locks that equivalence so a future refactor that
// dereferences content can't regress it.
func TestResponsesNullMessageContent(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"partial"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":null}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	for _, c := range final.Content {
		if tc, ok := c.(ai.TextContent); ok && tc.Text != "" {
			t.Fatalf("null content must rebuild to empty text, got %q", tc.Text)
		}
	}
}

// A provider that emits only function_call_arguments.done (no deltas) must still
// yield full args, and the trailing delta must be emitted.
func TestResponsesFunctionCallArgumentsDoneOnly(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":""}}

data: {"type":"response.function_call_arguments.done","arguments":"{\"x\":7}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":"{\"x\":7}"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	model := reasoningModel()
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools:    []ai.Tool{{Name: "calc", Description: "calc", Parameters: ai.Object(ai.Prop("x", ai.Integer()))}},
	}
	var sawDelta bool
	stream := func() *ai.AssistantMessageEventStream {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, sse)
		}))
		t.Cleanup(server.Close)
		m := *model
		m.BaseURL = server.URL
		return StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(req),
			&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

	}()
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallDelta && ev.Delta == `{"x":7}` {
			sawDelta = true
		}
	}
	final := stream.Result()
	if !sawDelta {
		t.Fatalf("expected trailing toolcall_delta with full args")
	}
	var tool *ai.ToolCall
	for _, c := range final.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			v := tc
			tool = &v
		}
	}
	if tool == nil {
		t.Fatalf("no tool call")
	}
	if v, _ := tool.Arguments["x"].(float64); v != 7 {
		t.Fatalf("args wrong: %#v", tool.Arguments)
	}
}

// Assistant text with a textSignature replays as a message item carrying that id.
func TestResponsesAssistantTextReplaySignature(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{
				Content:    ai.ContentList{ai.TextContent{Text: "prior", TextSignature: `{"v":1,"id":"msg_abc","phase":"final_answer"}`}},
				Api:        ai.APIOpenAIResponses,
				Provider:   "openai",
				Model:      "gpt-5",
				StopReason: ai.StopStop,
			},
			ai.NewUserText("again", 2),
		},
	}
	input := mustResponsesInput(t, model, req)
	var msgItem map[string]any
	for _, it := range input {
		if m, ok := it.(map[string]any); ok && m["type"] == "message" && m["role"] == "assistant" {
			msgItem = m
		}
	}
	if msgItem == nil {
		t.Fatalf("no assistant message item: %#v", input)
	}
	if msgItem["id"] != "msg_abc" {
		t.Fatalf("id wrong: %v", msgItem["id"])
	}
	if msgItem["phase"] != "final_answer" {
		t.Fatalf("phase wrong: %v", msgItem["phase"])
	}
}

// Foreign assistant tool-call ids get hashed into fc_<shortHash>; cross-model
// (same provider/api, different model id) fc_ ids are dropped.
func TestResponsesToolCallIDNormalization(t *testing.T) {
	model := reasoningModel()
	// Foreign source (anthropic): item id hashed into fc_ prefix.
	foreign := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ToolCall{ID: "call_1|toolu_xyz", Name: "calc", Arguments: map[string]any{}}},
			Api:        ai.APIAnthropicMessages,
			Provider:   "anthropic",
			Model:      "claude",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1|toolu_xyz", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	in := mustResponsesInput(t, model, foreign)
	var fc map[string]any
	for _, it := range in {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call" {
			fc = m
		}
	}
	if fc == nil {
		t.Fatalf("no function_call item: %#v", in)
	}
	id, _ := fc["id"].(string)
	if !strings.HasPrefix(id, "fc_") {
		t.Fatalf("foreign item id should be fc_-hashed, got %q", id)
	}
	if id == "fc_toolu_xyz" {
		t.Fatalf("foreign item id should be hashed, not raw: %q", id)
	}
	if fc["call_id"] != "call_1" {
		t.Fatalf("call_id wrong: %v", fc["call_id"])
	}

	// Cross-model (same provider/api, different model id): fc_ item id dropped.
	crossModel := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ToolCall{ID: "call_9|fc_old", Name: "calc", Arguments: map[string]any{}}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-4.1",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_9|fc_old", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	in2 := mustResponsesInput(t, model, crossModel)
	for _, it := range in2 {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call" {
			if _, has := m["id"]; has {
				t.Fatalf("cross-model fc_ id should be dropped, got %v", m["id"])
			}
			if m["call_id"] != "call_9" {
				t.Fatalf("call_id wrong: %v", m["call_id"])
			}
		}
	}
}

// Tool results containing images are emitted as input_image content parts.
func TestResponsesImageToolResult(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1|fc_1", Name: "shot", Arguments: map[string]any{}}},
			Api:     ai.APIOpenAIResponses, Provider: "openai", Model: "gpt-5", StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{
			ToolCallID: "call_1|fc_1", ToolName: "shot",
			Content:   ai.ContentList{ai.TextContent{Text: "captured"}, ai.ImageContent{MimeType: "image/png", Data: "QUJD"}},
			Timestamp: 2,
		},
	}}
	in := mustResponsesInput(t, model, req)
	var out any
	for _, it := range in {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call_output" {
			out = m["output"]
		}
	}
	parts, ok := out.([]any)
	if !ok {
		t.Fatalf("image tool result output should be content-parts, got %T %#v", out, out)
	}
	var sawText, sawImage bool
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		switch pm["type"] {
		case "input_text":
			sawText = pm["text"] == "captured"
		case "input_image":
			img, _ := pm["image_url"].(string)
			sawImage = strings.HasPrefix(img, "data:image/png;base64,QUJD")
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("expected input_text + input_image parts, got %#v", parts)
	}
}

// Non-vision model: image-only tool result falls back to "(see attached image)".
func TestResponsesImageToolResultNonVision(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, Input: []string{"text"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1|fc_1", Name: "shot", Arguments: map[string]any{}}},
			Api:     ai.APIOpenAIResponses, Provider: "openai", Model: "gpt-5", StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{
			ToolCallID: "call_1|fc_1", ToolName: "shot",
			Content:   ai.ContentList{ai.ImageContent{MimeType: "image/png", Data: "QUJD"}},
			Timestamp: 2,
		},
	}}
	in := mustResponsesInput(t, model, req)
	for _, it := range in {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call_output" {
			// transform downgrades the image to a placeholder for non-vision models,
			// so the text path is taken (placeholder text), not "(see attached image)".
			if _, isParts := m["output"].([]any); isParts {
				t.Fatalf("non-vision model must not emit input_image parts: %#v", m["output"])
			}
		}
	}
}

// Upstream 24bace27 flipped supportsStrictMode's default to false and made
// convertResponsesTools emit `strict` only where the provider supports it, so
// responses tools no longer carry an unconditional strict:false. pi 0.82.0:
//
//	compat undefined            -> [{"type":"function","name":"calc",…}]
//	{"supportsStrictMode":true} -> [{…,"strict":false}]
func TestResponsesToolsStrictOnlyWhenSupported(t *testing.T) {
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools:    []ai.Tool{{Name: "calc", Description: "calc", Parameters: ai.Object(ai.Prop("x", ai.Integer()))}},
	}
	toolOf := func(compat json.RawMessage) map[string]any {
		t.Helper()
		model := reasoningModel()
		model.Compat = compat
		body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})
		tools, ok := body["tools"].([]map[string]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools wrong: %#v", body["tools"])
		}
		return tools[0]
	}

	tool := toolOf(nil)
	if _, has := tool["strict"]; has {
		t.Fatalf("strict must be omitted when the model does not set supportsStrictMode: %#v", tool)
	}
	if tool["type"] != "function" || tool["name"] != "calc" {
		t.Fatalf("tool body shape wrong: %#v", tool)
	}

	tool = toolOf(json.RawMessage(`{"supportsStrictMode":true}`))
	if strict, has := tool["strict"]; !has || strict != false {
		t.Fatalf("supportsStrictMode:true must send strict:false, got %v (has=%v)", strict, has)
	}
}

// System role is developer only when reasoning && compat.supportsDeveloperRole != false.
func TestResponsesDeveloperRoleGating(t *testing.T) {
	firstRole := func(in []any) string {
		for _, it := range in {
			if m, ok := it.(map[string]any); ok {
				if r, has := m["role"]; has {
					return r.(string)
				}
			}
		}
		return ""
	}
	req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	reasoning := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true}
	if got := firstRole(mustResponsesInput(t, reasoning, req)); got != "developer" {
		t.Fatalf("reasoning model should use developer role, got %q", got)
	}

	nonReasoning := &ai.Model{ID: "gpt-4o", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: false}
	if got := firstRole(mustResponsesInput(t, nonReasoning, req)); got != "system" {
		t.Fatalf("non-reasoning model should use system role, got %q", got)
	}

	noDevRole := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true,
		Compat: json.RawMessage(`{"supportsDeveloperRole":false}`)}
	if got := firstRole(mustResponsesInput(t, noDevRole, req)); got != "system" {
		t.Fatalf("supportsDeveloperRole:false should use system role, got %q", got)
	}
}

// github-copilot reasoning-off must not send a reasoning block.
func TestResponsesReasoningOffExcludesCopilot(t *testing.T) {
	copilot := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "github-copilot", Reasoning: true}
	body := mustBuildResponsesParams(t, copilot, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, &OpenAIResponsesOptions{})
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("github-copilot reasoning-off must omit reasoning, got %v", body["reasoning"])
	}

	other := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true}
	body2 := mustBuildResponsesParams(t, other, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, &OpenAIResponsesOptions{})
	r, ok := body2["reasoning"].(map[string]any)
	if !ok || r["effort"] != "none" {
		t.Fatalf("non-copilot reasoning-off should send effort:none, got %v", body2["reasoning"])
	}
}

// mapResponsesStatus ports pi's status mapping incl. unknown→error. Upstream
// 32850ef7 qualifies "incomplete" by the provider's incomplete_details.reason:
// only max_output_tokens is a length stop, everything else is an error carrying
// the message the stream fails with.
func TestResponsesMapStatus(t *testing.T) {
	cases := []struct {
		status  string
		reason  string
		want    ai.StopReason
		wantMsg string
		err     bool
	}{
		{status: "", want: ai.StopStop},
		{status: "completed", want: ai.StopStop},
		{status: "incomplete", reason: "max_output_tokens", want: ai.StopLength},
		{status: "incomplete", reason: "content_filter", want: ai.StopError, wantMsg: "Response incomplete: content_filter"},
		{status: "incomplete", reason: "max_time_limit", want: ai.StopError, wantMsg: "Response incomplete: max_time_limit"},
		{status: "incomplete", want: ai.StopError, wantMsg: "Response incomplete without a provider reason"},
		{status: "failed", want: ai.StopError},
		{status: "cancelled", want: ai.StopError},
		{status: "in_progress", want: ai.StopStop},
		{status: "queued", want: ai.StopStop},
		{status: "weird", want: ai.StopStop, err: true},
	}
	for _, c := range cases {
		got, msg, err := mapResponsesStatus(c.status, c.reason)
		if (err != nil) != c.err {
			t.Fatalf("status %q/%q err=%v want err=%v", c.status, c.reason, err, c.err)
		}
		if c.err {
			continue
		}
		if got != c.want {
			t.Fatalf("status %q/%q got %s want %s", c.status, c.reason, got, c.want)
		}
		if msg != c.wantMsg {
			t.Fatalf("status %q/%q message = %q, want %q", c.status, c.reason, msg, c.wantMsg)
		}
	}
}

// response.failed must surface error.code/message or incomplete_details.reason.
func TestResponsesFailedSurfacesDetail(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"rate_limit","message":"slow down"}}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop reason, got %s", final.StopReason)
	}
	if final.ErrorMessage != "rate_limit: slow down" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// TestResponsesRawStopReason mirrors pi's openai-responses-terminal-event.test.ts
// additions in d7b02636: the response status is preserved verbatim on
// rawStopReason from every terminal event, including response.failed.
func TestResponsesRawStopReason(t *testing.T) {
	tests := []struct {
		name     string
		event    string
		wantStop ai.StopReason
		wantRaw  string
		wantErr  string
	}{
		{
			name:     "completed",
			event:    `data: {"type":"response.completed","response":{"id":"r","status":"completed"}}`,
			wantStop: ai.StopStop,
			wantRaw:  "completed",
		},
		{
			// Upstream 32850ef7 qualified this fixture with incomplete_details:
			// a truncated response is a length stop and rawStopReason carries
			// the provider's reason.
			name:     "incomplete truncated at max output tokens",
			event:    `data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
			wantStop: ai.StopLength,
			wantRaw:  "incomplete.max_output_tokens",
		},
		{
			// Content filtering is not resumable: an error carrying the reason.
			name:     "incomplete from content filtering",
			event:    `data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`,
			wantStop: ai.StopError,
			wantRaw:  "incomplete.content_filter",
			wantErr:  "Response incomplete: content_filter",
		},
		{
			// Unknown provider reasons are preserved verbatim, still as errors.
			name:     "incomplete for an unknown provider reason",
			event:    `data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_time_limit"}}}`,
			wantStop: ai.StopError,
			wantRaw:  "incomplete.max_time_limit",
			wantErr:  "Response incomplete: max_time_limit",
		},
		{
			// No incomplete_details at all: nothing to qualify rawStopReason
			// with, and no reason to believe the stop is resumable.
			name:     "incomplete without a provider reason",
			event:    `data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete"}}`,
			wantStop: ai.StopError,
			wantRaw:  "incomplete",
			wantErr:  "Response incomplete without a provider reason",
		},
		{
			name:     "failed",
			event:    `data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
			wantStop: ai.StopError,
			wantRaw:  "failed",
			wantErr:  "server_error: boom",
		},
		{
			// pi's finalizeResponse opens with `response.output` (upstream
			// 1f0dbc00), so a terminal event without a response throws before
			// rawStopReason is assigned.
			name:     "completed without a response object",
			event:    `data: {"type":"response.completed"}`,
			wantStop: ai.StopError,
			wantRaw:  "",
			wantErr:  "Cannot read properties of undefined (reading 'output')",
		},
		{
			// pi assigns `event.response?.status` unconditionally on
			// response.failed, so a failure event carrying no response CLEARS the
			// status an earlier terminal event recorded — it must not report a
			// stale "completed".
			name: "failed without a response object clears an earlier status",
			event: `data: {"type":"response.completed","response":{"id":"r","status":"completed"}}` + "\n\n" +
				`data: {"type":"response.failed"}`,
			wantStop: ai.StopError,
			wantRaw:  "",
			wantErr:  "Unknown error (no error details in response)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"type":"response.created","response":{"id":"r"}}` + "\n\n" + tt.event + "\n\n"
			final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
			if final.StopReason != tt.wantStop {
				t.Fatalf("stopReason = %s, want %s (%s)", final.StopReason, tt.wantStop, final.ErrorMessage)
			}
			if final.RawStopReason != tt.wantRaw {
				t.Fatalf("rawStopReason = %q, want %q", final.RawStopReason, tt.wantRaw)
			}
			if final.ErrorMessage != tt.wantErr {
				t.Fatalf("errorMessage = %q, want %q", final.ErrorMessage, tt.wantErr)
			}
		})
	}
}

// Unknown response.completed status fails the stream (pi throws).
func TestResponsesUnknownStatusFails(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.completed","response":{"id":"r","status":"bogus"}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopError {
		t.Fatalf("unknown status should fail, got %s", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "Unhandled stop reason") {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// Prompt-cache-key clamp must count code points, not bytes.
func TestResponsesPromptCacheKeyClampMultibyte(t *testing.T) {
	// 70 multibyte runes (each 3 bytes in UTF-8) -> 210 bytes.
	key := strings.Repeat("あ", 70)
	got := clampPromptCacheKey(key)
	if n := len([]rune(got)); n != 64 {
		t.Fatalf("clamp should keep 64 code points, got %d", n)
	}
	if got != strings.Repeat("あ", 64) {
		t.Fatalf("clamp result wrong: %q", got)
	}

	short := strings.Repeat("a", 10)
	if clampPromptCacheKey(short) != short {
		t.Fatalf("short key should pass through")
	}
}

// ---------------------------------------------------------------------------
// Parity sweep 2: A7 + D2-D7
// ---------------------------------------------------------------------------

// runResponsesSSEOpts is runResponsesSSE with caller-controlled options.
func runResponsesSSEOpts(t *testing.T, model *ai.Model, req ai.Context, sse string, opts *OpenAIResponsesOptions) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	m := *model
	m.BaseURL = server.URL
	if opts == nil {
		opts = &OpenAIResponsesOptions{}
	}
	if opts.APIKey == "" {
		opts.APIKey = "sk"
	}
	return StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(req), opts).Result()
}

func findResponsesItem(in []any, itemType string) map[string]any {
	for _, it := range in {
		if m, ok := it.(map[string]any); ok && m["type"] == itemType {
			return m
		}
	}
	return nil
}

// A7: same-model replays send tool-call ids verbatim — no normalization, no
// truncation (pi transform-messages.ts:133 gates normalizeToolCallId on
// !isSameModel; raw Responses ids are 450+ chars).
func TestResponsesSameModelToolCallIDReplayedVerbatim(t *testing.T) {
	model := reasoningModel() // gpt-5 / openai / openai-responses
	longCall := "call_" + strings.Repeat("x", 80)
	longItem := "fc_" + strings.Repeat("Y", 460)
	rawID := longCall + "|" + longItem
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ToolCall{ID: rawID, Name: "calc", Arguments: map[string]any{}}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-5",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: rawID, ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	in := mustResponsesInput(t, model, req)
	fc := findResponsesItem(in, "function_call")
	if fc == nil {
		t.Fatalf("no function_call item: %#v", in)
	}
	if fc["call_id"] != longCall {
		t.Fatalf("same-model call_id must be raw, got %v", fc["call_id"])
	}
	if fc["id"] != longItem {
		t.Fatalf("same-model item id must be raw (>64 chars untouched), got %v", fc["id"])
	}
	fco := findResponsesItem(in, "function_call_output")
	if fco == nil || fco["call_id"] != longCall {
		t.Fatalf("tool result call_id must be raw, got %#v", fco)
	}
}

// A7 (cross-model still normalized): special characters in the callId half are
// sanitized when the source is a different model.
func TestResponsesCrossModelToolCallIDStillNormalized(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ToolCall{ID: "call@9|fc_old", Name: "calc", Arguments: map[string]any{}}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-4.1", // different model id, same provider/api
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call@9|fc_old", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	in := mustResponsesInput(t, model, req)
	fc := findResponsesItem(in, "function_call")
	if fc == nil || fc["call_id"] != "call_9" {
		t.Fatalf("cross-model call_id should be sanitized to call_9, got %#v", fc)
	}
	if _, has := fc["id"]; has {
		t.Fatalf("cross-model fc_ item id should be dropped, got %v", fc["id"])
	}
	fco := findResponsesItem(in, "function_call_output")
	if fco == nil || fco["call_id"] != "call_9" {
		t.Fatalf("tool result should pick up the normalized id, got %#v", fco)
	}
}

// D2: service_tier is sent when set and omitted otherwise.
func TestResponsesServiceTierParam(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{ServiceTier: "flex"})
	if body["service_tier"] != "flex" {
		t.Fatalf("service_tier not sent: %v", body["service_tier"])
	}
	body2 := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})
	if _, has := body2["service_tier"]; has {
		t.Fatalf("service_tier must be omitted when unset")
	}
}

const responsesPricingSSEFmt = `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"hi"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hi"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed",%s"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28,"input_tokens_details":{"cached_tokens":0}}}}

`

// TestResponsesReasoningTokens checks output_tokens_details.reasoning_tokens
// populates Usage.Reasoning (pi's `reasoning: ... || 0`).
func TestResponsesReasoningTokens(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true,
		Cost: ai.ModelCost{Input: 1.25, Output: 10}}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":18,\"total_tokens\":38,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":12}}}}\n\n"
	final := runResponsesSSEOpts(t, model, req, sse, &OpenAIResponsesOptions{})
	if final.Usage.Reasoning != 12 {
		t.Fatalf("expected reasoning 12, got %+v", final.Usage)
	}
	if final.Usage.Output != 18 {
		t.Fatalf("output wrong: %+v", final.Usage)
	}
}

// D2: flex halves cost, priority and fast double it (×2.5 for the exact id
// gpt-5.5), and the response-reported service tier wins over the requested
// option. "fast" is Fast mode's name for priority processing: GPT-6 models
// report it even when "priority" was requested (upstream a6ca86102).
func TestResponsesServiceTierPricing(t *testing.T) {
	costModel := func(id string) *ai.Model {
		return &ai.Model{ID: id, Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true,
			Cost: ai.ModelCost{Input: 1.25, Output: 10}}
	}
	base := 1.25/1_000_000*20 + 10.0/1_000_000*8
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	approx := func(a, b float64) bool {
		d := a - b
		return d < 1e-15 && d > -1e-15
	}

	cases := []struct {
		name     string
		model    *ai.Model
		opts     *OpenAIResponsesOptions
		respTier string // injected into response.completed when non-empty
		want     float64
	}{
		{"default", costModel("gpt-5"), &OpenAIResponsesOptions{}, "", base},
		{"flex-halves", costModel("gpt-5"), &OpenAIResponsesOptions{ServiceTier: "flex"}, "", base * 0.5},
		{"priority-doubles", costModel("gpt-5"), &OpenAIResponsesOptions{ServiceTier: "priority"}, "", base * 2},
		{"gpt-5.5-priority-x2.5", costModel("gpt-5.5"), &OpenAIResponsesOptions{ServiceTier: "priority"}, "", base * 2.5},
		{"response-tier-wins", costModel("gpt-5"), &OpenAIResponsesOptions{ServiceTier: "priority"}, `"service_tier":"default",`, base},
		{"response-flex-without-option", costModel("gpt-5"), &OpenAIResponsesOptions{}, `"service_tier":"flex",`, base * 0.5},
		{"response-fast-for-requested-priority", costModel("gpt-6-luna"), &OpenAIResponsesOptions{ServiceTier: "priority"}, `"service_tier":"fast",`, base * 2},
		{"fast-doubles", costModel("gpt-6-luna"), &OpenAIResponsesOptions{ServiceTier: "fast"}, `"service_tier":"fast",`, base * 2},
		{"gpt-5.5-fast-x2.5", costModel("gpt-5.5"), &OpenAIResponsesOptions{ServiceTier: "fast"}, "", base * 2.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sse := fmt.Sprintf(responsesPricingSSEFmt, c.respTier)
			final := runResponsesSSEOpts(t, c.model, req, sse, c.opts)
			if final.StopReason != ai.StopStop {
				t.Fatalf("unexpected stop: %s (%s)", final.StopReason, final.ErrorMessage)
			}
			if !approx(final.Usage.Cost.Total, c.want) {
				t.Fatalf("cost total %v want %v", final.Usage.Cost.Total, c.want)
			}
		})
	}
}

// serviceTierCaptureFile is written by testdata/service-tier/capture.mjs, which
// streams the published pi-ai build's openai-responses adapter.
const serviceTierCaptureFile = "testdata/service-tier/service-tier-0.87.1.json"

// TestResponsesServiceTierPricingMatchesPi replays each captured stream at the
// captured rates and requires pi's exact usage.cost doubles, not a tolerance:
// the cost is persisted in sessions and sent over the protocol. The gpt-5.5
// priority rows (x2.5) are usages where a fused multiply-add in the re-added
// total (one rounding where V8 rounds twice) moves the last bit, on a target
// the compiler fuses on (arm64; amd64 at GOAMD64=v3): one row per bucket, the
// cacheWrite one at a synthetic rate the row carries.
func TestResponsesServiceTierPricingMatchesPi(t *testing.T) {
	data, err := os.ReadFile(serviceTierCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/service-tier/capture.mjs)", serviceTierCaptureFile, err)
	}
	var capture struct {
		Rows []struct {
			Model       string           `json:"model"`
			Rates       ai.ModelCost     `json:"rates"`
			ServiceTier string           `json:"serviceTier"`
			SSE         string           `json:"sse"`
			Cost        ai.CostBreakdown `json:"cost"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", serviceTierCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", serviceTierCaptureFile)
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	for i, row := range capture.Rows {
		provider, id, _ := strings.Cut(row.Model, "/")
		base := ai.GetModel(provider, id)
		if base == nil {
			t.Fatalf("row %d: catalog has no %s", i, row.Model)
		}
		model := *base
		model.Cost = row.Rates
		final := runResponsesSSEOpts(t, &model, req, row.SSE, &OpenAIResponsesOptions{ServiceTier: row.ServiceTier})
		if final.StopReason != ai.StopStop {
			t.Fatalf("row %d: stop %s (%s)", i, final.StopReason, final.ErrorMessage)
		}
		got := final.Usage.Cost
		for _, f := range []struct {
			name      string
			got, want float64
		}{
			{"input", got.Input, row.Cost.Input},
			{"output", got.Output, row.Cost.Output},
			{"cacheRead", got.CacheRead, row.Cost.CacheRead},
			{"cacheWrite", got.CacheWrite, row.Cost.CacheWrite},
			{"total", got.Total, row.Cost.Total},
		} {
			if math.Float64bits(f.got) != math.Float64bits(f.want) {
				t.Errorf("row %d %s serviceTier %q: cost.%s = %v, pi %v", i, row.Model, row.ServiceTier, f.name, f.got, f.want)
			}
		}
	}
}

// D3: session cache headers — shape selected by compat.sessionAffinityFormat
// ("openai" sends session_id + x-client-request-id; "openai-nosession" drops
// session_id; "openrouter" sends x-session-id), all suppressed when
// cacheRetention is "none" (pi openai-responses.ts:115,207-217; upstream 298665cf).
func TestResponsesSessionCacheHeaders(t *testing.T) {
	capture := func(model *ai.Model, opts *OpenAIResponsesOptions) http.Header {
		var got http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n")
		}))
		defer server.Close()
		m := *model
		m.BaseURL = server.URL
		opts.APIKey = "sk"
		StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}), opts).Result()
		return got
	}

	// Default (openai auto-detect): session_id + x-client-request-id, no x-session-id.
	h := capture(reasoningModel(), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1"}})
	if h.Get("session_id") != "sess-1" || h.Get("x-client-request-id") != "sess-1" {
		t.Fatalf("expected both session headers, got session_id=%q x-client-request-id=%q", h.Get("session_id"), h.Get("x-client-request-id"))
	}
	if h.Get("x-session-id") != "" {
		t.Fatalf("openai format must not send x-session-id, got %q", h.Get("x-session-id"))
	}

	// "openai-nosession" drops session_id but keeps x-client-request-id.
	noSidModel := reasoningModel()
	noSidModel.Compat = json.RawMessage(`{"sessionAffinityFormat":"openai-nosession"}`)
	h2 := capture(noSidModel, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1"}})
	if h2.Get("session_id") != "" {
		t.Fatalf("openai-nosession must suppress session_id, got %q", h2.Get("session_id"))
	}
	if h2.Get("x-client-request-id") != "sess-1" {
		t.Fatalf("x-client-request-id must still be sent, got %q", h2.Get("x-client-request-id"))
	}

	// "openrouter" sends only x-session-id (auto-detected from provider here).
	orModel := reasoningModel()
	orModel.Provider = "openrouter"
	h4 := capture(orModel, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1"}})
	if h4.Get("x-session-id") != "sess-1" {
		t.Fatalf("openrouter must send x-session-id, got %q", h4.Get("x-session-id"))
	}
	if h4.Get("session_id") != "" || h4.Get("x-client-request-id") != "" {
		t.Fatalf("openrouter must not send openai headers, got session_id=%q x-client-request-id=%q", h4.Get("session_id"), h4.Get("x-client-request-id"))
	}

	h3 := capture(reasoningModel(), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1", CacheRetention: ai.CacheNone}})
	if h3.Get("session_id") != "" || h3.Get("x-client-request-id") != "" || h3.Get("x-session-id") != "" {
		t.Fatalf("cacheRetention none must suppress all session headers, got session_id=%q x-client-request-id=%q x-session-id=%q", h3.Get("session_id"), h3.Get("x-client-request-id"), h3.Get("x-session-id"))
	}
}

// D4: shortHash iterates UTF-16 code units (JS charCodeAt); vectors verified
// against the pi npm build with node.
func TestResponsesShortHashUTF16Vectors(t *testing.T) {
	cases := map[string]string{
		"":                   "k4n83c7h0j2b",
		"emoji 🙈 id":         "jk0b7r1xq9646",
		"toolu_xyz":          "1j6u6f41xacfv1",
		"🙈🙉🙊":                "1pd5f9x1j6a281",
		"héllo wörld":        "1slrdvn1t61j5h",
		"call_abc123|fc_456": "13c60wm1owxk5l",
	}
	if got := shortHash(strings.Repeat("a", 500)); got != "d33jejlgylnv" {
		t.Errorf("shortHash(a*500) = %q want d33jejlgylnv", got)
	}
	for in, want := range cases {
		if got := shortHash(in); got != want {
			t.Errorf("shortHash(%q) = %q want %q", in, got, want)
		}
	}
}

// D4: normalizeResponsesIDPart replaces each UTF-16 code unit, so an astral
// character becomes TWO underscores (JS regex without /u); node-verified.
func TestResponsesNormalizeIDPartUTF16(t *testing.T) {
	cases := map[string]string{
		"ab🙈cd":                 "ab__cd",
		"call_1|fc_x":           "call_1_fc_x",
		"abc__":                 "abc",
		strings.Repeat("x", 70): strings.Repeat("x", 64),
	}
	for in, want := range cases {
		if got := normalizeResponsesIDPart(in); got != want {
			t.Errorf("normalizeResponsesIDPart(%q) = %q want %q", in, got, want)
		}
	}
}

// D5: a stream whose response.completed carries an error-grade status must
// fail with "An unknown error occurred", never emit done (pi :140-142).
func TestResponsesErrorStopReasonFailsStream(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.completed","response":{"id":"r","status":"cancelled"}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if final.ErrorMessage != "An unknown error occurred" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// D6: providers outside OPENAI_TOOL_CALL_PROVIDERS sanitize the WHOLE raw id
// (pipe → underscore) into call_id and send NO item id (shared :110 + the
// undefined split).
func TestResponsesNonAllowedProviderPipeID(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "github-copilot", Reasoning: true}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ToolCall{ID: "call_1|fc_x", Name: "calc", Arguments: map[string]any{}}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai", // foreign source → normalization applies
			Model:      "gpt-5",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1|fc_x", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	in := mustResponsesInput(t, model, req)
	fc := findResponsesItem(in, "function_call")
	if fc == nil {
		t.Fatalf("no function_call item: %#v", in)
	}
	if fc["call_id"] != "call_1_fc_x" {
		t.Fatalf("whole id should be sanitized into call_id, got %v", fc["call_id"])
	}
	if _, has := fc["id"]; has {
		t.Fatalf("non-allowed provider must not send an item id, got %v", fc["id"])
	}
	fco := findResponsesItem(in, "function_call_output")
	if fco == nil || fco["call_id"] != "call_1_fc_x" {
		t.Fatalf("tool result should pick up the sanitized id, got %#v", fco)
	}
}

// D6: github-copilot requests carry the dynamic copilot headers
// (pi openai-responses.ts:191-198).
func TestResponsesCopilotDynamicHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "github-copilot", Reasoning: true, BaseURL: server.URL}

	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}).Result()
	if got.Get("X-Initiator") != "user" || got.Get("Openai-Intent") != "conversation-edits" {
		t.Fatalf("copilot headers missing: X-Initiator=%q Openai-Intent=%q", got.Get("X-Initiator"), got.Get("Openai-Intent"))
	}
	if got.Get("Copilot-Vision-Request") != "" {
		t.Fatalf("vision header must be absent without images")
	}

	visionReq := ai.Context{Messages: []ai.Message{
		ai.UserMessage{Content: ai.ContentList{ai.ImageContent{MimeType: "image/png", Data: "QUJD"}}, Timestamp: 1},
		ai.NewUserText("done?", 2),
	}}
	model.Input = []string{"text", "image"}
	StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(visionReq), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}).Result()
	if got.Get("Copilot-Vision-Request") != "true" {
		t.Fatalf("vision header missing with image input")
	}
}

// responsesHTTPError streams against a server that answers status/body for the
// given provider id and returns the terminal ErrorMessage.
func responsesHTTPError(t *testing.T, provider string, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer server.Close()
	m := *reasoningModel()
	m.Provider = provider
	m.BaseURL = server.URL
	final := StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}).
		Result()
	return final.ErrorMessage
}

// httpErrorOracleRow is one row of pi's own errorMessage for a non-2xx body,
// captured by testdata/httperror/capture.mjs: from pi-ai's src at 2b0a123de
// (openai 7.19.0), which no published build ships yet, until the first one
// that does replaces it. Goldens come from pi, never from what the porter
// believed.
type httpErrorOracleRow struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
	Body   string `json:"body"`
	// BodyBase64 is a byteRows body, which is not valid UTF-8.
	BodyBase64   []byte            `json:"bodyBase64"`
	ErrorMessage map[string]string `json:"errorMessage"`
	// RetryAfter and MaxRetries are what a failFast row's 429 carried and the
	// stream's maxRetries.
	RetryAfter string `json:"retryAfter"`
	MaxRetries int    `json:"maxRetries"`
}

// httpErrorOracleFile is capture.mjs's output over the published pi-ai 1.3.0
// build (the first that ships ab30693d6; it records the interim 2b0a123de src
// oracle's rows unchanged).
const httpErrorOracleFile = "pi-ai-1.3.0.json"

func loadHTTPErrorCapture(t *testing.T) (rows, failFast []httpErrorOracleRow) {
	t.Helper()
	rows, _, failFast = loadHTTPErrorCaptureWithBytes(t)
	return rows, failFast
}

// loadHTTPErrorCaptureWithBytes also returns the byteRows, whose bodies are
// not valid UTF-8.
func loadHTTPErrorCaptureWithBytes(t *testing.T) (rows, byteRows, failFast []httpErrorOracleRow) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "httperror", httpErrorOracleFile))
	if err != nil {
		t.Fatalf("read oracle: %v", err)
	}
	var doc struct {
		Rows     []httpErrorOracleRow `json:"rows"`
		ByteRows []httpErrorOracleRow `json:"byteRows"`
		FailFast []httpErrorOracleRow `json:"failFast"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode oracle: %v", err)
	}
	if len(doc.Rows) == 0 || len(doc.ByteRows) == 0 || len(doc.FailFast) == 0 {
		t.Fatalf("%s has no rows, byteRows or failFast rows; re-capture it", httpErrorOracleFile)
	}
	return doc.Rows, doc.ByteRows, doc.FailFast
}

func loadHTTPErrorOracle(t *testing.T) []httpErrorOracleRow {
	t.Helper()
	rows, _ := loadHTTPErrorCapture(t)
	return rows
}

// TestResponsesHTTPErrorMatchesPi drives every captured body through the
// adapter and compares the terminal ErrorMessage with pi's byte for byte. pi's
// composition (openai-responses.ts catch -> error-body.ts): the openai SDK's
// APIError message is `${status} ${msg}`; when the APIError's `error` is a
// non-empty plain object — the body's `error` member, or since openai 7 the
// whole body when that member is absent or null — its JSON.stringify form
// (capped at 4000 UTF-16 units) REPLACES that message unless the message
// already contains it.
func TestResponsesHTTPErrorMatchesPi(t *testing.T) {
	for _, row := range loadHTTPErrorOracle(t) {
		t.Run(row.Name, func(t *testing.T) {
			want := row.ErrorMessage["openai-responses"]
			if got := responsesHTTPError(t, "openai", row.Status, row.Body); got != want {
				t.Fatalf("error message = %q, want pi's %q", got, want)
			}
		})
	}
}

// D7a: four readable rows of the oracle above. pi's Responses shape is
// `<label> API error (<status>): <tail>`, the tail being the stringified
// `error` object when the body has one and the SDK's `${status} ${msg}`
// otherwise.
func TestResponsesHTTPErrorFormat(t *testing.T) {
	run := func(status int, body string) string { return responsesHTTPError(t, "openai", status, body) }
	if got := run(429, `{"error":{"message":"slow down"}}`); got != `OpenAI API error (429): {"message":"slow down"}` {
		t.Errorf("json error body: %q", got)
	}
	if got := run(500, "oops"); got != "OpenAI API error (500): 500 oops" {
		t.Errorf("text error body: %q", got)
	}
	if got := run(503, ""); got != "OpenAI API error (503): 503 status code (no body)" {
		t.Errorf("empty error body: %q", got)
	}
	if got := run(400, `{"error":"boom"}`); got != `OpenAI API error (400): 400 "boom"` {
		t.Errorf("string error field: %q", got)
	}
}

// TestResponsesHTTPErrorNamesProvider mirrors upstream 0c7bb7c5c (#9298): the
// label is `${model.provider === "openai" ? "OpenAI" : model.provider}`, so a
// Grok 403 no longer reads as an OpenAI error.
//
// That rule was SOURCE-ONLY until 2026-09-20 — the 0.85.1 build labelled every
// Responses provider "OpenAI", so this test asserted the label from source and
// borrowed one oracle row for the tail. 0.86.1 ships it, and the re-captured
// oracle carries an `openai-responses/xai` column for every row, so the whole
// table is the oracle now: every row against pi instead of one row against a
// hand-written string. It is the only column the 0.86.1 re-capture moved.
//
// `opencode` has no captured column and stays source-derived: same branch, a
// third provider id, asserting that the label is the id verbatim rather than
// anything xai-specific.
func TestResponsesHTTPErrorNamesProvider(t *testing.T) {
	for _, row := range loadHTTPErrorOracle(t) {
		t.Run(row.Name, func(t *testing.T) {
			want, ok := row.ErrorMessage["openai-responses/xai"]
			if !ok {
				t.Fatalf("oracle row %q has no openai-responses/xai column; re-capture it", row.Name)
			}
			if got := responsesHTTPError(t, "xai", row.Status, row.Body); got != want {
				t.Fatalf("xai error message = %q, want pi's %q", got, want)
			}
		})
	}
	const body = `{"error":{"message":"blocked"}}`
	if got, want := responsesHTTPError(t, "opencode", 403, body),
		`opencode API error (403): {"message":"blocked"}`; got != want {
		t.Fatalf("opencode error message = %q, want %q", got, want)
	}
}

// TestHTTPErrorFailFastMatchesPi: a 429 asking for a retry delay past
// maxRetryDelayMs (120s against the 60s default) under maxRetries 1 fails at
// once, and pi's retryProviderRequest quotes the SDK error's message:
// `Server requested 120s retry delay (max: 60s). ${error.message}`. For the
// openai loops that message follows openai 7's makeStatusError, which takes a
// JSON object or array body without a non-null `error` for the error itself;
// anthropic's SDK takes the whole body as it always did.
func TestHTTPErrorFailFastMatchesPi(t *testing.T) {
	_, failFast := loadHTTPErrorCapture(t)
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	for _, row := range failFast {
		for column, want := range row.ErrorMessage {
			t.Run(row.Name+"/"+column, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", row.RetryAfter)
					w.WriteHeader(row.Status)
					io.WriteString(w, row.Body)
				}))
				defer server.Close()
				opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk", MaxRetries: row.MaxRetries}}
				var final *ai.AssistantMessage
				switch column {
				case "openai-responses", "openai-responses/xai":
					m := *reasoningModel()
					m.Provider, m.BaseURL = "openai", server.URL
					if column == "openai-responses/xai" {
						m.Provider = "xai"
					}
					final = StreamOpenAIResponses(t.Context(), &m, req, &OpenAIResponsesOptions{StreamOptions: opts}).Result()
				case "openai-completions":
					m := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL}
					final = StreamOpenAICompletions(t.Context(), m, req, &OpenAIOptions{StreamOptions: opts}).Result()
				case "anthropic-messages":
					m := anthropicUAModel()
					m.BaseURL = server.URL
					final = StreamAnthropic(t.Context(), m, req, &AnthropicOptions{StreamOptions: opts}).Result()
				default:
					t.Fatalf("no Go adapter for column %s", column)
				}
				if final.StopReason != ai.StopError || final.ErrorMessage != want {
					t.Fatalf("stream = %s %q, want pi's %q", final.StopReason, final.ErrorMessage, want)
				}
			})
		}
	}
}

// D7b: max_output_tokens is omitted for 0 (JS truthiness).
func TestResponsesMaxTokensZeroOmitted(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	zero := 0
	body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{MaxTokens: &zero}})
	if _, has := body["max_output_tokens"]; has {
		t.Fatalf("max_output_tokens must be omitted for 0")
	}
	hundred := 100
	body2 := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{MaxTokens: &hundred}})
	if body2["max_output_tokens"] != 100 {
		t.Fatalf("max_output_tokens wrong: %v", body2["max_output_tokens"])
	}
}

// max_output_tokens is floored at 16 — the Responses API rejects lower (#6265).
func TestResponsesMaxTokensFloor(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	small := 8
	body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{MaxTokens: &small}})
	if body["max_output_tokens"] != openaiResponsesMinOutputTokens {
		t.Fatalf("max_output_tokens = %v, want %d floor", body["max_output_tokens"], openaiResponsesMinOutputTokens)
	}
	atFloor := 16
	body2 := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{MaxTokens: &atFloor}})
	if body2["max_output_tokens"] != 16 {
		t.Fatalf("at-floor value must pass through: %v", body2["max_output_tokens"])
	}
}

// Upstream b8b873b98: compat.supportsMaxOutputTokens gates max_output_tokens
// entirely — some Codex-protocol gateways reject the parameter. Unlike the
// other Responses compat opt-ins it defaults to TRUE, so only an explicit
// false drops the parameter, and the floor never resurrects it.
func TestResponsesSupportsMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name      string
		compat    json.RawMessage
		maxTokens int
		want      any // nil means the parameter must be absent
	}{
		{"unset compat defaults to true", nil, 1024, 1024},
		{"explicit true", json.RawMessage(`{"supportsMaxOutputTokens":true}`), 1024, 1024},
		{"unrelated compat keys keep the default", json.RawMessage(`{"supportsToolSearch":true}`), 1024, 1024},
		{"explicit false omits the parameter", json.RawMessage(`{"supportsMaxOutputTokens":false}`), 1024, nil},
		{"explicit false outranks the floor", json.RawMessage(`{"supportsMaxOutputTokens":false}`), 8, nil},
		// A type-mismatched SIBLING key cannot disturb this one: compat keys are
		// resolved independently, as pi's `?? true` does. The full per-key
		// contract for all three compat readers lives in compat_test.go; this
		// case is here because it is the one the difftest scenario
		// `responses-compat-malformed-key` measures against real pi.
		{"mistyped sibling key cannot revert this override",
			json.RawMessage(`{"supportsToolSearch":"yes","supportsMaxOutputTokens":false}`), 1024, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := reasoningModel()
			model.Compat = tc.compat
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			body := mustBuildResponsesParams(t, model, req,
				&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{MaxTokens: &tc.maxTokens}})
			got, has := body["max_output_tokens"]
			if tc.want == nil {
				if has {
					t.Fatalf("max_output_tokens must be omitted, got %v", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("max_output_tokens = %v (present=%v), want %v", got, has, tc.want)
			}
		})
	}
}

// No catalog model sets supportsMaxOutputTokens, so the default request body
// must stay byte-identical to the pre-b8b873b98 wire shape. Golden captured
// from this port at 65ffeec, before the flag existed.
func TestResponsesDefaultBodyUnchangedByMaxOutputTokensFlag(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	maxTokens := 1024
	body := mustBuildResponsesParams(t, model, req,
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{MaxTokens: &maxTokens}})
	const want = `{"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],"max_output_tokens":1024,"model":"gpt-5","reasoning":{"effort":"none"},"store":false,"stream":true}`
	if diff := gotWantJSON(t, body, want); diff != "" {
		t.Fatal(diff)
	}
}

// The contract b8b873b98 introduces is "the default body MINUS exactly one
// key", not merely "max_output_tokens is absent". Asserting the FULL body under
// both flag values — with the optional params that sit immediately around the
// gate populated — pins the difference to that one key: a gate that swallows a
// neighbouring block (temperature nesting inside it) or that reaches back into
// params it has no business touching (dropping store) changes the body without
// changing whether max_output_tokens is present, and only a whole-body golden
// sees it.
func TestResponsesFullBodyUnderMaxOutputTokensFlag(t *testing.T) {
	tests := []struct {
		name   string
		compat json.RawMessage
		want   string
	}{
		{
			name:   "flag true (the catalog default)",
			compat: json.RawMessage(`{"supportsMaxOutputTokens":true}`),
			want:   `{"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],"max_output_tokens":1024,"model":"gpt-5","reasoning":{"effort":"none"},"service_tier":"priority","store":false,"stream":true,"temperature":0.5}`,
		},
		{
			name:   "flag false drops max_output_tokens and nothing else",
			compat: json.RawMessage(`{"supportsMaxOutputTokens":false}`),
			want:   `{"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],"model":"gpt-5","reasoning":{"effort":"none"},"service_tier":"priority","store":false,"stream":true,"temperature":0.5}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := reasoningModel()
			model.Compat = tc.compat
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			maxTokens := 1024
			temperature := 0.5
			body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{
				StreamOptions: ai.StreamOptions{MaxTokens: &maxTokens, Temperature: &temperature},
				ServiceTier:   "priority",
			})
			if diff := gotWantJSON(t, body, tc.want); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

// D7c: an invalid thinkingSignature on a same-model replay fails the stream
// (pi's JSON.parse throws) instead of silently dropping the block.
func TestResponsesInvalidThinkingSignatureFailsStream(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:    ai.ContentList{ai.ThinkingContent{Thinking: "deep", ThinkingSignature: "not-json"}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-5",
			StopReason: ai.StopStop,
		},
		ai.NewUserText("again", 2),
	}}
	if _, err := responsesInput(model, ai.NormalizeContext(req), ""); err == nil {
		t.Fatalf("expected responsesInput to error on invalid thinkingSignature")
	}
	final := runResponsesSSE(t, model, req, "")
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s (%q)", final.StopReason, final.ErrorMessage)
	}
	if final.ErrorMessage == "" {
		t.Fatalf("expected a parse error message")
	}
}

// D7e: a function_call output_item.done without a prior output_item.added
// still emits toolcall_end with the constructed toolCall (pi shared :481-491;
// faithfully NOT appended to content).
func TestResponsesFunctionCallDoneWithoutAdded(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_9","call_id":"call_9","name":"calc","arguments":"{\"x\":3}"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	m := *reasoningModel()
	m.BaseURL = server.URL
	stream := StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

	var end *ai.ToolCall
	var sawStart bool
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallStart {
			sawStart = true
		}
		if ev.Type == ai.EventToolCallEnd {
			end = ev.ToolCall
		}
	}
	final := stream.Result()
	if end == nil {
		t.Fatalf("expected toolcall_end for done-without-added")
	}
	if end.ID != "call_9|fc_9" || end.Name != "calc" {
		t.Fatalf("constructed toolCall wrong: %#v", end)
	}
	if v, _ := end.Arguments["x"].(float64); v != 3 {
		t.Fatalf("constructed args wrong: %#v", end.Arguments)
	}
	// pi 8c9dbffa: getOrCreateSlot now materializes the block for a
	// done-without-added, so a toolcall_start fires and the block lands in
	// content (and the stop reason promotes to toolUse).
	if !sawStart {
		t.Fatalf("expected toolcall_start for done-without-added")
	}
	var toolInContent *ai.ToolCall
	for _, c := range final.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			v := tc
			toolInContent = &v
		}
	}
	if toolInContent == nil || toolInContent.ID != "call_9|fc_9" {
		t.Fatalf("toolCall must be appended to content: %#v", final.Content)
	}
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop reason wrong: %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// D7f, revised: pi's finalizeResponse opens with
// backfillReasoningSignatures(response.output ?? []) since upstream 1f0dbc00,
// which reads off the response unguarded. A response.completed without a
// response therefore fails the stream with V8's TypeError, even after a tool
// call, rather than promoting toolUse (measured: pi at 002fc8385 ends error
// "Cannot read properties of undefined (reading 'output')" on this body).
func TestResponsesCompletedNullResponse(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":""}}

data: {"type":"response.function_call_arguments.delta","delta":"{\"x\":1}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":"{\"x\":1}"}}

data: {"type":"response.completed"}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopError || final.ErrorMessage != "Cannot read properties of undefined (reading 'output')" {
		t.Fatalf("response.completed without a response ended %s %q, pi error \"Cannot read properties of undefined (reading 'output')\"", final.StopReason, final.ErrorMessage)
	}
}

// D7h: onPayload/onResponse errors fail the stream; a non-nil onPayload return
// replaces the params wholesale.
func TestResponsesOnPayloadOnResponsePropagation(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	final := runResponsesSSEOpts(t, model, req, "", &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{OnPayload: func(payload any, m *ai.Model) (any, error) { return nil, fmt.Errorf("payload veto") }},
	}})
	if final.StopReason != ai.StopError || final.ErrorMessage != "payload veto" {
		t.Fatalf("onPayload error must fail stream: %s %q", final.StopReason, final.ErrorMessage)
	}

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	m := *model
	m.BaseURL = server.URL
	StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(req), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk", OnPayload: func(payload any, mm *ai.Model) (any, error) { return map[string]any{"replaced": true}, nil }},
	}}).
		Result()
	if gotBody == nil || gotBody["replaced"] != true || len(gotBody) != 1 {
		t.Fatalf("onPayload replacement must be wholesale: %#v", gotBody)
	}

	final3 := runResponsesSSEOpts(t, model, req, "", &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{OnResponse: func(resp ai.ProviderResponse, mm *ai.Model) error { return fmt.Errorf("response veto") }},
	}})
	if final3.StopReason != ai.StopError || final3.ErrorMessage != "response veto" {
		t.Fatalf("onResponse error must fail stream: %s %q", final3.StopReason, final3.ErrorMessage)
	}
}

// Upstream cd95c274: a response.incomplete terminal event finalizes usage and
// stop reason identically to response.completed (status "incomplete" -> length).
func TestResponsesIncompleteFinalizesUsageAndStopReason(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"partial"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"partial"}]}}

data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28,"input_tokens_details":{"cached_tokens":5}}}}

`
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true,
		Cost: ai.ModelCost{Input: 1.25, Output: 10}}
	final := runResponsesSSE(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopLength {
		t.Fatalf("incomplete should map to length stop, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if final.Usage.Input != 15 || final.Usage.CacheRead != 5 || final.Usage.Output != 8 || final.Usage.TotalTokens != 28 {
		t.Fatalf("incomplete usage not finalized: %+v", final.Usage)
	}
	if final.Usage.Cost.Total <= 0 {
		t.Fatalf("incomplete must run cost calc, got %v", final.Usage.Cost.Total)
	}
	var text string
	for _, c := range final.Content {
		if tc, ok := c.(ai.TextContent); ok {
			text = tc.Text
		}
	}
	if text != "partial" {
		t.Fatalf("text wrong: %q", text)
	}
}

// Upstream f9a49869: the streaming partial starts with a pending stop reason and
// resolves to "stop" as soon as a message item reaches the "final_answer" phase,
// exposing the terminal reason mid-stream before response.completed arrives.
func TestResponsesFinalAnswerPhaseResolvesPendingStop(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"Answer: 42"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","phase":"final_answer","content":[{"type":"output_text","text":"Answer: 42"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	model := reasoningModel()
	model.BaseURL = server.URL
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	stream := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

	var startStop, textEndStop ai.StopReason
	sawStart, sawTextEnd := false, false
	for e := range stream.Events() {
		switch e.Type {
		case ai.EventStart:
			sawStart = true
			startStop = e.Partial.StopReason
		case ai.EventTextEnd:
			sawTextEnd = true
			textEndStop = e.Partial.StopReason
		}
	}
	final := stream.Result()

	if !sawStart || startStop != ai.StopPending {
		t.Fatalf("stream should start pending, got start=%v stop=%q", sawStart, startStop)
	}
	if !sawTextEnd || textEndStop != ai.StopStop {
		t.Fatalf("final_answer phase must resolve partial to stop before terminal, got textEnd=%v stop=%q", sawTextEnd, textEndStop)
	}
	if final.StopReason != ai.StopStop {
		t.Fatalf("final stop reason wrong: %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// Upstream f9a49869: the final_answer phase also resolves the pending stop when
// it arrives on the output_item.added event (the createSlot call site), not only
// on output_item.done — the partial is stop from the very first *_start event.
func TestResponsesFinalAnswerPhaseOnAddedResolvesStop(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1","phase":"final_answer"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"hi"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","phase":"final_answer","content":[{"type":"output_text","text":"hi"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	model := reasoningModel()
	model.BaseURL = server.URL
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	stream := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

	var textStartStop ai.StopReason
	sawTextStart := false
	for e := range stream.Events() {
		if e.Type == ai.EventTextStart {
			sawTextStart = true
			textStartStop = e.Partial.StopReason
		}
	}
	final := stream.Result()

	if !sawTextStart || textStartStop != ai.StopStop {
		t.Fatalf("phase on output_item.added must resolve partial to stop at text start, got textStart=%v stop=%q", sawTextStart, textStartStop)
	}
	if final.StopReason != ai.StopStop {
		t.Fatalf("final stop reason wrong: %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// Upstream f9a49869: a provisional "stop" set by the final_answer phase must be
// overridden by the terminal event's real reason — an incomplete response still
// maps to length, not the phase's stop.
func TestResponsesIncompleteOverridesFinalAnswerStop(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"partial answer"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","phase":"final_answer","content":[{"type":"output_text","text":"partial answer"}]}}

data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28}}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopLength {
		t.Fatalf("terminal incomplete must override the provisional final_answer stop, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// Upstream cd95c274: a stream that ends without response.completed/.incomplete/
// .failed fails with this exact message.
func TestResponsesNoTerminalEventFailsStream(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","delta":"partial"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"partial"}]}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopError {
		t.Fatalf("missing terminal event should fail, got %s", final.StopReason)
	}
	if final.ErrorMessage != "OpenAI Responses stream ended before a terminal response event" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// Upstream 1b2aa0ca0 (openai-responses-terminal-event.test.ts): a completed
// stream whose tool call never received output_item.done fails instead of
// handing the agent a call it would run with cut-off or mixed-up arguments.
func TestResponsesUnfinishedToolCallFailsStream(t *testing.T) {
	cases := []struct {
		name, sse, want string
	}{
		{
			name: "no output_item.done",
			sse: `data: {"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":""}}

data: {"type":"response.function_call_arguments.delta","sequence_number":1,"output_index":0,"item_id":"fc_1","delta":"{\"command\":\"rm -rf /tmp/build"}

data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_unfinished","status":"completed"}}

`,
			want: "OpenAI Responses stream completed with an unfinished tool call: bash (call_1|fc_1)",
		},
		{
			// pi #9974: llama.cpp omits output_index from every event and sends
			// both done events after all deltas, so the second call's slot
			// replaces the first's and call_a is never finished.
			name: "parallel calls without output_index",
			sse: `data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":""}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_a","delta":"{\"command\":\"echo a\"}"}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"bash","arguments":""}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_b","delta":"{\"command\":\"echo b\"}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":"{\"command\":\"echo a\"}"}}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"bash","arguments":"{\"command\":\"echo b\"}"}}

data: {"type":"response.completed","response":{"id":"resp_no_output_index","status":"completed"}}

`,
			want: "OpenAI Responses stream completed with an unfinished tool call: bash (call_a|fc_a)",
		},
		{
			// pi walks output.content, so of several open calls the error
			// names the first in content order.
			name: "several unfinished calls",
			sse: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":""}}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":""}}

data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_3","call_id":"call_3","name":"edit","arguments":""}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":"{}"}}

data: {"type":"response.completed","response":{"id":"resp_several","status":"completed"}}

`,
			want: "OpenAI Responses stream completed with an unfinished tool call: read (call_2|fc_2)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, tc.sse)
			if final.StopReason != ai.StopError || final.ErrorMessage != tc.want {
				t.Fatalf("stream ended %s %q, want error %q", final.StopReason, final.ErrorMessage, tc.want)
			}
		})
	}
}

// pi's processResponsesStream keys its output slots by event.output_index in
// a Map, which compares keys by SameValueZero: an absent index (undefined), a
// null, a 0 and a "0" are four different slots. Collapsing the first three to
// 0 decides success and failure differently since 1b2aa0ca0 fails a stream
// with an open call. Expectations are pi's own outcomes, from running pi's
// openai-responses stream at 4259686d9 over these bodies.
func TestResponsesSlotsKeyOutputIndexAsPiDoes(t *testing.T) {
	type call struct {
		id   string
		args map[string]any
	}
	echo := func(s string) map[string]any { return map[string]any{"command": "echo " + s} }
	cases := []struct {
		name, sse   string
		stop        ai.StopReason
		errorText   string
		calls       int // tool call blocks in the final message, open ones included
		wantToolUse []call
	}{
		{
			name: "a 0 and an absent index are two slots",
			sse: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":""}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"bash","arguments":""}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":"{\"command\":\"echo a\"}"}}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"bash","arguments":"{\"command\":\"echo b\"}"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`,
			stop:        ai.StopToolUse,
			calls:       2,
			wantToolUse: []call{{"call_a|fc_a", echo("a")}, {"call_b|fc_b", echo("b")}},
		},
		{
			name: "a 0 and a null index are two slots",
			sse: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":""}}

data: {"type":"response.output_item.added","output_index":null,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"bash","arguments":""}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":"{\"command\":\"echo a\"}"}}

data: {"type":"response.output_item.done","output_index":null,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"bash","arguments":"{\"command\":\"echo b\"}"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`,
			stop:        ai.StopToolUse,
			calls:       2,
			wantToolUse: []call{{"call_a|fc_a", echo("a")}, {"call_b|fc_b", echo("b")}},
		},
		{
			// The done event without an index opens a new slot of its own and
			// finishes that, leaving the call at index 0 open.
			name: "a done without the index its call was added under",
			sse: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_a","delta":"{\"command\":\"echo a\"}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":"{\"command\":\"echo a\"}"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`,
			stop:      ai.StopError,
			errorText: "OpenAI Responses stream completed with an unfinished tool call: bash (call_a|fc_a)",
			calls:     2,
		},
		{
			// The done event is processed, not dropped: its "0" opens a slot of
			// its own.
			name: "a string index is not the number",
			sse: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":""}}

data: {"type":"response.output_item.done","output_index":"0","item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"bash","arguments":"{\"command\":\"echo a\"}"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`,
			stop:      ai.StopError,
			errorText: "OpenAI Responses stream completed with an unfinished tool call: bash (call_a|fc_a)",
			calls:     2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, tc.sse)
			if final.StopReason != tc.stop || final.ErrorMessage != tc.errorText {
				t.Fatalf("stream ended %s %q, want %s %q", final.StopReason, final.ErrorMessage, tc.stop, tc.errorText)
			}
			var got []call
			for _, c := range final.Content {
				if tcall, ok := c.(ai.ToolCall); ok {
					got = append(got, call{tcall.ID, tcall.Arguments})
				}
			}
			if len(got) != tc.calls {
				t.Fatalf("%d tool call blocks, want pi's %d: %+v", len(got), tc.calls, got)
			}
			if tc.stop == ai.StopToolUse && !reflect.DeepEqual(got, tc.wantToolUse) {
				t.Fatalf("tool calls = %+v, want %+v", got, tc.wantToolUse)
			}
		})
	}
}

// pi's backfill of a reasoning item's encrypted_content (upstream 1f0dbc00)
// rewrites the block in place, so a stream that then fails — an incomplete
// response stopped by the content filter — still carries the backfilled
// signature on its error message, as pi's does at 4259686d9.
func TestResponsesBackfilledSignatureReachesTheErrorMessage(t *testing.T) {
	sse := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"t"}]}}

data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"type":"reasoning","id":"rs_1","encrypted_content":"ENC"}]}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopError || final.ErrorMessage != "Response incomplete: content_filter" {
		t.Fatalf("stream ended %s %q, want the content-filter error", final.StopReason, final.ErrorMessage)
	}
	want := `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"t"}],"encrypted_content":"ENC"}`
	for _, c := range final.Content {
		if th, ok := c.(ai.ThinkingContent); ok {
			if th.ThinkingSignature != want {
				t.Fatalf("thinking signature = %s, want %s", th.ThinkingSignature, want)
			}
			return
		}
	}
	t.Fatal("no thinking block on the error message")
}

// Upstream openai-responses-terminal-event.test.ts (002fc8385), "forwards
// parsed provider stream events in order": each event reaches the observer
// with the stream's model, in stream order — response.reasoning_text.delta
// included, and before the missing terminal event fails the stream.
func TestResponsesForwardsProviderStreamEventsInOrder(t *testing.T) {
	sse := `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_wrapper_early_eof"}}

data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_wrapper_early_eof","summary":[]}}

data: {"type":"response.reasoning_text.delta","sequence_number":2,"output_index":0,"content_index":0,"item_id":"rs_wrapper_early_eof","delta":"partial reasoning before the wrapper stream ends"}

`
	model := &ai.Model{
		ID: "gpt-5-mini", Name: "GPT-5 Mini", Api: ai.APIOpenAIResponses, Provider: "openai",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 400000, MaxTokens: 128000,
	}
	var types []string
	var models []*ai.Model
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 0)}}),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test"},
			OnProviderStreamEvent: func(event any, eventModel *ai.Model) error {
				object, _ := event.(ai.OrderedObject)
				eventType, _ := object.Plain()["type"].(string)
				types = append(types, eventType)
				models = append(models, eventModel)
				return nil
			},
		}}).Result()

	want := []string{"response.created", "response.output_item.added", "response.reasoning_text.delta"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %q, want %q", types, want)
	}
	for i, m := range models {
		if m != model {
			t.Fatalf("event %d observed with model %p, want the stream's %p", i, m, model)
		}
	}
	if final.ErrorMessage != "OpenAI Responses stream ended before a terminal response event" {
		t.Fatalf("stream ended %s %q", final.StopReason, final.ErrorMessage)
	}
}

// C5 (responses half): prompt_cache_retention is independent of sessionId;
// prompt_cache_key still requires one.
func TestResponsesCacheRetentionWithoutSessionID(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{CacheRetention: ai.CacheLong}})
	if body["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention must be sent without sessionId, got %v", body["prompt_cache_retention"])
	}
	if _, has := body["prompt_cache_key"]; has {
		t.Fatalf("prompt_cache_key requires a sessionId")
	}
}

// Upstream 17de82d7b coupled the two prompt-cache params, so they are pinned
// together. `supportsExplicitPromptCacheMode` (GPT-5.6+) now OWNS long
// retention: such a model expresses it as `prompt_cache_options: {ttl: "30m"}`
// and must NOT also send `prompt_cache_retention: "24h"`, which older models
// still use. `{mode: "explicit"}` (upstream 241431c6) is unchanged — retention
// "none" on an explicit-cache model, so write compaction and branch summaries
// do not poison the session cache. supportsLongCacheRetention defaults to true.
func TestResponsesPromptCacheParams(t *testing.T) {
	explicit := json.RawMessage(`{"supportsExplicitPromptCacheMode":true}`)
	explicitNoLong := json.RawMessage(`{"supportsExplicitPromptCacheMode":true,"supportsLongCacheRetention":false}`)
	noLong := json.RawMessage(`{"supportsLongCacheRetention":false}`)
	tests := []struct {
		name          string
		compat        json.RawMessage
		retention     ai.CacheRetention
		wantRetention string // "" = param absent
		wantOptions   string // "" = param absent
	}{
		{"default compat, none", nil, ai.CacheNone, "", ""},
		{"default compat, short", nil, "", "", ""},
		{"default compat, long", nil, ai.CacheLong, `"24h"`, ""},
		{"explicit, none", explicit, ai.CacheNone, "", `{"mode":"explicit"}`},
		{"explicit, short", explicit, "", "", ""},
		{"explicit, long", explicit, ai.CacheLong, "", `{"ttl":"30m"}`},
		{"explicit without long support, long", explicitNoLong, ai.CacheLong, "", ""},
		{"explicit without long support, none", explicitNoLong, ai.CacheNone, "", `{"mode":"explicit"}`},
		{"no long support, long", noLong, ai.CacheLong, "", ""},
		{"explicit false, none", json.RawMessage(`{"supportsExplicitPromptCacheMode":false}`), ai.CacheNone, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := reasoningModel()
			model.Compat = tc.compat
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			body := mustBuildResponsesParams(t, model, req,
				&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1", CacheRetention: tc.retention}})

			check := func(param, want string) {
				t.Helper()
				got, has := body[param]
				if want == "" {
					if has {
						t.Fatalf("%s must be absent, got %#v", param, got)
					}
					return
				}
				if !has {
					t.Fatalf("%s missing, want %s", param, want)
				}
				if diff := gotWantJSON(t, got, want); diff != "" {
					t.Fatal(diff)
				}
			}
			check("prompt_cache_retention", tc.wantRetention)
			check("prompt_cache_options", tc.wantOptions)
		})
	}
}

// The same split, driven by the REAL catalog rather than hand-built compat —
// upstream's cache-retention test pins exactly this pair (17de82d7b). gpt-4o-mini
// carries no explicit-cache compat and keeps "24h"; gpt-6-astra arrived with the
// 0.85.1 regen carrying supportsExplicitPromptCacheMode and takes ttl "30m";
// db91e0331 extends upstream's table with gpt-6-sol and gpt-6-luna, which
// arrived with the 0.87.1 regen. A regen that changes any of these models'
// compat fails here.
func TestResponsesPromptCacheParamsFromCatalog(t *testing.T) {
	tests := []struct {
		modelID       string
		wantRetention string
		wantOptions   string
	}{
		{"gpt-4o-mini", `"24h"`, ""},
		{"gpt-6-astra", "", `{"ttl":"30m"}`},
		{"gpt-6-sol", "", `{"ttl":"30m"}`},
		{"gpt-6-luna", "", `{"ttl":"30m"}`},
	}
	for _, tc := range tests {
		t.Run(tc.modelID, func(t *testing.T) {
			model := ai.GetModel("openai", tc.modelID)
			if model == nil {
				t.Fatalf("catalog has no openai/%s", tc.modelID)
			}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{
				StreamOptions: ai.StreamOptions{SessionID: "session-2", CacheRetention: ai.CacheLong},
			})
			if got := body["prompt_cache_key"]; got != "session-2" {
				t.Errorf("prompt_cache_key = %#v, want session-2", got)
			}
			for _, p := range []struct{ param, want string }{
				{"prompt_cache_retention", tc.wantRetention},
				{"prompt_cache_options", tc.wantOptions},
			} {
				got, has := body[p.param]
				if p.want == "" {
					if has {
						t.Errorf("%s must be absent, got %#v", p.param, got)
					}
					continue
				}
				if !has {
					t.Errorf("%s missing, want %s", p.param, p.want)
					continue
				}
				if diff := gotWantJSON(t, got, p.want); diff != "" {
					t.Error(diff)
				}
			}
		})
	}
}

// Mirrors openai-responses-compat.test.ts "sends none reasoning effort for
// OpenAI %s when no reasoning is requested" (db91e0331 adds gpt-6-sol and
// gpt-6-luna) and its sibling "omits reasoning effort for OpenAI %s when off is
// unsupported", driven by the real catalog. With no reasoning requested, pi
// sends reasoning {effort: thinkingLevelMap.off ?? "none"} unless off is null,
// so each row's wire value comes from its catalog off entry. A regen that moves
// one of these off entries fails here.
func TestResponsesReasoningOffFromCatalog(t *testing.T) {
	req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	for _, id := range []string{
		"gpt-5.1", "gpt-5.2", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
		"gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna",
	} {
		t.Run("sends none/"+id, func(t *testing.T) {
			model := ai.GetModel("openai", id)
			if model == nil {
				t.Fatalf("catalog has no openai/%s", id)
			}
			got, has := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})["reasoning"]
			if !has {
				t.Fatal(`reasoning missing, want {"effort":"none"}`)
			}
			if diff := gotWantJSON(t, got, `{"effort":"none"}`); diff != "" {
				t.Error(diff)
			}
		})
	}
	for _, id := range []string{"gpt-5", "gpt-5-mini", "gpt-5-nano", "gpt-5-pro", "gpt-5.2-pro", "gpt-5.4-pro", "gpt-5.5-pro"} {
		t.Run("omits/"+id, func(t *testing.T) {
			model := ai.GetModel("openai", id)
			if model == nil {
				t.Fatalf("catalog has no openai/%s", id)
			}
			if got, has := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})["reasoning"]; has {
				t.Errorf("reasoning = %#v, want absent (off is null)", got)
			}
		})
	}
}

// gotWantJSON renders got as JSON and compares it byte-for-byte with want.
func gotWantJSON(t *testing.T, got any, want string) string {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) == want {
		return ""
	}
	return "got " + string(b) + ", want " + want
}

// Regression for #6009 (upstream 8c9dbffa): when output items interleave, a
// delta for an earlier item that arrives AFTER a later item's block was started
// must route to the earlier item's block by output_index, not to whatever block
// was appended last. Here the reasoning item (output_index 0) gets a
// reasoning_text.delta only after the message item (output_index 1) has opened
// and received a text delta — the thinking must land on content[0], the text on
// content[1].
func TestResponsesOutOfOrderItemsPreserveReasoning(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"Answer: "}

data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"thinking hard"}

data: {"type":"response.output_text.delta","output_index":1,"delta":"42"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"thinking hard"}]}}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"Answer: 42"}]}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	// Capture the stream events so we can assert the interleaved reasoning delta
	// targeted content[0] (the reasoning block), not content[1].
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	m := *reasoningModel()
	m.BaseURL = server.URL
	stream := StreamOpenAIResponses(context.Background(), &m, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

	var thinkingDeltaIndex = -1
	for ev := range stream.Events() {
		if ev.Type == ai.EventThinkingDelta && ev.Delta == "thinking hard" {
			thinkingDeltaIndex = ev.ContentIndex
		}
	}
	final := stream.Result()

	// The interleaved thinking delta must have been emitted against content[0].
	if thinkingDeltaIndex != 0 {
		t.Fatalf("thinking delta routed to contentIndex %d, want 0", thinkingDeltaIndex)
	}

	if len(final.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d: %#v", len(final.Content), final.Content)
	}
	think, ok := final.Content[0].(ai.ThinkingContent)
	if !ok {
		t.Fatalf("content[0] not thinking: %#v", final.Content[0])
	}
	if think.Thinking != "thinking hard" {
		t.Fatalf("reasoning landed on wrong block: %q", think.Thinking)
	}
	text, ok := final.Content[1].(ai.TextContent)
	if !ok {
		t.Fatalf("content[1] not text: %#v", final.Content[1])
	}
	if text.Text != "Answer: 42" {
		t.Fatalf("text landed on wrong block: %q", text.Text)
	}
}

// pi #6290 (upstream 279f53b0): an empty tool result with no image content must
// send "(no tool output)" as the function_call_output, not "(see attached image)".
func TestResponsesEmptyToolResultNoImagePlaceholder(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("run it", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1|fc_1", Name: "bash", Arguments: map[string]any{"command": "true"}}},
			Api:     ai.APIOpenAIResponses, Provider: "openai", Model: "gpt-5", StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: ""}}, Timestamp: 2},
	}}
	in := mustResponsesInput(t, model, req)
	found := false
	for _, it := range in {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call_output" {
			found = true
			if m["output"] != "(no tool output)" {
				t.Fatalf("empty no-image output = %#v, want %q", m["output"], "(no tool output)")
			}
		}
	}
	if !found {
		t.Fatalf("no function_call_output emitted: %#v", in)
	}
}

// Mirrors pi openai-responses-terminal-event.test.ts (upstream a9ecf301):
// input_tokens includes both cached and cache-write tokens, so both are
// subtracted (clamped at 0) to get non-cached input, and cache_write_tokens
// surfaces as cacheWrite.
func TestOpenAIResponsesUsageCacheWrite(t *testing.T) {
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_completed\",\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":20,\"output_tokens\":7,\"total_tokens\":27," +
		"\"input_tokens_details\":{\"cached_tokens\":2,\"cache_write_tokens\":3}}}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: server.URL,
		MaxTokens: 4096, Cost: ai.ModelCost{Input: 1.25, Output: 10},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	final := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}).Result()

	if final.Usage.Input != 15 || final.Usage.CacheRead != 2 || final.Usage.CacheWrite != 3 ||
		final.Usage.Output != 7 || final.Usage.TotalTokens != 27 {
		t.Fatalf("usage wrong: %+v (want Input 15, CacheRead 2, CacheWrite 3, Output 7, Total 27)", final.Usage)
	}
}

// TestResponsesXaiEncryptedReasoningInclude locks pi 5220aba6: every
// reasoning-capable xai model requests include=["reasoning.encrypted_content"]
// regardless of which reasoning branch fired — including the grok-4.5 shape
// (thinkingLevelMap.off null, no effort → no reasoning object, include still
// sent) — while non-xai models without effort stay include-free.
func TestResponsesXaiEncryptedReasoningInclude(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	// grok-4.5 shape: reasoning true, off:null map, no effort requested.
	grok := &ai.Model{
		ID: "grok-4.5", Api: ai.APIOpenAIResponses, Provider: "xai",
		BaseURL: "https://api.x.ai/v1", Reasoning: true,
		ThinkingLevelMap: map[ai.ModelThinkingLevel]*string{"off": nil, "minimal": nil},
		Input:            []string{"text"}, MaxTokens: 4096,
	}
	params := mustBuildResponsesParams(t, grok, req, &OpenAIResponsesOptions{})
	include, ok := params["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("xai include = %v, want [reasoning.encrypted_content]", params["include"])
	}
	if _, hasReasoning := params["reasoning"]; hasReasoning {
		t.Fatalf("off:null with no effort must not send reasoning: %v", params["reasoning"])
	}

	// With an effort both the reasoning object and include are present.
	params = mustBuildResponsesParams(t, grok, req, &OpenAIResponsesOptions{ReasoningEffort: "high"})
	if _, ok := params["include"].([]any); !ok {
		t.Fatalf("xai include missing with effort: %v", params["include"])
	}

	// Non-xai reasoning model without effort: no include.
	other := &ai.Model{
		ID: "some-model", Api: ai.APIOpenAIResponses, Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Reasoning: true,
		Input: []string{"text"}, MaxTokens: 4096,
	}
	params = mustBuildResponsesParams(t, other, req, &OpenAIResponsesOptions{})
	if _, has := params["include"]; has {
		t.Fatalf("non-xai without effort must not send include: %v", params["include"])
	}

	// Non-reasoning xai model: no include (the branch is inside model.reasoning).
	flat := &ai.Model{
		ID: "grok-build-0.1", Api: ai.APIOpenAIResponses, Provider: "xai",
		BaseURL: "https://api.x.ai/v1", Reasoning: false,
		Input: []string{"text"}, MaxTokens: 4096,
	}
	params = mustBuildResponsesParams(t, flat, req, &OpenAIResponsesOptions{})
	if _, has := params["include"]; has {
		t.Fatalf("non-reasoning xai must not send include: %v", params["include"])
	}
}

// ---- Upstream 24bace27: constrained sampling (responses) ----

func grammarResponsesModel(compat string) *ai.Model {
	m := reasoningModel()
	m.Compat = json.RawMessage(compat)
	return m
}

// pi 0.82.0 tools payload for a lark grammar tool on openai-responses:
//
//	[{"type":"custom","name":"gram","description":"gram tool",
//	  "format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]
func TestResponsesGrammarToolSerialization(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}
	body := mustBuildResponsesParams(t, grammarResponsesModel(`{"supportsOpenAIGrammarTools":true}`), req, &OpenAIResponsesOptions{})
	if got := mustJSON(t, body["tools"]); got != `[{"description":"gram tool","format":{"definition":"start: /.+/","syntax":"lark","type":"grammar"},"name":"gram","type":"custom"}]` {
		t.Fatalf("grammar tool payload: %s", got)
	}

	// Without grammar support it degrades to a function tool (and, since this
	// model does not set supportsStrictMode, carries no strict key).
	plain := mustBuildResponsesParams(t, reasoningModel(), req, &OpenAIResponsesOptions{})
	tools, _ := plain["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["type"] != "function" {
		t.Fatalf("grammar tool must fall back to a function tool: %#v", plain["tools"])
	}
}

// pi 0.82.0 replay of a grammar tool call + its result:
//
//	{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_abc","name":"gram","input":"SELECT 1"}
//	{"type":"custom_tool_call_output","call_id":"ctc_abc","output":"ok"}
func TestResponsesGrammarToolCallReplay(t *testing.T) {
	model := grammarResponsesModel(`{"supportsOpenAIGrammarTools":true}`)
	req := ai.Context{
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{
				Content:  ai.ContentList{ai.ToolCall{ID: "ctc_abc|ctc_item", Name: "gram", Arguments: map[string]any{"query": "SELECT 1"}}},
				Model:    model.ID,
				Provider: model.Provider,
				Api:      model.Api,
			},
			ai.ToolResultMessage{ToolCallID: "ctc_abc|ctc_item", ToolName: "gram", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
		},
		Tools: []ai.Tool{grammarSamplingTool()},
	}
	in := mustResponsesInput(t, model, req)
	call := findResponsesItem(in, "custom_tool_call")
	if got := mustJSON(t, call); got != `{"call_id":"ctc_abc","id":"ctc_item","input":"SELECT 1","name":"gram","type":"custom_tool_call"}` {
		t.Fatalf("custom_tool_call: %s", got)
	}
	out := findResponsesItem(in, "custom_tool_call_output")
	if got := mustJSON(t, out); got != `{"call_id":"ctc_abc","output":"ok","type":"custom_tool_call_output"}` {
		t.Fatalf("custom_tool_call_output: %s", got)
	}
}

// Upstream 24bace27 also changed the function_call replay path: an item id that
// is not fc_* cannot ride on a function_call item and is now dropped. pi 0.82.0
// with grammar support OFF replays the same transcript as
// {"type":"function_call","call_id":"ctc_abc",…} — no "id" — while an fc_* id
// still survives.
func TestResponsesFunctionCallReplayDropsNonFCItemID(t *testing.T) {
	model := reasoningModel()
	transcript := func(id string) ai.Context {
		return ai.Context{
			Messages: []ai.Message{
				ai.NewUserText("hi", 1),
				ai.AssistantMessage{
					Content:  ai.ContentList{ai.ToolCall{ID: id, Name: "gram", Arguments: map[string]any{"query": "SELECT 1"}}},
					Model:    model.ID,
					Provider: model.Provider,
					Api:      model.Api,
				},
				ai.ToolResultMessage{ToolCallID: id, ToolName: "gram", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
			},
			Tools: []ai.Tool{grammarSamplingTool()},
		}
	}

	call := findResponsesItem(mustResponsesInput(t, model, transcript("ctc_abc|ctc_item")), "function_call")
	if _, has := call["id"]; has {
		t.Fatalf("a non-fc_ item id must be dropped from a function_call: %#v", call)
	}
	if call["call_id"] != "ctc_abc" {
		t.Fatalf("call_id must survive: %#v", call)
	}

	call = findResponsesItem(mustResponsesInput(t, model, transcript("fc_abc|fc_item")), "function_call")
	if call["id"] != "fc_item" {
		t.Fatalf("an fc_ item id must survive: %#v", call)
	}
}

// Event sequence captured from pi 0.82.0 driving the same SSE.
func TestResponsesGrammarToolCallStreaming(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r1"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":""}}

data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"SEL"}

data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"ECT \"a\""}

data: {"type":"response.custom_tool_call_input.done","output_index":0,"input":"SELECT \"a\""}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"SELECT \"a\""}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}

data: [DONE]

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := grammarResponsesModel(`{"supportsOpenAIGrammarTools":true}`)
	model.BaseURL = server.URL
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}
	stream := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

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
	if !ok || tc.ID != "ctc_call|ctc_item" || tc.Name != "gram" || tc.Arguments["query"] != `SELECT "a"` {
		t.Fatalf("final tool call = %#v", final.Content)
	}
}

// TestResponsesGrammarInitialInput: output_item.added may carry a non-empty
// `input`. pi 8b5899dce reverted 5c6655e76 and put that input back ON the block
// the toolcall_start event announces (`arguments: { [inputProperty]: input }`),
// deleting the follow-up replay delta. Only the JSON delta buffer stays empty,
// so the first streamed chunk after the start opens the synthesized object and
// carries the initial input along with it.
func TestResponsesGrammarInitialInput(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r1"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"SEL"}}

data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"ECT 1"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"SELECT 1"}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}

data: [DONE]

`
	deltas, final := runResponsesGrammarSSE(t, sse)
	assertDeltas(t, deltas, []string{`{"query":"SELECT 1`, `"}`})
	assertGrammarArgs(t, final, "query", "SELECT 1")
}

// TestResponsesGrammarDoneWithoutInput: output_item.done may omit `input`
// entirely, where pi's `item.input ?? …` falls back to the input accumulated so
// far. An absent field is NOT the same as an empty string, which is why
// responsesItem.Input is a *string.
//
// Expectation captured from pi 0.82.0 the same way: ["{\"query\":\"SELECT 2", "\"}"].
func TestResponsesGrammarDoneWithoutInput(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r1"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":""}}

data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"SELECT 2"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram"}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}

data: [DONE]

`
	deltas, final := runResponsesGrammarSSE(t, sse)
	assertDeltas(t, deltas, []string{`{"query":"SELECT 2`, `"}`})
	assertGrammarArgs(t, final, "query", "SELECT 2")
}

// TestResponsesGrammarInitialInputOnStartEvent pins what pi 8b5899dce restored:
// the input carried on output_item.added is folded INTO the start event's
// arguments, and no delta replays it afterwards. The start event must therefore
// show `{"query":"SEL"}`, and the only delta on this stream is the one the
// terminal output_item.done synthesizes — which, because the JSON buffer was
// never advanced by the initial input, opens and closes the object in one go.
func TestResponsesGrammarInitialInputOnStartEvent(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r1"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"SEL"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"SEL"}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}

data: [DONE]

`
	assertToolCallTrace(t, grammarToolCallTrace(t, sse), []grammarToolCallEvent{
		{typ: ai.EventToolCallStart, index: 0, blocks: `[{"query":"SEL"}]`},
		{typ: ai.EventToolCallDelta, index: 0, delta: `{"query":"SEL"}`, blocks: `[{"query":"SEL"}]`},
	})
}

// TestResponsesGrammarMissingInputStartsEmpty pins pi's `item.input || ""`
// (shared:506): the input property is present on the start event's arguments
// even when the item carries no input at all, and an absent `input` field is
// indistinguishable from `""` here. The old seed replay's truthiness guard
// (5c6655e76) is gone with it, so there is no delta after any of these starts.
func TestResponsesGrammarMissingInputStartsEmpty(t *testing.T) {
	item := func(outputIndex int, id, input string) string {
		return fmt.Sprintf(
			`data: {"type":"response.output_item.added","output_index":%d,"item":{"type":"custom_tool_call","id":%q,"call_id":"c_%s","name":"gram"%s}}`,
			outputIndex, id, id, input)
	}
	sse := `data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n" +
		item(0, "absent", ``) + "\n\n" +
		item(1, "empty", `,"input":""`) + "\n\n" +
		item(2, "seeded", `,"input":"SEL"`) + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"data: [DONE]\n\n"

	// The third start is the observation point: its partial carries all three
	// blocks, and the two inputless ones must read `{"query":""}`.
	assertToolCallTrace(t, grammarToolCallTrace(t, sse), []grammarToolCallEvent{
		{typ: ai.EventToolCallStart, index: 0, blocks: `[{"query":""}]`},
		{typ: ai.EventToolCallStart, index: 1, blocks: `[{"query":""},{"query":""}]`},
		{typ: ai.EventToolCallStart, index: 2, blocks: `[{"query":""},{"query":""},{"query":"SEL"}]`},
	})
}

// TestResponsesGrammarInitialInputThenDeltas mirrors pi's own oracle for
// 8b5899dce — constrained-sampling.test.ts "starts custom Responses tool calls
// with their initial input": an item arriving with input "a" followed by a
// delta "b" and a done of "abc" must start at {property: "a"}, end at
// {property: "abc"}, and still emit deltas that concatenate into that same
// object. The initial input is on the start event AND inside the first delta,
// because the delta buffer opens only when the first delta is synthesized.
func TestResponsesGrammarInitialInputThenDeltas(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r1"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"a"}}

data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"b"}

data: {"type":"response.custom_tool_call_input.done","output_index":0,"input":"abc"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_item","call_id":"ctc_call","name":"gram","input":"abc"}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}

data: [DONE]

`
	trace := grammarToolCallTrace(t, sse)
	if len(trace) == 0 || trace[0].typ != ai.EventToolCallStart || trace[0].blocks != `[{"query":"a"}]` {
		t.Fatalf("first tool call event = %+v, want a start carrying {\"query\":\"a\"}", trace)
	}
	var joined strings.Builder
	for _, ev := range trace[1:] {
		if ev.typ != ai.EventToolCallDelta {
			t.Fatalf("event after the start = %+v, want a delta", ev)
		}
		joined.WriteString(ev.delta)
	}
	if got := joined.String(); got != `{"query":"abc"}` {
		t.Fatalf("joined deltas = %s, want {\"query\":\"abc\"}", got)
	}

	final := runResponsesGrammarStream(t, sse).Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop reason = %s, want toolUse", final.StopReason)
	}
	tc, ok := final.Content[0].(ai.ToolCall)
	if !ok || tc.ID != "ctc_call|ctc_item" || tc.Name != "gram" || mustJSON(t, tc.Arguments) != `{"query":"abc"}` {
		t.Fatalf("final tool call = %#v", final.Content)
	}
}

// The Responses half of pi 8b5899dce's restored eager `getClientApiKey` is
// covered by the apis x scenarios table in
// TestOpenAIStreamSimpleMissingKeySurfacesOnTheStream (openai_test.go), whose
// {openai-responses, "preempts a grammar tool failure"} cell asserts this exact
// precedence against the same control. Upstream has one oracle here
// (8b5899dce:packages/ai/test/pre-generation-error.test.ts), so this file keeps
// none of its own.

// grammarToolCallEvent is one tool-call event of a grammar stream: its type and
// content index, its delta, and the arguments of EVERY tool-call block in the
// partial it carried — the arguments of a block other than the event's own are
// how a stray write between two events becomes observable.
type grammarToolCallEvent struct {
	typ    ai.EventType
	index  int
	delta  string
	blocks string
}

// grammarToolCallTrace streams sse and records every tool-call event with the
// state of the whole partial it was pushed with.
func grammarToolCallTrace(t *testing.T, sse string) []grammarToolCallEvent {
	t.Helper()
	var trace []grammarToolCallEvent
	for ev := range runResponsesGrammarStream(t, sse).Events() {
		switch ev.Type {
		case ai.EventToolCallStart, ai.EventToolCallDelta:
			args := []map[string]any{}
			for i, block := range ev.Partial.Content {
				tc, ok := block.(ai.ToolCall)
				if !ok {
					t.Fatalf("%s block %d = %#v", ev.Type, i, block)
				}
				args = append(args, tc.Arguments)
			}
			trace = append(trace, grammarToolCallEvent{
				typ: ev.Type, index: ev.ContentIndex, delta: ev.Delta, blocks: mustJSON(t, args),
			})
		}
	}
	return trace
}

func assertToolCallTrace(t *testing.T, got, want []grammarToolCallEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tool call events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool call event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// runResponsesGrammarStream streams sse through the responses adapter against a
// model with grammar tools enabled, and returns the live event stream.
func runResponsesGrammarStream(t *testing.T, sse string) *ai.AssistantMessageEventStream {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)

	model := grammarResponsesModel(`{"supportsOpenAIGrammarTools":true}`)
	model.BaseURL = server.URL
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}
	return StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(req),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}})

}

func runResponsesGrammarSSE(t *testing.T, sse string) ([]string, *ai.AssistantMessage) {
	t.Helper()
	stream := runResponsesGrammarStream(t, sse)

	var deltas []string
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	return deltas, stream.Result()
}

func assertDeltas(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("deltas = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func assertGrammarArgs(t *testing.T, final *ai.AssistantMessage, property, want string) {
	t.Helper()
	for _, c := range final.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			if got, _ := tc.Arguments[property].(string); got != want {
				t.Fatalf("arguments[%q] = %q, want %q", property, got, want)
			}
			return
		}
	}
	t.Fatalf("no tool call in final message (stop=%s err=%s)", final.StopReason, final.ErrorMessage)
}

// pi 02bd2d1c6: namespaces ride on function_call / custom_tool_call items and
// are echoed back on replay, but only for a call the current model could have
// made itself — its own call, or a deferred tool this request is loading.
func TestResponsesNamespaceParsedFromStream(t *testing.T) {
	const fnAdded = `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":""%s}}`
	const fnDone = `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":"{\"x\":7}"%s}}`
	const ctcAdded = `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"ctc_call","name":"gram","input":""%s}}`
	const ctcDone = `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"ctc_call","name":"gram","input":"SELECT \"a\""%s}}`
	const ns = `,"namespace":"mcp_math"`

	body := func(added, done string) string {
		return "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
			added + "\n\n" + done + "\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	}

	fnModel := reasoningModel()
	fnReq := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools:    []ai.Tool{{Name: "calc", Description: "calc", Parameters: ai.Object(ai.Prop("x", ai.Integer()))}},
	}
	grammarModel := grammarResponsesModel(`{"supportsOpenAIGrammarTools":true}`)
	grammarReq := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{grammarSamplingTool()}}

	for _, tc := range []struct {
		name  string
		model *ai.Model
		req   ai.Context
		sse   string
	}{
		{"function_call, both items", fnModel, fnReq, body(fmt.Sprintf(fnAdded, ns), fmt.Sprintf(fnDone, ns))},
		// Only the done item carries it: this is what the != "" guards are for.
		{"function_call, done only", fnModel, fnReq, body(fmt.Sprintf(fnAdded, ""), fmt.Sprintf(fnDone, ns))},
		{"function_call, added only", fnModel, fnReq, body(fmt.Sprintf(fnAdded, ns), fmt.Sprintf(fnDone, ""))},
		{"custom_tool_call, both items", grammarModel, grammarReq, body(fmt.Sprintf(ctcAdded, ns), fmt.Sprintf(ctcDone, ns))},
		{"custom_tool_call, done only", grammarModel, grammarReq, body(fmt.Sprintf(ctcAdded, ""), fmt.Sprintf(ctcDone, ns))},
		{"custom_tool_call, added only", grammarModel, grammarReq, body(fmt.Sprintf(ctcAdded, ns), fmt.Sprintf(ctcDone, ""))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final := runResponsesSSE(t, tc.model, tc.req, tc.sse)
			var tool *ai.ToolCall
			for _, c := range final.Content {
				if call, ok := c.(ai.ToolCall); ok {
					tool = &call
				}
			}
			if tool == nil {
				t.Fatalf("no tool call (stop=%s err=%s)", final.StopReason, final.ErrorMessage)
			}
			if tool.Namespace != "mcp_math" {
				t.Fatalf("namespace = %q, want mcp_math", tool.Namespace)
			}
		})
	}
}

// The model's own call replays with its namespace intact.
func TestResponsesNamespaceReplayedForSameModel(t *testing.T) {
	model := reasoningModel() // gpt-5 / openai / openai-responses
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{
				ID: "call_1|fc_1", Name: "calc", Arguments: map[string]any{}, Namespace: "mcp_math",
			}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-5",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	fc := findResponsesItem(mustResponsesInput(t, model, req), "function_call")
	if fc == nil {
		t.Fatalf("no function_call item")
	}
	if fc["namespace"] != "mcp_math" {
		t.Fatalf("namespace = %v, want mcp_math", fc["namespace"])
	}
}

// A different model's call cannot claim this model's namespace.
func TestResponsesNamespaceDroppedForDifferentModel(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{
				ID: "call_1|fc_1", Name: "calc", Arguments: map[string]any{}, Namespace: "mcp_math",
			}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-4.1", // different model id, same provider/api
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	fc := findResponsesItem(mustResponsesInput(t, model, req), "function_call")
	if fc == nil {
		t.Fatalf("no function_call item")
	}
	if ns, has := fc["namespace"]; has {
		t.Fatalf("cross-model namespace should be dropped, got %v", ns)
	}
}

// A call from another provider or another api is not this model's call either,
// so its namespace cannot be replayed (pi gates all three on provider+api+id).
func TestResponsesNamespaceDroppedAcrossProviderAndApi(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		api      string
	}{
		{"different provider", "azure", ai.APIOpenAIResponses},
		{"different api", "openai", ai.APIOpenAICodexResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := reasoningModel()
			req := ai.Context{Messages: []ai.Message{
				ai.NewUserText("hi", 1),
				ai.AssistantMessage{
					Content: ai.ContentList{ai.ToolCall{
						ID: "call_1|fc_1", Name: "calc", Arguments: map[string]any{}, Namespace: "mcp_math",
					}},
					Api:        tc.api,
					Provider:   tc.provider,
					Model:      "gpt-5", // same model id, so only provider/api can rule it out
					StopReason: ai.StopToolUse,
				},
				ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
			}}
			fc := findResponsesItem(mustResponsesInput(t, model, req), "function_call")
			if fc == nil {
				t.Fatalf("no function_call item")
			}
			if ns, has := fc["namespace"]; has {
				t.Fatalf("namespace must not cross provider/api, got %v", ns)
			}
		})
	}
}

// DELIBERATE DIVERGENCE, pinned so it cannot drift silently: pi guards on
// `namespace !== undefined`, so a provider-sent empty string replays there as
// `"namespace": ""`. Go models the field as a plain string (the ThoughtSignature
// precedent) and drops it. Recorded in docs/UPSTREAM.md -> "Divergences" as D21.
func TestResponsesEmptyNamespaceDroppedDivergence(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{
				ID: "call_1|fc_1", Name: "calc", Arguments: map[string]any{}, Namespace: "",
			}},
			Api:        ai.APIOpenAIResponses,
			Provider:   "openai",
			Model:      "gpt-5",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "calc", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 2},
	}}
	fc := findResponsesItem(mustResponsesInput(t, model, req), "function_call")
	if fc == nil {
		t.Fatalf("no function_call item")
	}
	if ns, has := fc["namespace"]; has {
		t.Fatalf("pi would emit \"\" here and we deliberately do not; got %v", ns)
	}
}

// TestResponsesStopReasonWithoutErrorMessageOmitsTheField pins pi 1a7bc80e7:
// when the mapped stop reason carries no error message, upstream now DELETES
// output.errorMessage instead of assigning undefined, because the field is
// observable in serialized session JSON. Go's omitempty on the cleared string
// is the same wire shape, so this pins behavior the port already had.
func TestResponsesStopReasonWithoutErrorMessageOmitsTheField(t *testing.T) {
	const usage = `"usage":{"input_tokens":1,"output_tokens":1}`
	completed := `data: {"type":"response.completed","response":{"id":"r1","status":"completed",` + usage + `}}`
	tests := []struct {
		name string
		// terminal is the terminal event (or events) the stream ends with.
		terminal string
		wantStop ai.StopReason
	}{
		{
			name:     "completed",
			terminal: completed,
			wantStop: ai.StopStop,
		},
		{
			// A length stop is the case that shows the difference: it is a
			// non-stop reason that still has nothing to say about an error.
			name:     "incomplete on max_output_tokens",
			terminal: `data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},` + usage + `}}`,
			wantStop: ai.StopLength,
		},
		{
			// The one input where upstream's `delete output.errorMessage` and
			// the assignment it replaced part company: a message that was
			// already set. Only a second terminal event reaches it — the
			// finalize block runs once per terminal event and neither pi nor
			// the port stops after the first (shared:743) — so a content-filtered
			// incomplete followed by a completed must end with the message
			// CLEARED, not carried into a successful done.
			name: "a later terminal event clears an earlier error message",
			terminal: `data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"},` + usage + `}}` +
				"\n\n" + completed,
			wantStop: ai.StopStop,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n" +
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
				tt.terminal + "\n\n" +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, sse)
			}))
			defer server.Close()

			model := reasoningModel()
			model.BaseURL = server.URL
			final := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(
				ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

				&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}).
				Result()

			if final.StopReason != tt.wantStop {
				t.Fatalf("stopReason = %s (err %q), want %s", final.StopReason, final.ErrorMessage, tt.wantStop)
			}
			encoded, err := json.Marshal(final)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if raw, present := fields["errorMessage"]; present {
				t.Fatalf("errorMessage must be absent, got %s", raw)
			}
		})
	}
}

// TestCopilotGPTModelsUseResponsesApi is the tripwire the ledger asked for.
// pi 7d8ab31a4 widened github-copilot's needsResponsesApi from a `gpt-5` prefix
// to `gpt-`, and the 0.86.1 regen carried it: github-copilot/gpt-6-astra moved
// from openai-completions to openai-responses and so began executing this
// file's copilot-specific branches for the first time. Nothing pinned any
// copilot model's api before, so a later regen could have flipped one back with
// no test going red. Derived from the catalog rather than listing ids, so a new
// gpt- model is covered the day it appears — and the precondition is asserted
// so the loop cannot go quietly vacuous.
func TestCopilotGPTModelsUseResponsesApi(t *testing.T) {
	models := ai.GetModels("github-copilot")
	if len(models) == 0 {
		t.Fatal("the catalog has no github-copilot models; this test would prove nothing")
	}
	seen := 0
	for _, m := range models {
		if !strings.HasPrefix(m.ID, "gpt-") {
			continue
		}
		seen++
		if m.Api != ai.APIOpenAIResponses {
			t.Errorf("github-copilot/%s api = %q, want %q", m.ID, m.Api, ai.APIOpenAIResponses)
		}
	}
	if seen == 0 {
		t.Fatal("no github-copilot gpt- model in the catalog; the precondition is gone")
	}
}

// Responses replays function_call arguments as JSON.stringify's text
// (openai-responses-shared.ts): &, < and > stay as themselves.
func TestResponsesReplayedArgumentsAreJSONStringify(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1|fc_1", Name: "bash", Arguments: map[string]any{"command": "make && ./run <in >out"}}},
			Model:   model.ID, Provider: model.Provider, Api: model.Api, StopReason: ai.StopToolUse, Timestamp: 2,
		},
	}}
	body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})
	input, _ := body["input"].([]any)
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] == "function_call" {
			if got, want := m["arguments"], `{"command":"make && ./run <in >out"}`; got != want {
				t.Fatalf("arguments = %v, want %s", got, want)
			}
			return
		}
	}
	t.Fatalf("no function_call item in %v", body["input"])
}
