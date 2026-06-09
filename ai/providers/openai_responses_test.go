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
	body := buildResponsesParams(model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
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
	body := buildResponsesParams(model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, &OpenAIResponsesOptions{})
	if _, ok := body["include"]; ok {
		t.Fatalf("non-reasoning model must not send include")
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("non-reasoning model must not send reasoning")
	}
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
	input := responsesInput(model, req)
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
	in := responsesInput(model, foreign)
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
	in2 := responsesInput(model, crossModel)
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
	in := responsesInput(model, req)
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
	in := responsesInput(model, req)
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
	body := buildResponsesParams(model, req, &OpenAIResponsesOptions{})
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
	if got := firstRole(responsesInput(reasoning, req)); got != "developer" {
		t.Fatalf("reasoning model should use developer role, got %q", got)
	}

	nonReasoning := &ai.Model{ID: "gpt-4o", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: false}
	if got := firstRole(responsesInput(nonReasoning, req)); got != "system" {
		t.Fatalf("non-reasoning model should use system role, got %q", got)
	}

	noDevRole := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true,
		Compat: json.RawMessage(`{"supportsDeveloperRole":false}`)}
	if got := firstRole(responsesInput(noDevRole, req)); got != "system" {
		t.Fatalf("supportsDeveloperRole:false should use system role, got %q", got)
	}
}

// github-copilot reasoning-off must not send a reasoning block.
func TestResponsesReasoningOffExcludesCopilot(t *testing.T) {
	copilot := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "github-copilot", Reasoning: true}
	body := buildResponsesParams(copilot, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, &OpenAIResponsesOptions{})
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("github-copilot reasoning-off must omit reasoning, got %v", body["reasoning"])
	}

	other := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true}
	body2 := buildResponsesParams(other, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, &OpenAIResponsesOptions{})
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
