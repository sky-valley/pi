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
	final := StreamOpenAIResponses(context.Background(), model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}, ReasoningEffort: "medium"}).Result()

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

// mustResponsesInput converts messages, failing the test on conversion errors.
func mustResponsesInput(t *testing.T, model *ai.Model, req ai.Context) []any {
	t.Helper()
	in, err := responsesInput(model, req)
	if err != nil {
		t.Fatalf("responsesInput: %v", err)
	}
	return in
}

// mustBuildResponsesParams builds request params, failing the test on errors.
func mustBuildResponsesParams(t *testing.T, model *ai.Model, req ai.Context, opts *OpenAIResponsesOptions) map[string]any {
	t.Helper()
	params, err := buildResponsesParams(model, req, opts)
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
	return StreamOpenAIResponses(context.Background(), &m, req,
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}}).Result()
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
		return StreamOpenAIResponses(context.Background(), &m, req,
			&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}})
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

// Tools must carry strict:false (port of convertResponsesTools default).
func TestResponsesToolsStrictFalse(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools:    []ai.Tool{{Name: "calc", Description: "calc", Parameters: ai.Object(ai.Prop("x", ai.Integer()))}},
	}
	body := mustBuildResponsesParams(t, model, req, &OpenAIResponsesOptions{})
	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools wrong: %#v", body["tools"])
	}
	if strict, has := tools[0]["strict"]; !has || strict != false {
		t.Fatalf("tool must set strict:false, got %v (has=%v)", strict, has)
	}
	if tools[0]["type"] != "function" || tools[0]["name"] != "calc" {
		t.Fatalf("tool body shape wrong: %#v", tools[0])
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

// mapResponsesStatus ports pi's status mapping incl. unknown→error.
func TestResponsesMapStatus(t *testing.T) {
	cases := []struct {
		status string
		want   ai.StopReason
		err    bool
	}{
		{"", ai.StopStop, false},
		{"completed", ai.StopStop, false},
		{"incomplete", ai.StopLength, false},
		{"failed", ai.StopError, false},
		{"cancelled", ai.StopError, false},
		{"in_progress", ai.StopStop, false},
		{"queued", ai.StopStop, false},
		{"weird", ai.StopStop, true},
	}
	for _, c := range cases {
		got, err := mapResponsesStatus(c.status)
		if (err != nil) != c.err {
			t.Fatalf("status %q err=%v want err=%v", c.status, err, c.err)
		}
		if !c.err && got != c.want {
			t.Fatalf("status %q got %s want %s", c.status, got, c.want)
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
	return StreamOpenAIResponses(context.Background(), &m, req, opts).Result()
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

// D2: flex halves cost, priority doubles it (×2.5 for the exact id gpt-5.5),
// and the response-reported service tier wins over the requested option.
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

// D3: session cache headers — session_id gated on compat.sendSessionIdHeader,
// x-client-request-id always sent with a sessionId, both suppressed when
// cacheRetention is "none" (pi openai-responses.ts:115,200-205).
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
		StreamOpenAIResponses(context.Background(), &m, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
		return got
	}

	h := capture(reasoningModel(), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1"}})
	if h.Get("session_id") != "sess-1" || h.Get("x-client-request-id") != "sess-1" {
		t.Fatalf("expected both session headers, got session_id=%q x-client-request-id=%q", h.Get("session_id"), h.Get("x-client-request-id"))
	}

	noSidModel := reasoningModel()
	noSidModel.Compat = json.RawMessage(`{"sendSessionIdHeader":false}`)
	h2 := capture(noSidModel, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1"}})
	if h2.Get("session_id") != "" {
		t.Fatalf("sendSessionIdHeader:false must suppress session_id, got %q", h2.Get("session_id"))
	}
	if h2.Get("x-client-request-id") != "sess-1" {
		t.Fatalf("x-client-request-id must still be sent, got %q", h2.Get("x-client-request-id"))
	}

	h3 := capture(reasoningModel(), &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{SessionID: "sess-1", CacheRetention: ai.CacheNone}})
	if h3.Get("session_id") != "" || h3.Get("x-client-request-id") != "" {
		t.Fatalf("cacheRetention none must suppress both headers, got session_id=%q x-client-request-id=%q", h3.Get("session_id"), h3.Get("x-client-request-id"))
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
	StreamOpenAIResponses(context.Background(), model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}}).Result()
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
	StreamOpenAIResponses(context.Background(), model, visionReq, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}}).Result()
	if got.Get("Copilot-Vision-Request") != "true" {
		t.Fatalf("vision header missing with image input")
	}
}

// D6: cloudflare-ai-gateway resolves {VAR} placeholders in baseUrl, sends the
// API key via cf-aig-authorization, and suppresses the default Authorization
// (pi openai-responses.ts:212-223).
func TestResponsesCloudflareAIGateway(t *testing.T) {
	var gotPath string
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct42")
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "cloudflare-ai-gateway",
		BaseURL: server.URL + "/{CLOUDFLARE_ACCOUNT_ID}"}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	final := StreamOpenAIResponses(context.Background(), model, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "cfkey"}}).Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	if gotPath != "/acct42/responses" {
		t.Fatalf("baseURL placeholder not resolved, path %q", gotPath)
	}
	if got.Get("cf-aig-authorization") != "Bearer cfkey" {
		t.Fatalf("cf-aig-authorization wrong: %q", got.Get("cf-aig-authorization"))
	}
	if got.Get("authorization") != "" {
		t.Fatalf("default Authorization must be suppressed for cloudflare-ai-gateway, got %q", got.Get("authorization"))
	}
}

// D6: a missing {VAR} env fails the stream with pi's exact message.
func TestResponsesCloudflareMissingEnvFailsStream(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "cloudflare-ai-gateway",
		BaseURL: "https://gateway.example/{CLOUDFLARE_ACCOUNT_ID}"}
	final := StreamOpenAIResponses(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "cfkey"}}).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error, got %s", final.StopReason)
	}
	if final.ErrorMessage != "CLOUDFLARE_ACCOUNT_ID is required for provider cloudflare-ai-gateway but is not set." {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// D7a: HTTP errors use pi's Responses format — formatOpenAIResponsesError
// wrapping the openai SDK APIError message (`${status} ${msg}`).
func TestResponsesHTTPErrorFormat(t *testing.T) {
	run := func(status int, body string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
		defer server.Close()
		m := *reasoningModel()
		m.BaseURL = server.URL
		final := StreamOpenAIResponses(context.Background(), &m, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
			&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}}).Result()
		return final.ErrorMessage
	}
	if got := run(429, `{"error":{"message":"slow down"}}`); got != "OpenAI API error (429): 429 slow down" {
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
	if _, err := responsesInput(model, req); err == nil {
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
	stream := StreamOpenAIResponses(context.Background(), &m, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "sk"}})
	var end *ai.ToolCall
	for ev := range stream.Events() {
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
	// pi never lands this block in content — replicated exactly.
	for _, c := range final.Content {
		if _, ok := c.(ai.ToolCall); ok {
			t.Fatalf("toolCall must not be appended to content: %#v", final.Content)
		}
	}
	if final.StopReason != ai.StopStop {
		t.Fatalf("stop reason wrong: %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// D7f: response.completed with a null response still maps the stop reason and
// promotes toolUse (pi shared :518-521 runs outside the response null-check).
func TestResponsesCompletedNullResponse(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":""}}

data: {"type":"response.function_call_arguments.delta","delta":"{\"x\":1}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":"{\"x\":1}"}}

data: {"type":"response.completed"}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("null response.completed should still promote toolUse, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// D7h: onPayload/onResponse errors fail the stream; a non-nil onPayload return
// replaces the params wholesale.
func TestResponsesOnPayloadOnResponsePropagation(t *testing.T) {
	model := reasoningModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	final := runResponsesSSEOpts(t, model, req, "", &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		OnPayload: func(payload any, m *ai.Model) (any, error) { return nil, fmt.Errorf("payload veto") },
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
	StreamOpenAIResponses(context.Background(), &m, req, &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		APIKey:    "sk",
		OnPayload: func(payload any, mm *ai.Model) (any, error) { return map[string]any{"replaced": true}, nil },
	}}).Result()
	if gotBody == nil || gotBody["replaced"] != true || len(gotBody) != 1 {
		t.Fatalf("onPayload replacement must be wholesale: %#v", gotBody)
	}

	final3 := runResponsesSSEOpts(t, model, req, "", &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		OnResponse: func(resp ai.ProviderResponse, mm *ai.Model) error { return fmt.Errorf("response veto") },
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

data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28,"input_tokens_details":{"cached_tokens":5}}}}

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
