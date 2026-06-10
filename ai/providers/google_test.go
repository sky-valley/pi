package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

const googleSSE = `data: {"candidates":[{"content":{"parts":[{"text":"Think","thought":true}]}}],"responseId":"resp_1"}

data: {"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":"there"}]}}]}

data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"cachedContentTokenCount":1,"totalTokenCount":18}}

`

func TestGoogleProviderParsesStream(t *testing.T) {
	var gotBody map[string]any
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "g-key" {
			t.Errorf("missing api key header")
		}
		gotURL = r.URL.String()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL,
		Reasoning: true, MaxTokens: 8192, Cost: ai.ModelCost{Input: 0.3, Output: 2.5},
	}
	req := ai.Context{
		SystemPrompt: "sys",
		Messages:     []ai.Message{ai.NewUserText("hi", 1)},
		Tools:        []ai.Tool{{Name: "lookup", Description: "look up", Parameters: ai.Object(ai.Prop("q", ai.String()))}},
	}
	maxTok := 8192
	final := StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{APIKey: "g-key", MaxTokens: &maxTok}}).Result()

	if final.StopReason != ai.StopToolUse {
		t.Fatalf("expected toolUse, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if final.ResponseID != "resp_1" {
		t.Fatalf("responseId wrong: %q", final.ResponseID)
	}
	var thinking, text string
	var tool *ai.ToolCall
	for _, c := range final.Content {
		switch v := c.(type) {
		case ai.ThinkingContent:
			thinking = v.Thinking
		case ai.TextContent:
			text = v.Text
		case ai.ToolCall:
			tc := v
			tool = &tc
		}
	}
	if thinking != "Think" {
		t.Fatalf("thinking wrong: %q", thinking)
	}
	if text != "Hello there" {
		t.Fatalf("text wrong: %q", text)
	}
	if tool == nil || tool.Name != "lookup" || tool.Arguments["q"] != "x" {
		t.Fatalf("tool wrong: %#v", tool)
	}
	// usage: input = prompt(10) - cached(1) = 9; output = candidates(5)+thoughts(2)=7; cacheRead=1
	if final.Usage.Input != 9 || final.Usage.Output != 7 || final.Usage.CacheRead != 1 {
		t.Fatalf("usage wrong: %+v", final.Usage)
	}
	if !strings.Contains(gotURL, "streamGenerateContent") || !strings.Contains(gotURL, "alt=sse") {
		t.Fatalf("url wrong: %s", gotURL)
	}
	if _, ok := gotBody["contents"]; !ok {
		t.Fatalf("contents not sent: %v", gotBody)
	}
	// REST body shape: tools/toolConfig/systemInstruction lifted to top level; the
	// bogus `config` mirror must be gone; generationConfig holds only gen params.
	if _, ok := gotBody["config"]; ok {
		t.Fatalf("bogus top-level config present: %v", gotBody)
	}
	if _, ok := gotBody["tools"]; !ok {
		t.Fatalf("tools not at top level: %v", gotBody)
	}
	if _, ok := gotBody["systemInstruction"]; !ok {
		t.Fatalf("systemInstruction not at top level: %v", gotBody)
	}
	si, _ := gotBody["systemInstruction"].(map[string]any)
	if si == nil || si["role"] != "user" {
		t.Fatalf("systemInstruction shape wrong: %v", gotBody["systemInstruction"])
	}
	gen, _ := gotBody["generationConfig"].(map[string]any)
	if gen == nil {
		t.Fatalf("generationConfig missing: %v", gotBody)
	}
	if _, ok := gen["maxOutputTokens"]; !ok {
		t.Fatalf("generationConfig missing maxOutputTokens: %v", gen)
	}
	for _, bad := range []string{"tools", "toolConfig", "systemInstruction"} {
		if _, ok := gen[bad]; ok {
			t.Fatalf("generationConfig should not contain %s: %v", bad, gen)
		}
	}
	// default tool schema goes through parametersJsonSchema for non-Claude models.
	toolsArr, _ := gotBody["tools"].([]any)
	if len(toolsArr) == 0 {
		t.Fatalf("tools empty: %v", gotBody["tools"])
	}
	fd0 := toolsArr[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if _, ok := fd0["parametersJsonSchema"]; !ok {
		t.Fatalf("expected parametersJsonSchema for gemini model: %v", fd0)
	}
}

// roundtripBody marshals/unmarshals the built params so tests inspect the wire
// JSON (matching what the server receives) rather than the live map.
func roundtripBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return out
}

func firstFunctionResponse(contents []any) map[string]any {
	for _, c := range contents {
		m, _ := c.(map[string]any)
		if m == nil {
			continue
		}
		parts, _ := m["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if fr, ok := pm["functionResponse"].(map[string]any); ok {
				return fr
			}
		}
	}
	return nil
}

// --- Task 4 + 5: REST body shape & disabled-thinking per family ---

func TestGoogleDisabledThinkingPerFamily(t *testing.T) {
	cases := []struct {
		id      string
		wantKey string
		wantVal any
	}{
		{"gemini-2.5-flash", "thinkingBudget", float64(0)},
		{"gemini-3-pro-preview", "thinkingLevel", "LOW"},
		{"gemini-3-flash-preview", "thinkingLevel", "MINIMAL"},
		{"gemma-4-12b", "thinkingLevel", "MINIMAL"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			model := &ai.Model{ID: tc.id, Api: ai.APIGoogleGenerativeAI, Provider: "google", Reasoning: true}
			opts := &GoogleOptions{ThinkingProvided: true, ThinkingEnabled: false}
			body := roundtripBody(t, buildGoogleParams(model, ai.Context{}, opts))
			gen, _ := body["generationConfig"].(map[string]any)
			if gen == nil {
				t.Fatalf("no generationConfig: %v", body)
			}
			tc2, _ := gen["thinkingConfig"].(map[string]any)
			if tc2 == nil {
				t.Fatalf("no thinkingConfig: %v", gen)
			}
			if got := tc2[tc.wantKey]; got != tc.wantVal {
				t.Fatalf("%s: want %s=%v, got %v (cfg=%v)", tc.id, tc.wantKey, tc.wantVal, got, tc2)
			}
			// includeThoughts must NOT be set on the disabled path.
			if _, ok := tc2["includeThoughts"]; ok {
				t.Fatalf("disabled thinkingConfig must not set includeThoughts: %v", tc2)
			}
		})
	}
}

func TestGoogleThinkingConfigUnderGenerationConfig(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", Reasoning: true}
	budget := 8192
	opts := &GoogleOptions{ThinkingProvided: true, ThinkingEnabled: true, ThinkingBudget: &budget}
	body := roundtripBody(t, buildGoogleParams(model, ai.Context{}, opts))
	if _, ok := body["thinkingConfig"]; ok {
		t.Fatalf("thinkingConfig must not be at top level: %v", body)
	}
	gen := body["generationConfig"].(map[string]any)
	tc := gen["thinkingConfig"].(map[string]any)
	if tc["includeThoughts"] != true || tc["thinkingBudget"] != float64(8192) {
		t.Fatalf("thinkingConfig wrong: %v", tc)
	}
}

func TestGoogleNoThinkingConfigWhenNotProvided(t *testing.T) {
	// Generic Stream path (RegisterGoogle) never sets ThinkingProvided.
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", Reasoning: true}
	body := roundtripBody(t, buildGoogleParams(model, ai.Context{}, &GoogleOptions{}))
	if gen, ok := body["generationConfig"].(map[string]any); ok {
		if _, has := gen["thinkingConfig"]; has {
			t.Fatalf("thinkingConfig must be absent when thinking not provided: %v", gen)
		}
	}
}

// --- Task 1 + 2: tool-result images & responseValue ---

func TestGoogleToolResultImageGemini2SeparateTurn(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{Provider: "google", Model: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI,
			Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "shot", Arguments: map[string]any{}}}},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "shot", Content: ai.ContentList{
			ai.ImageContent{MimeType: "image/png", Data: "AAAA"},
		}},
	}}
	body := roundtripBody(t, buildGoogleParams(model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	// 2.x: user, model, user(functionResponse), user(Tool result image) = 4 turns.
	if len(contents) != 4 {
		t.Fatalf("expected 4 turns for Gemini 2.x image tool result, got %d: %v", len(contents), contents)
	}
	fr := firstFunctionResponse(contents)
	if fr == nil {
		t.Fatalf("no functionResponse")
	}
	if _, ok := fr["parts"]; ok {
		t.Fatalf("Gemini 2.x must NOT nest images in functionResponse.parts: %v", fr)
	}
	if resp := fr["response"].(map[string]any); resp["output"] != "(see attached image)" {
		t.Fatalf("image-only responseValue wrong: %v", resp)
	}
	last := contents[3].(map[string]any)
	parts := last["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "Tool result image:" {
		t.Fatalf("expected 'Tool result image:' turn: %v", last)
	}
	if _, ok := parts[1].(map[string]any)["inlineData"]; !ok {
		t.Fatalf("expected inlineData in image turn: %v", parts)
	}
}

func TestGoogleToolResultImageGemini3Nested(t *testing.T) {
	model := &ai.Model{ID: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI, Provider: "google", Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{Provider: "google", Model: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI,
			Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "shot", Arguments: map[string]any{}}}},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "shot", Content: ai.ContentList{
			ai.ImageContent{MimeType: "image/png", Data: "AAAA"},
		}},
	}}
	body := roundtripBody(t, buildGoogleParams(model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	// Gemini 3: user, model, user(functionResponse with nested parts) = 3 turns.
	if len(contents) != 3 {
		t.Fatalf("expected 3 turns for Gemini 3 nested image tool result, got %d: %v", len(contents), contents)
	}
	fr := firstFunctionResponse(contents)
	imgs, ok := fr["parts"].([]any)
	if !ok || len(imgs) != 1 {
		t.Fatalf("Gemini 3 must nest images in functionResponse.parts: %v", fr)
	}
	if _, ok := imgs[0].(map[string]any)["inlineData"]; !ok {
		t.Fatalf("nested part missing inlineData: %v", imgs)
	}
}

func TestGoogleToolResultEmptyResponseValue(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google"}
	req := ai.Context{Messages: []ai.Message{
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "noop", Content: ai.ContentList{}},
	}}
	body := roundtripBody(t, buildGoogleParams(model, req, &GoogleOptions{}))
	fr := firstFunctionResponse(body["contents"].([]any))
	if resp := fr["response"].(map[string]any); resp["output"] != "" {
		t.Fatalf("empty tool result should yield empty output, got %v", resp)
	}
}

// --- Task 3: convertTools field choice (pins pi's actual runtime behavior) ---

// pi google.ts:356 and google-vertex.ts:445 both call convertTools(context.tools)
// with NO second argument, so useParameters defaults to false and BOTH providers
// ALWAYS emit parametersJsonSchema — for every model, Claude-via-Google included.
// useParameters() therefore returns false unconditionally. These tests pin that.
func TestGoogleConvertToolsAlwaysUsesJsonSchema(t *testing.T) {
	schema := ai.Object(ai.Prop("q", ai.String()))
	// Claude-via-Google must still go through parametersJsonSchema, matching pi.
	for _, modelID := range []string{"claude-3-5-sonnet", "gpt-oss-120b", "gemini-2.5-flash", "gemini-3-pro"} {
		if useParameters(modelID) {
			t.Fatalf("useParameters(%q) must be false to match pi (always parametersJsonSchema)", modelID)
		}
		tools := googleTools([]ai.Tool{{Name: "t", Description: "d", Parameters: schema}}, useParameters(modelID))
		fd := roundtripFD(t, tools)
		if _, ok := fd["parametersJsonSchema"]; !ok {
			t.Fatalf("model %q must use parametersJsonSchema: %v", modelID, fd)
		}
		if _, ok := fd["parameters"]; ok {
			t.Fatalf("model %q must not emit OpenAPI parameters: %v", modelID, fd)
		}
	}
}

// sanitizeForOpenApi is retained from pi (google-shared.ts) even though the runtime
// `parameters` branch is never taken; assert it strips JSON-Schema meta-declarations
// exactly like pi so the helper stays faithful if a future caller needs it.
func TestGoogleSanitizeForOpenApiStripsMetaKeys(t *testing.T) {
	schema := ai.Object(ai.Prop("q", ai.String()))
	schema.Extra = map[string]any{
		"$schema":     "http://json-schema.org/draft-07/schema#",
		"$defs":       map[string]any{"X": map[string]any{"type": "string"}},
		"definitions": map[string]any{"Y": map[string]any{"type": "number"}},
	}
	sanitized := sanitizeForOpenApi(schemaToGeneric(schema))
	params, ok := sanitized.(map[string]any)
	if !ok {
		t.Fatalf("expected sanitized object: %#v", sanitized)
	}
	for _, meta := range []string{"$schema", "$defs", "definitions"} {
		if _, has := params[meta]; has {
			t.Fatalf("meta key %s not stripped: %v", meta, params)
		}
	}
	if params["type"] != "object" {
		t.Fatalf("sanitized schema lost its shape: %v", params)
	}
}

func roundtripFD(t *testing.T, tools []any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(tools)
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	return out[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
}

// --- Task 6: unknown / safety finishReason ---

func googleStreamWithFinish(t *testing.T, finish string) *ai.AssistantMessage {
	t.Helper()
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"" + finish + "\"}]}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	return StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
}

func TestGoogleFinishReasonSafety(t *testing.T) {
	final := googleStreamWithFinish(t, "SAFETY")
	if final.StopReason != ai.StopError {
		t.Fatalf("SAFETY should be error, got %s", final.StopReason)
	}
	if final.ErrorMessage != "An unknown error occurred" {
		t.Fatalf("SAFETY error message wrong: %q", final.ErrorMessage)
	}
}

func TestGoogleFinishReasonUnknownFails(t *testing.T) {
	final := googleStreamWithFinish(t, "TOTALLY_NEW_REASON")
	if final.StopReason != ai.StopError {
		t.Fatalf("unknown finishReason should be error, got %s", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "Unhandled stop reason: TOTALLY_NEW_REASON") {
		t.Fatalf("unknown finishReason message wrong: %q", final.ErrorMessage)
	}
}

func TestGoogleFinishReasonStop(t *testing.T) {
	final := googleStreamWithFinish(t, "STOP")
	if final.StopReason != ai.StopStop {
		t.Fatalf("STOP should be stop, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

// --- Task 7: text-part thoughtSignature both directions ---

func TestGoogleTextSignatureRecv(t *testing.T) {
	// base64-valid signature on a text part should land on TextSignature.
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\",\"thoughtSignature\":\"YWJjZA==\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	final := StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	var sig string
	for _, c := range final.Content {
		if tc, ok := c.(ai.TextContent); ok {
			sig = tc.TextSignature
		}
	}
	if sig != "YWJjZA==" {
		t.Fatalf("text signature not captured on recv: %q", sig)
	}
}

func TestGoogleTextSignatureSend(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google"}
	req := ai.Context{Messages: []ai.Message{
		ai.AssistantMessage{Provider: "google", Model: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI,
			Content: ai.ContentList{ai.TextContent{Text: "hi", TextSignature: "YWJjZA=="}}},
	}}
	body := roundtripBody(t, buildGoogleParams(model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	p := parts[0].(map[string]any)
	if p["thoughtSignature"] != "YWJjZA==" {
		t.Fatalf("text thoughtSignature not replayed: %v", p)
	}
}

func TestGoogleTextSignatureDroppedCrossModel(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google"}
	req := ai.Context{Messages: []ai.Message{
		ai.AssistantMessage{Provider: "openai", Model: "gpt-4", Api: ai.APIOpenAICompletions,
			Content: ai.ContentList{ai.TextContent{Text: "hi", TextSignature: "YWJjZA=="}}},
	}}
	body := roundtripBody(t, buildGoogleParams(model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if _, ok := parts[0].(map[string]any)["thoughtSignature"]; ok {
		t.Fatalf("cross-model thoughtSignature must be dropped: %v", parts[0])
	}
}

// --- Task 8: duplicate / empty tool-call id ---

func TestGoogleDuplicateAndEmptyToolCallIDs(t *testing.T) {
	// Two empty-id calls + one duplicate id, all in one response.
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[" +
		"{\"functionCall\":{\"name\":\"a\",\"args\":{}}}," +
		"{\"functionCall\":{\"name\":\"a\",\"args\":{}}}," +
		"{\"functionCall\":{\"id\":\"dup\",\"name\":\"b\",\"args\":{}}}," +
		"{\"functionCall\":{\"id\":\"dup\",\"name\":\"b\",\"args\":{}}}" +
		"]},\"finishReason\":\"STOP\"}]}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	final := StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	var ids []string
	for _, c := range final.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			ids = append(ids, tc.ID)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("expected 4 tool calls, got %d: %v", len(ids), ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Fatalf("empty tool-call id not regenerated: %v", ids)
		}
		if seen[id] {
			t.Fatalf("duplicate tool-call id not regenerated: %v", ids)
		}
		seen[id] = true
	}
	// The first kept-as-provided "dup" must survive; the second must be regenerated.
	if ids[2] != "dup" {
		t.Fatalf("first provided id should be kept: %v", ids)
	}
}

// googleServe streams a fixed SSE body and runs StreamGoogle against it.
func googleServe(t *testing.T, modelID, sse string) *ai.AssistantMessageEventStream {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	model := &ai.Model{ID: modelID, Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	return StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}})
}

// --- F1: gemma-4 thinkingLevel map ---

func TestGoogleThinkingLevelMaps(t *testing.T) {
	cases := []struct {
		id     string
		effort string
		want   string
	}{
		// gemma-4 (pi google.ts:441-450): minimal/low -> MINIMAL, medium/high -> HIGH.
		{"gemma-4-12b", "minimal", "MINIMAL"},
		{"gemma-4-12b", "low", "MINIMAL"},
		{"gemma-4-12b", "medium", "HIGH"},
		{"gemma-4-12b", "high", "HIGH"},
		// gemini-3-pro mapping kept: minimal/low -> LOW, medium/high -> HIGH.
		{"gemini-3-pro-preview", "minimal", "LOW"},
		{"gemini-3-pro-preview", "low", "LOW"},
		{"gemini-3-pro-preview", "medium", "HIGH"},
		{"gemini-3-pro-preview", "high", "HIGH"},
		// gemini-3-flash falls to the generic 1:1 map.
		{"gemini-3-flash-preview", "minimal", "MINIMAL"},
		{"gemini-3-flash-preview", "low", "LOW"},
		{"gemini-3-flash-preview", "medium", "MEDIUM"},
		{"gemini-3-flash-preview", "high", "HIGH"},
		// xhigh matches no case anywhere -> "" (pi returns undefined).
		{"gemma-4-12b", "xhigh", ""},
		{"gemini-3-pro-preview", "xhigh", ""},
		{"gemini-3-flash-preview", "xhigh", ""},
	}
	for _, tc := range cases {
		if got := googleThinkingLevel(tc.effort, tc.id); got != tc.want {
			t.Errorf("googleThinkingLevel(%q,%q) = %q, want %q", tc.effort, tc.id, got, tc.want)
		}
	}
}

// --- F2: text-part presence semantics ---

func TestGoogleSignatureOnlyPartProducesNothing(t *testing.T) {
	// A part with only thoughtSignature (no text, no functionCall) matches
	// neither of pi's independent checks (google.ts:97,158) -> no block, no event.
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"thoughtSignature\":\"YWJjZA==\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	stream := googleServe(t, "gemini-2.5-flash", sse)
	var types []ai.EventType
	for ev := range stream.Events() {
		types = append(types, ev.Type)
	}
	final := stream.Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if len(final.Content) != 1 {
		t.Fatalf("signature-only part must produce no content block: %#v", final.Content)
	}
	text, ok := final.Content[0].(ai.TextContent)
	if !ok || text.Text != "hello" {
		t.Fatalf("expected single text block: %#v", final.Content[0])
	}
	for _, et := range types {
		if et == ai.EventThinkingStart {
			t.Fatalf("signature-only part must not start a thinking block: %v", types)
		}
	}
	// Exactly one text_start (no phantom empty block before it).
	starts := 0
	for _, et := range types {
		if et == ai.EventTextStart {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("expected exactly 1 text_start, got %d (%v)", starts, types)
	}
}

func TestGoogleTextAndFunctionCallSamePart(t *testing.T) {
	// pi's checks are independent: a part with BOTH text and functionCall
	// processes both, in that order.
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"before\",\"functionCall\":{\"name\":\"f\",\"args\":{\"a\":1}}}]},\"finishReason\":\"STOP\"}]}\n\n"
	final := googleServe(t, "gemini-2.5-flash", sse).Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stream failed: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if len(final.Content) != 2 {
		t.Fatalf("expected text + toolCall blocks, got %#v", final.Content)
	}
	text, ok := final.Content[0].(ai.TextContent)
	if !ok || text.Text != "before" {
		t.Fatalf("text block wrong: %#v", final.Content[0])
	}
	tc, ok := final.Content[1].(ai.ToolCall)
	if !ok || tc.Name != "f" {
		t.Fatalf("toolCall block wrong: %#v", final.Content[1])
	}
}

func TestGoogleEmptyStringTextPartIsPresent(t *testing.T) {
	// `"text":""` is present (not undefined) in pi -> it opens a block.
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	final := googleServe(t, "gemini-2.5-flash", sse).Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if len(final.Content) != 1 {
		t.Fatalf("empty-string text part must still open a block: %#v", final.Content)
	}
	if text, ok := final.Content[0].(ai.TextContent); !ok || text.Text != "" {
		t.Fatalf("expected empty text block: %#v", final.Content[0])
	}
}

// --- F3: mid-stream error chunks + truncated streams ---

func TestGoogleErrorChunkFailsStream(t *testing.T) {
	errJSON := `{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n" +
		"data: " + errJSON + "\n\n"
	stream := googleServe(t, "gemini-2.5-flash", sse)
	sawDone := false
	for ev := range stream.Events() {
		if ev.Type == ai.EventDone {
			sawDone = true
		}
	}
	final := stream.Result()
	if sawDone {
		t.Fatalf("error chunk must not produce a clean done event")
	}
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	// genai SDK ApiError surface: "got status: ${status}. ${JSON.stringify(chunk)}".
	want := "got status: RESOURCE_EXHAUSTED. " + errJSON
	if final.ErrorMessage != want {
		t.Fatalf("error message wrong:\n got %q\nwant %q", final.ErrorMessage, want)
	}
}

func TestGoogleBareJSONErrorChunkFailsStream(t *testing.T) {
	// Google emits mid-stream errors as bare JSON (no data: prefix); the SDK
	// detects these before SSE parsing and throws an ApiError.
	errJSON := `{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n\n" + errJSON
	final := googleServe(t, "gemini-2.5-flash", sse).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if final.ErrorMessage != "got status: INTERNAL. "+errJSON {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

func TestGoogleTruncatedStreamFails(t *testing.T) {
	// Stream ends with an unconsumed partial segment (no trailing delimiter):
	// the SDK throws "Incomplete JSON segment at the end".
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"par"
	final := googleServe(t, "gemini-2.5-flash", sse).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("truncated stream must fail, got %s", final.StopReason)
	}
	if final.ErrorMessage != "Incomplete JSON segment at the end" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// --- F4a: toolCall thoughtSignature present from toolcall_start on ---

func TestGoogleToolCallSignatureOnStartPartial(t *testing.T) {
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":{}},\"thoughtSignature\":\"YWJjZA==\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	stream := googleServe(t, "gemini-2.5-flash", sse)
	checked := false
	for ev := range stream.Events() {
		if ev.Type != ai.EventToolCallStart {
			continue
		}
		checked = true
		tc, ok := ev.Partial.Content[ev.ContentIndex].(ai.ToolCall)
		if !ok {
			t.Fatalf("toolcall_start partial has no ToolCall at index %d: %#v", ev.ContentIndex, ev.Partial.Content)
		}
		// pi builds the ToolCall WITH thoughtSignature before pushing
		// toolcall_start (google.ts:186-195).
		if tc.ThoughtSignature != "YWJjZA==" {
			t.Fatalf("thoughtSignature missing on toolcall_start partial: %#v", tc)
		}
	}
	if !checked {
		t.Fatalf("no toolcall_start event observed")
	}
	final := stream.Result()
	tc := final.Content[0].(ai.ToolCall)
	if tc.ThoughtSignature != "YWJjZA==" {
		t.Fatalf("thoughtSignature missing on final ToolCall: %#v", tc)
	}
}

// --- F4b: generationConfig always sent ---

func TestGoogleGenerationConfigAlwaysSent(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google"}
	// No temperature, no maxTokens, no thinking: SDK still sends generationConfig {}.
	body := roundtripBody(t, buildGoogleParams(model, ai.Context{}, &GoogleOptions{}))
	gen, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig must always be sent (SDK setValueByPath is unconditional): %v", body)
	}
	if len(gen) != 0 {
		t.Fatalf("expected empty generationConfig, got %v", gen)
	}
}

// --- F4c: xhigh passes through with no level and no budget ---

func TestGoogleXHighNoLevelNoBudget(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer server.Close()
	// Model advertises xhigh (thinkingLevelMap presence makes it a supported
	// level) so clampThinkingLevel keeps it instead of clamping to high.
	xhigh := "xhigh"
	model := &ai.Model{
		ID: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		BaseURL: server.URL, Reasoning: true,
		ThinkingLevelMap: ai.ThinkingLevelMap{"xhigh": &xhigh},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	StreamSimpleGoogle(context.Background(), model, req, &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{APIKey: "k"}, Reasoning: ai.ThinkingXHigh,
	}).Result()
	gen, _ := gotBody["generationConfig"].(map[string]any)
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if tc == nil {
		t.Fatalf("thinkingConfig missing: %v", gen)
	}
	if tc["includeThoughts"] != true {
		t.Fatalf("includeThoughts must be set: %v", tc)
	}
	// pi: xhigh falls through getThinkingLevel/getGoogleBudget -> neither key.
	if _, has := tc["thinkingLevel"]; has {
		t.Fatalf("xhigh must not map to a thinkingLevel: %v", tc)
	}
	if _, has := tc["thinkingBudget"]; has {
		t.Fatalf("xhigh must not map to a thinkingBudget: %v", tc)
	}
}

func TestGoogleOnPayloadErrorFailsStream(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", Input: []string{"text"}}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
	}))
	defer server.Close()
	model.BaseURL = server.URL
	opts := &GoogleOptions{StreamOptions: ai.StreamOptions{
		APIKey: "k",
		OnPayload: func(payload any, m *ai.Model) (any, error) {
			return nil, errors.New("payload veto")
		},
	}}
	final := StreamGoogle(context.Background(), model, req, opts).Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "payload veto" {
		t.Fatalf("onPayload error must fail the stream: %s / %q", final.StopReason, final.ErrorMessage)
	}
	if requested {
		t.Fatalf("request must not be sent when onPayload errors")
	}
}
