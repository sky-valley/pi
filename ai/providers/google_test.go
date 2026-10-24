package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(req), &GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key"}, MaxTokens: &maxTok}}).Result()

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

// Upstream 16235fd93: a disabled thinkingConfig is thinkingBudget:0 unless the
// model uses Gemini's level control AND its thinkingLevelMap marks "off"
// unsupported; then it is the lowest supported level, resolved through the map,
// without includeThoughts. The maps below are the shipped catalog's shapes for
// each family (inlined, so the test does not follow a catalog regen); the same
// ids without a map keep "off" supported and disable with a budget. pi's values:
// testdata/google-thinking/thinking-config-1692bba63.json "disabled/*".
func TestGoogleDisabledThinkingPerFamily(t *testing.T) {
	proMap := ai.ThinkingLevelMap{"off": nil, "minimal": nil, "low": strPtr("LOW"), "medium": nil, "high": strPtr("HIGH")}
	flashMap := ai.ThinkingLevelMap{"off": nil}
	gemmaMap := ai.ThinkingLevelMap{"off": nil, "minimal": strPtr("MINIMAL"), "low": nil, "medium": nil, "high": strPtr("HIGH")}
	budgetOff := map[string]any{"thinkingBudget": float64(0)}
	cases := []struct {
		name   string
		id     string
		levels ai.ThinkingLevelMap
		want   map[string]any
	}{
		{"gemini 2.5", "gemini-2.5-flash", nil, budgetOff},
		{"gemini 3 pro", "gemini-3.1-pro-preview", proMap, map[string]any{"thinkingLevel": "LOW"}},
		{"gemini 3 flash", "gemini-3-flash-preview", flashMap, map[string]any{"thinkingLevel": "MINIMAL"}},
		{"gemma 4", "gemma-4-31b-it", gemmaMap, map[string]any{"thinkingLevel": "MINIMAL"}},
		// pi b0c8f65f / #5761: the rolling flash aliases use levels like the
		// gemini-3 flash family.
		{"flash latest", "gemini-flash-latest", flashMap, map[string]any{"thinkingLevel": "MINIMAL"}},
		{"flash lite latest", "gemini-flash-lite-latest", flashMap, map[string]any{"thinkingLevel": "MINIMAL"}},
		{"gemini 3 pro without a map", "gemini-3-pro-preview", nil, budgetOff},
		{"gemini 3 flash without a map", "gemini-3-flash-preview", nil, budgetOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: tc.id, Api: ai.APIGoogleGenerativeAI, Provider: "google", Reasoning: true, ThinkingLevelMap: tc.levels}
			opts := &GoogleOptions{ThinkingProvided: true, ThinkingEnabled: false}
			body := roundtripBody(t, mustBuildGoogleParams(t, model, ai.Context{}, opts))
			gen, _ := body["generationConfig"].(map[string]any)
			if gen == nil {
				t.Fatalf("no generationConfig: %v", body)
			}
			// Exact: includeThoughts must NOT be set on the disabled path.
			if got := gen["thinkingConfig"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("thinkingConfig\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// getDisabledGoogleThinkingConfig resolves its fallback through the map, which
// can fail: buildGoogleParams returns that error.
func TestGoogleDisabledThinkingUnmappableFallbackFails(t *testing.T) {
	model := &ai.Model{
		ID: "gemini-3.8-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", Reasoning: true,
		ThinkingLevelMap: ai.ThinkingLevelMap{"off": nil, "minimal": strPtr("extreme")},
	}
	opts := &GoogleOptions{ThinkingProvided: true, ThinkingEnabled: false}
	_, err := buildGoogleParams(model, ai.NormalizeContext(ai.Context{}), opts)
	want := "Unsupported Google thinking level mapping for google/gemini-3.8-flash: minimal -> extreme"
	if err == nil || err.Error() != want {
		t.Fatalf("want error %q, got %v", want, err)
	}
}

func TestGoogleThinkingConfigUnderGenerationConfig(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", Reasoning: true}
	budget := 8192
	opts := &GoogleOptions{ThinkingProvided: true, ThinkingEnabled: true, ThinkingBudget: &budget}
	body := roundtripBody(t, mustBuildGoogleParams(t, model, ai.Context{}, opts))
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, ai.Context{}, &GoogleOptions{}))
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
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
		tools, err := googleTools([]ai.Tool{{Name: "t", Description: "d", Parameters: schema}}, useParameters(modelID), supportsGoogleStrictToolSampling(modelID))
		if err != nil {
			t.Fatalf("googleTools: %v", err)
		}
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
	generic, err := schemaToGeneric(schema)
	if err != nil {
		t.Fatal(err)
	}
	sanitized := sanitizeForOpenApi(generic)
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

func googleStreamSSE(t *testing.T, sse string) *ai.AssistantMessage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	return StreamGoogle(context.Background(), model, ai.NormalizeContext(req), &GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).Result()
}

func googleStreamWithFinish(t *testing.T, finish string) *ai.AssistantMessage {
	t.Helper()
	return googleStreamSSE(t, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\""+finish+"\"}]}\n\n")
}

// TestGoogleFinishReasonSafety mirrors pi's google-raw-stop-reason.test.ts. Since
// d7b02636 a terminal error names the provider's own finish reason instead of the
// generic "An unknown error occurred".
//
// TOO_MANY_TOOL_CALLS joined pi's error-mapped set in upstream 4a6ed0194 (the
// @google/genai 1.52.0 -> 2.21.0 bump); 71dca871b dropped it and ceea48f5d
// reverted that drop three days later.
func TestGoogleFinishReasonSafety(t *testing.T) {
	for _, finish := range []string{"SAFETY", "MALFORMED_FUNCTION_CALL", "TOO_MANY_TOOL_CALLS"} {
		t.Run(finish, func(t *testing.T) {
			final := googleStreamWithFinish(t, finish)
			if final.StopReason != ai.StopError {
				t.Fatalf("should be error, got %s", final.StopReason)
			}
			if final.RawStopReason != finish {
				t.Fatalf("rawStopReason = %q, want %q", final.RawStopReason, finish)
			}
			if want := "Provider stopped with: " + finish; final.ErrorMessage != want {
				t.Fatalf("error message = %q, want %q", final.ErrorMessage, want)
			}
		})
	}
}

// TestGoogleRawStopReasonPreservedOnStop pins that the raw finish reason rides
// along on a non-error stop too (pi assigns it before mapping).
func TestGoogleRawStopReasonPreservedOnStop(t *testing.T) {
	final := googleStreamWithFinish(t, "STOP")
	if final.StopReason != ai.StopStop {
		t.Fatalf("STOP should map to stop, got %s", final.StopReason)
	}
	if final.RawStopReason != "STOP" {
		t.Fatalf("rawStopReason = %q, want STOP", final.RawStopReason)
	}
}

// TestGoogleFinishReasonUnknownFails pins the exhaustive-switch fallthrough: pi's
// mapStopReason throws "Unhandled stop reason: <reason>" and the catch in
// google-generative-ai.ts lands error.message verbatim in errorMessage (a plain
// Error carries no status, so formatProviderError adds nothing), which is why the
// message is asserted byte-for-byte.
func TestGoogleFinishReasonUnknownFails(t *testing.T) {
	for _, finish := range []string{"TOTALLY_NEW_REASON"} {
		t.Run(finish, func(t *testing.T) {
			final := googleStreamWithFinish(t, finish)
			if final.StopReason != ai.StopError {
				t.Fatalf("unknown finishReason should be error, got %s", final.StopReason)
			}
			if final.RawStopReason != finish {
				t.Fatalf("rawStopReason = %q, want %q", final.RawStopReason, finish)
			}
			if want := "Unhandled stop reason: " + finish; final.ErrorMessage != want {
				t.Fatalf("error message = %q, want %q", final.ErrorMessage, want)
			}
		})
	}
}

func TestGoogleFinishReasonStop(t *testing.T) {
	final := googleStreamWithFinish(t, "STOP")
	if final.StopReason != ai.StopStop {
		t.Fatalf("STOP should be stop, got %s (%s)", final.StopReason, final.ErrorMessage)
	}
}

func googleStreamWithFinishAndToolCall(t *testing.T, finish string) *ai.AssistantMessage {
	t.Helper()
	return googleStreamSSE(t, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"id\":\"call-1\",\"name\":\"echo\",\"args\":{\"value\":\"truncated\"}}}]},\"finishReason\":\""+finish+"\"}]}\n\n")
}

// TestGoogleLengthStopPreservedWithToolCalls mirrors pi's "preserves MAX_TOKENS
// with a tool call as length": the toolUse override only applies to a plain
// STOP, so a truncated response keeps its length stop.
func TestGoogleLengthStopPreservedWithToolCalls(t *testing.T) {
	final := googleStreamWithFinishAndToolCall(t, "MAX_TOKENS")
	if final.StopReason != ai.StopLength {
		t.Fatalf("MAX_TOKENS with tool call should stay length, got %s", final.StopReason)
	}
	if final.RawStopReason != "MAX_TOKENS" {
		t.Fatalf("rawStopReason = %q, want MAX_TOKENS", final.RawStopReason)
	}
	hasToolCall := slices.ContainsFunc(final.Content, func(b ai.Content) bool {
		_, ok := b.(ai.ToolCall)
		return ok
	})
	if !hasToolCall {
		t.Fatalf("content should carry the tool call: %+v", final.Content)
	}
}

// TestGoogleStopWithToolCallsMapsToolUse pins the surviving half of the
// override: a plain STOP with tool calls still becomes toolUse.
func TestGoogleStopWithToolCallsMapsToolUse(t *testing.T) {
	final := googleStreamWithFinishAndToolCall(t, "STOP")
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("STOP with tool call should map to toolUse, got %s", final.StopReason)
	}
	if final.RawStopReason != "STOP" {
		t.Fatalf("rawStopReason = %q, want STOP", final.RawStopReason)
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
	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(req), &GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).Result()
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if _, ok := parts[0].(map[string]any)["thoughtSignature"]; ok {
		t.Fatalf("cross-model thoughtSignature must be dropped: %v", parts[0])
	}
}

// --- signed empty text/thinking blocks (pi 6138f5a07) ---

// googleModelParts builds a request from one assistant turn and returns its
// "model" content parts.
func googleModelParts(t *testing.T, am ai.AssistantMessage) []any {
	t.Helper()
	model := &ai.Model{ID: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI, Provider: "google"}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("Hi", 1), am}}
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
	for _, c := range body["contents"].([]any) {
		m := c.(map[string]any)
		if m["role"] == "model" {
			return m["parts"].([]any)
		}
	}
	t.Fatalf("no model turn in contents: %v", body["contents"])
	return nil
}

// Gemini can attach a thoughtSignature to a part whose visible text is empty and
// requires it echoed back; dropping the block breaks the reasoning chain and the
// model intermittently ends a mid-task turn with a thought-only STOP.
func TestGoogleSignedEmptyThinkingKept(t *testing.T) {
	parts := googleModelParts(t, ai.AssistantMessage{
		Provider: "google", Model: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI,
		Content: ai.ContentList{
			ai.ThinkingContent{Thinking: "", ThinkingSignature: "AAAAAAAAAAAAAAAAAAAAAA=="},
			ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
		},
	})
	var signed []map[string]any
	for _, p := range parts {
		m := p.(map[string]any)
		if m["thoughtSignature"] == "AAAAAAAAAAAAAAAAAAAAAA==" {
			signed = append(signed, m)
		}
	}
	if len(signed) != 1 {
		t.Fatalf("want 1 signed part, got %d: %v", len(signed), parts)
	}
	if signed[0]["thought"] != true {
		t.Fatalf("signed empty thinking must stay a thought part: %v", signed[0])
	}
	if signed[0]["text"] != "" {
		t.Fatalf("signed empty thinking text should stay empty: %v", signed[0])
	}
}

func TestGoogleSignedEmptyTextKept(t *testing.T) {
	parts := googleModelParts(t, ai.AssistantMessage{
		Provider: "google", Model: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI,
		Content: ai.ContentList{
			ai.TextContent{Text: "", TextSignature: "AAAAAAAAAAAAAAAAAAAAAA=="},
			ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
		},
	})
	var signed []map[string]any
	for _, p := range parts {
		m := p.(map[string]any)
		if m["thoughtSignature"] == "AAAAAAAAAAAAAAAAAAAAAA==" {
			signed = append(signed, m)
		}
	}
	if len(signed) != 1 {
		t.Fatalf("want 1 signed part, got %d: %v", len(signed), parts)
	}
	if _, ok := signed[0]["thought"]; ok {
		t.Fatalf("signed empty text must not become a thought part: %v", signed[0])
	}
	if signed[0]["text"] != "" {
		t.Fatalf("signed empty text should stay empty: %v", signed[0])
	}
}

func TestGoogleUnsignedEmptyBlocksStillDropped(t *testing.T) {
	parts := googleModelParts(t, ai.AssistantMessage{
		Provider: "google", Model: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI,
		Content: ai.ContentList{
			ai.ThinkingContent{Thinking: ""},
			ai.TextContent{Text: "   "},
			ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
		},
	})
	if len(parts) != 1 {
		t.Fatalf("unsigned empty blocks must be dropped, got %v", parts)
	}
	if _, ok := parts[0].(map[string]any)["functionCall"]; !ok {
		t.Fatalf("only the tool call should survive: %v", parts[0])
	}
}

// Locked one layer up, not by the converter's cross-model branch: transformMessages
// strips cross-model thinking and signatures before googleContents runs, so the
// loosened empty-block skips above cannot leak a signature cross-model. Invariant
// under the 6138f5a07 port by design — a regression lock on the risk that port
// introduces, not coverage of the (unreachable) cross-model branch itself.
func TestGoogleCrossModelSignaturesNeverReachRequest(t *testing.T) {
	parts := googleModelParts(t, ai.AssistantMessage{
		Provider: "google", Model: "other-model", Api: ai.APIGoogleGenerativeAI,
		Content: ai.ContentList{
			ai.ThinkingContent{Thinking: "", ThinkingSignature: "AAAAAAAAAAAAAAAAAAAAAA=="},
			ai.TextContent{Text: "", TextSignature: "AAAAAAAAAAAAAAAAAAAAAA=="},
			ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
		},
	})
	if len(parts) != 1 {
		t.Fatalf("cross-model empty blocks must be dropped, got %v", parts)
	}
	if _, ok := parts[0].(map[string]any)["functionCall"]; !ok {
		t.Fatalf("only the tool call should survive: %v", parts[0])
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	if strings.Contains(string(raw), "AAAAAAAAAAAAAAAAAAAAAA==") {
		t.Fatalf("cross-model signature must not leak: %s", raw)
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
	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(req), &GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}).Result()
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
	return StreamGoogle(context.Background(), model, ai.NormalizeContext(req), &GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}})
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

// TestGoogleSSEErrorEventDoesNotFailStream: @google/genai 2.21.0 checks for an
// {"error":...} payload only when a whole body read is bare JSON
// (processStreamResponse); a data: event is parsed and converted by
// generateContentResponseFromMldev, which keeps no "error" field, so pi sees an
// empty chunk and reads on. Measured by testdata/google-stream-events
// (scenarios "an SSE-framed error event ..."). parity-sweep-2 F3 had recorded
// the opposite as SDK behaviour.
func TestGoogleSSEErrorEventDoesNotFailStream(t *testing.T) {
	errEvent := "data: {\"error\":{\"code\":429,\"message\":\"quota exceeded\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n"
	text := func(s string) string {
		return "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + s + "\"}]}}]}\n\n"
	}
	stop := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]},\"finishReason\":\"STOP\"}]}\n\n"

	t.Run("the stream reads on", func(t *testing.T) {
		final := googleServe(t, "gemini-2.5-flash", text("before ")+errEvent+text("after")+stop).Result()
		if final.StopReason != ai.StopStop {
			t.Fatalf("stop reason %s (%s), want stop", final.StopReason, final.ErrorMessage)
		}
		if got := final.Content[0].(ai.TextContent).Text; got != "before after!" {
			t.Fatalf("text %q, want %q", got, "before after!")
		}
	})
	t.Run("as the last event", func(t *testing.T) {
		final := googleServe(t, "gemini-2.5-flash", text("partial")+errEvent).Result()
		if final.StopReason != ai.StopError || final.ErrorMessage != "Google stream ended without a finish reason" {
			t.Fatalf("got %s %q, want error %q", final.StopReason, final.ErrorMessage, "Google stream ended without a finish reason")
		}
	})
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
	body := roundtripBody(t, mustBuildGoogleParams(t, model, ai.Context{}, &GoogleOptions{}))
	gen, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig must always be sent (SDK setValueByPath is unconditional): %v", body)
	}
	if len(gen) != 0 {
		t.Fatalf("expected empty generationConfig, got %v", gen)
	}
}

// --- F4c: an xhigh that maps to nothing Google understands is an error ---
//
// This used to assert the opposite: xhigh fell through both getThinkingLevel and
// getGoogleBudget, producing thinkingConfig:{includeThoughts:true} with neither
// key. Upstream af2c35223 resolves the level through the map first and rejects
// anything that lands outside Google's four standard levels, so a
// self-referential entry like xhigh -> "xhigh" now fails the request instead of
// silently sending an under-specified thinkingConfig.

func TestGoogleXHighUnmappedIsRejected(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
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
	msg := StreamSimpleGoogle(context.Background(), model, ai.NormalizeContext(req), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}, Reasoning: ai.ThinkingXHigh,
	}).
		Result()
	if msg.StopReason != ai.StopError {
		t.Fatalf("want stopReason error, got %q", msg.StopReason)
	}
	want := "Unsupported Google thinking level mapping for google/gemini-3-pro-preview: xhigh -> xhigh"
	if msg.ErrorMessage != want {
		t.Fatalf("want %q, got %q", want, msg.ErrorMessage)
	}
	if requested {
		t.Fatal("no request may be sent for an unresolvable level")
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
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", OnPayload: func(payload any, m *ai.Model) (any, error) {
			return nil, errors.New("payload veto")
		}},
	}}
	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(req), opts).Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "payload veto" {
		t.Fatalf("onPayload error must fail the stream: %s / %q", final.StopReason, final.ErrorMessage)
	}
	if requested {
		t.Fatalf("request must not be sent when onPayload errors")
	}
}

// TestGoogleNeverCallsOnResponse: pi's google-generative-ai adapter hands the
// request to the @google/genai client and never calls onResponse (no call in
// google-generative-ai.ts at 8676a0dcd), so a hook that would fail the stream
// is never reached, on success or on an HTTP error.
func TestGoogleNeverCallsOnResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   ai.StopReason
	}{
		{"success", http.StatusOK, googleSSE, ai.StopToolUse},
		{"http error", http.StatusBadRequest, `{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`, ai.StopError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
			calls := 0
			opts := &GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "k",
				OnResponse: func(ai.ProviderResponse, *ai.Model) error {
					calls++
					return errors.New("response veto")
				},
			}}}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			final := StreamGoogle(context.Background(), model, ai.NormalizeContext(req), opts).Result()
			if calls != 0 {
				t.Fatalf("OnResponse called %d times; pi's google adapter never calls it", calls)
			}
			if final.StopReason != tc.want {
				t.Fatalf("stop reason %s (%s), want %s", final.StopReason, final.ErrorMessage, tc.want)
			}
		})
	}
}

// TestGoogleRefusedConnectionFailsFetch: a request that gets no response
// fails as fetch rejects it in pi, with undici's "fetch failed" and no start
// event, not with net/http's error text. Measured against pi's stream at
// 8676a0dcd (node v26.4.0): a closed port gives [error] "fetch failed". The
// malformed-response case is the capture's "a malformed response fails the
// fetch".
func TestGoogleRefusedConnectionFailsFetch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + ln.Addr().String()
	ln.Close()
	var types []string
	stream := StreamGoogle(context.Background(), googleCaptureModel(baseURL),
		ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}})
	for ev := range stream.Events() {
		types = append(types, string(ev.Type))
	}
	final := stream.Result()
	if !slices.Equal(types, []string{"error"}) || final.StopReason != ai.StopError || final.ErrorMessage != "fetch failed" {
		t.Fatalf("stream %v, stop %s %q; pi: [error], error \"fetch failed\"", types, final.StopReason, final.ErrorMessage)
	}
}

// TestGoogleIgnoresTimeoutMs: pi hands @google/genai no timeout (createClient
// sets no httpOptions.timeout; retryGoogleRequest takes maxRetries,
// maxRetryDelayMs and signal), so TimeoutMs does not bound a google request.
// Measured against pi's stream at 8676a0dcd (node v26.4.0): timeoutMs 50 with
// the headers 1500ms late → start … done, stop "stop".
func TestGoogleIgnoresTimeoutMs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()
	final := StreamGoogle(context.Background(), googleCaptureModel(server.URL),
		ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", TimeoutMs: 50}}}).Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop %s %q; pi ignores timeoutMs and reads the late response", final.StopReason, final.ErrorMessage)
	}
}

// TestGoogleHeadersTimeoutFailsFetch: what bounds the wait for headers is
// fetch's own headersTimeout, undici's 300s default, and its expiry fails as
// every other no-response failure does: "fetch failed", no start event.
// Measured against pi's stream at 8676a0dcd (node v26.4.0): a server that
// reads the request and never answers → [error] "fetch failed" after 301s,
// timeoutMs 50 or 1000 alike.
func TestGoogleHeadersTimeoutFailsFetch(t *testing.T) {
	shortenUndiciHeadersTimeout(t, 50)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	var types []string
	stream := StreamGoogle(context.Background(), googleCaptureModel(server.URL),
		ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", TimeoutMs: 60_000}}})
	for ev := range stream.Events() {
		types = append(types, string(ev.Type))
	}
	final := stream.Result()
	if !slices.Equal(types, []string{"error"}) || final.StopReason != ai.StopError || final.ErrorMessage != "fetch failed" {
		t.Fatalf("stream %v, stop %s %q; pi: [error], error \"fetch failed\"", types, final.StopReason, final.ErrorMessage)
	}
}

// googleTimeoutError is a transport error that reports itself as a timeout,
// as net/http's dial, TLS-handshake and response-header timeouts do.
type googleTimeoutError struct{ msg string }

func (e googleTimeoutError) Error() string { return e.msg }
func (e googleTimeoutError) Timeout() bool { return true }

// TestGoogleFetchErrorIsFetchFailedForEveryTransportFailure: fetch rejects
// with "fetch failed" whatever failed before the response, timeouts included.
// Measured against pi's stream at 8676a0dcd (node v26.4.0): a TLS handshake
// that never completes and a connect to a non-routable address both end
// [error] "fetch failed" (undici's 10s connect timeout); the refused
// connection is TestGoogleRefusedConnectionFailsFetch.
func TestGoogleFetchErrorIsFetchFailedForEveryTransportFailure(t *testing.T) {
	for _, cause := range []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: googleTimeoutError{"i/o timeout"}},
		googleTimeoutError{"net/http: TLS handshake timeout"},
		googleTimeoutError{"net/http: timeout awaiting response headers"},
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
		io.ErrUnexpectedEOF,
	} {
		err := &url.Error{Op: "Post", URL: "https://generativelanguage.googleapis.com/v1beta/models/g:streamGenerateContent?alt=sse", Err: cause}
		if got := undiciFetchError(err); got.Error() != "fetch failed" {
			t.Errorf("%v (timeout %v) → %q; pi: \"fetch failed\"", cause, err.Timeout(), got)
		}
	}
	// Only client.Do's failures are the transport's: any other error — a
	// *url.Error from parsing a URL included, which the adapters refuse
	// before sending (requestURL) — is returned as it is.
	for _, err := range []error{
		&url.Error{Op: "parse", URL: "http://[::1/x", Err: errors.New("missing ']' in host")},
		errors.New("Ephemeral tokens are only supported by the live API."),
		errRequestAborted,
	} {
		if got := undiciFetchError(err); got != err {
			t.Errorf("%v → %v; want it returned as it is", err, got)
		}
	}
}

// TestGoogleForwardsEachSDKChunkInOrder transliterates
// google-raw-stop-reason.test.ts › 'Google provider stream events' ›
// 'forwards each SDK chunk in order before normalizing it for Google
// Generative AI' (upstream 002fc8385). Upstream mocks the SDK, so its chunks
// are the literals it yields; here they come off the wire, so each is the
// SDK's converted shape — the known fields in the converter's order plus
// sdkHttpResponse.headers (the full record is TestGoogleStreamEventsMatchPi's).
func TestGoogleForwardsEachSDKChunkInOrder(t *testing.T) {
	sse := "data: {\"responseId\":\"resp_google\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1,\"totalTokenCount\":3}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
	var received []ai.OrderedObject
	var eventModels []*ai.Model
	opts := &GoogleOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-api-key"},
		OnProviderStreamEvent: func(data any, eventModel *ai.Model) error {
			received = append(received, data.(ai.OrderedObject))
			eventModels = append(eventModels, eventModel)
			return nil
		},
	}}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	result := StreamGoogle(context.Background(), model, ai.NormalizeContext(req), opts).Result()

	want := []struct{ keys, candidates string }{
		{"candidates responseId sdkHttpResponse", `[{"content":{"parts":[{"text":"hello"}]}}]`},
		{"candidates usageMetadata sdkHttpResponse", `[{"finishReason":"STOP"}]`},
	}
	if len(received) != len(want) {
		t.Fatalf("received %d chunks, want %d", len(received), len(want))
	}
	for i, chunk := range received {
		var keys []string
		for _, f := range chunk {
			keys = append(keys, f.Key)
		}
		if got := strings.Join(keys, " "); got != want[i].keys {
			t.Errorf("chunk %d keys %q, want %q", i, got, want[i].keys)
		}
		candidates, _ := json.Marshal(googleField(chunk, "candidates"))
		if string(candidates) != want[i].candidates {
			t.Errorf("chunk %d candidates %s, want %s", i, candidates, want[i].candidates)
		}
		headers, _ := googleField(chunk, "sdkHttpResponse").(ai.OrderedObject)
		if ct := googleField(googleField(headers, "headers").(ai.OrderedObject), "content-type"); ct != "text/event-stream" {
			t.Errorf("chunk %d content-type %v", i, ct)
		}
	}
	if usage, _ := json.Marshal(googleField(received[1], "usageMetadata")); string(usage) != `{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}` {
		t.Errorf("usageMetadata %s", usage)
	}
	if !slices.Equal(eventModels, []*ai.Model{model, model}) {
		t.Errorf("event models %v, want the stream's model twice", eventModels)
	}
	if result.StopReason != ai.StopStop || result.ResponseID != "resp_google" {
		t.Fatalf("result %s %q (%s)", result.StopReason, result.ResponseID, result.ErrorMessage)
	}
	if len(result.Content) != 1 || result.Content[0].(ai.TextContent).Text != "hello" {
		t.Fatalf("content %#v", result.Content)
	}
}

// TestGoogleProviderStreamEventErrorFailsStream: pi awaits the callback, so a
// throw fails the request (README, 002fc8385): the catch sets errorMessage to
// the thrown message (formatProviderError of a status-less Error) and
// stopReason to "aborted" when the signal is aborted, else "error". The chunk
// the callback refused is never normalized.
func TestGoogleProviderStreamEventErrorFailsStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
		want   ai.StopReason
	}{
		{"error", false, ai.StopError},
		{"aborted", true, ai.StopAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, googleSSE)
			}))
			defer server.Close()
			model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: server.URL}
			calls := 0
			opts := &GoogleOptions{StreamOptions: ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
				OnProviderStreamEvent: func(any, *ai.Model) error {
					calls++
					if tc.cancel {
						cancel()
					}
					return errors.New("observer boom")
				},
			}}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
			stream := StreamGoogle(ctx, model, ai.NormalizeContext(req), opts)
			var types []ai.EventType
			for ev := range stream.Events() {
				types = append(types, ev.Type)
			}
			final := stream.Result()
			if calls != 1 {
				t.Fatalf("callback called %d times, want 1", calls)
			}
			if !slices.Equal(types, []ai.EventType{ai.EventStart, ai.EventError}) {
				t.Fatalf("events %v, want [start error]", types)
			}
			if final.StopReason != tc.want || final.ErrorMessage != "observer boom" {
				t.Fatalf("got %s %q, want %s %q", final.StopReason, final.ErrorMessage, tc.want, "observer boom")
			}
		})
	}
}

// mustBuildGoogleParams builds a generateContent request body, failing the test
// on the errors constrained sampling can raise.
func mustBuildGoogleParams(t *testing.T, model *ai.Model, req ai.Context, opts *GoogleOptions) map[string]any {
	t.Helper()
	params, err := buildGoogleParams(model, ai.NormalizeContext(req), opts)
	if err != nil {
		t.Fatalf("buildGoogleParams: %v", err)
	}
	return params
}

// Upstream 24bace27: Gemini 3+ maps json_schema-constrained tools to the
// VALIDATED function-calling mode, "none"/"any" still win over it, and with no
// tool choice and no constrained tool the toolConfig key is omitted entirely.
func TestGoogleFunctionCallingMode(t *testing.T) {
	plain := ai.Tool{Name: "calc", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.Integer()))}
	strict := plain
	strict.ConstrainedSampling = &ai.ConstrainedSamplingConfig{
		Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer,
	}

	for _, tc := range []struct {
		name       string
		modelID    string
		tool       ai.Tool
		toolChoice string
		want       string // "" means toolConfig must be absent
	}{
		{"gemini-3 strict tool", "gemini-3-pro", strict, "", "VALIDATED"},
		{"gemini-3 strict tool, auto still validated", "gemini-3-pro", strict, "auto", "VALIDATED"},
		{"gemini-3 strict tool, none wins", "gemini-3-pro", strict, "none", "NONE"},
		{"gemini-3 strict tool, any wins", "gemini-3-pro", strict, "any", "ANY"},
		{"gemini-2 strict tool is not validated", "gemini-2.5-pro", strict, "", ""},
		{"gemini-3 plain tool, no choice", "gemini-3-pro", plain, "", ""},
		{"gemini-3 plain tool, explicit auto", "gemini-3-pro", plain, "auto", "AUTO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: tc.modelID, Api: ai.APIGoogleGenerativeAI, Provider: "google"}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{tc.tool}}
			body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{ToolChoice: tc.toolChoice}))
			cfg, has := body["toolConfig"]
			if tc.want == "" {
				if has {
					t.Fatalf("toolConfig must be omitted, got %#v", cfg)
				}
				return
			}
			cfgMap, _ := cfg.(map[string]any)
			fcc, _ := cfgMap["functionCallingConfig"].(map[string]any)
			if fcc["mode"] != tc.want {
				t.Fatalf("mode = %v, want %v", fcc["mode"], tc.want)
			}
		})
	}
}

// A tool that REQUIRES strict sampling fails on a pre-Gemini-3 model.
func TestGoogleStrictSamplingRequireFails(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-pro", Api: ai.APIGoogleGenerativeAI, Provider: "google"}
	req := ai.Context{
		Messages: []ai.Message{ai.NewUserText("hi", 1)},
		Tools: []ai.Tool{{
			Name: "js_require", Description: "d", Parameters: ai.Object(ai.Prop("x", ai.Integer())),
			ConstrainedSampling: &ai.ConstrainedSamplingConfig{
				Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingRequire,
			},
		}},
	}
	_, err := buildGoogleParams(model, ai.NormalizeContext(req), &GoogleOptions{})
	assertErrString(t, err, `Tool "js_require" requires JSON-schema constrained sampling, but strict tools are unsupported.`)
}

// --- Gemini 3 tool call IDs (pi cbaca6038) ---

// TestGoogleRequiresToolCallID locks the model families that get explicit tool
// call IDs. Gemini 3 joined Claude and gpt-oss upstream; the version comes from
// getGeminiMajorVersion, so gemini-3.6 and gemini-live-3 count too and a bare
// "gemini-3x" (no version separator) does not.
func TestGoogleRequiresToolCallID(t *testing.T) {
	cases := []struct {
		modelID string
		want    bool
	}{
		{"gemini-2.5-flash", false},
		{"gemini-3-pro-preview", true},
		{"gemini-3.6-flash", true},
		{"gemini-live-3-flash", true},
		{"gemini-30-pro", true},
		{"GEMINI-3-PRO", true}, // getGeminiMajorVersion lowercases first
		{"gemini3-pro", false},
		{"claude-sonnet-4-5", true},
		{"gpt-oss-120b", true},
		{"other-model", false},
	}
	for _, c := range cases {
		if got := requiresToolCallID(c.modelID); got != c.want {
			t.Errorf("requiresToolCallID(%q) = %v, want %v", c.modelID, got, c.want)
		}
	}
}

// TestGoogleGemini3PreservesToolCallIDs: the request-body consequence — Gemini 3
// echoes the tool call IDs back in functionCall.id / functionResponse.id, while
// Gemini 2.5 still omits them.
func TestGoogleGemini3PreservesToolCallIDs(t *testing.T) {
	for _, modelID := range []string{"gemini-3-pro-preview", "gemini-2.5-flash"} {
		wantIDs := modelID == "gemini-3-pro-preview"
		model := &ai.Model{ID: modelID, Api: ai.APIGoogleGenerativeAI, Provider: "google"}
		req := ai.Context{Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.AssistantMessage{
				Provider: "google", Model: modelID, Api: ai.APIGoogleGenerativeAI,
				Content: ai.ContentList{
					ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
					ai.ToolCall{ID: "call_2", Name: "bash", Arguments: map[string]any{"command": "pwd"}},
				},
				StopReason: ai.StopToolUse,
			},
			ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: "hi"}}},
			ai.ToolResultMessage{ToolCallID: "call_2", ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: "files"}}},
		}}
		body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
		contents := body["contents"].([]any)

		var wantCall, wantResp []string
		if wantIDs {
			wantCall, wantResp = []string{"call_1", "call_2"}, []string{"call_1", "call_2"}
		}
		if got := googlePartIDs(contents, "functionCall"); !slices.Equal(got, wantCall) {
			t.Errorf("%s functionCall ids = %v, want %v", modelID, got, wantCall)
		}
		if got := googlePartIDs(contents, "functionResponse"); !slices.Equal(got, wantResp) {
			t.Errorf("%s functionResponse ids = %v, want %v", modelID, got, wantResp)
		}
	}
}

// googlePartIDs collects the "id" of every part carrying the given key, in
// request order.
func googlePartIDs(contents []any, key string) []string {
	var ids []string
	for _, c := range contents {
		m, _ := c.(map[string]any)
		parts, _ := m["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			inner, ok := pm[key].(map[string]any)
			if !ok {
				continue
			}
			if id, ok := inner["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// @google/genai refuses an ephemeral token (an api key starting
// "auth_tokens/") when it adds its auth header, which it does after it has
// appended the record: a record header that fails does so first, and a record
// x-goog-api-key does not rescue the key, because the refusal comes before the
// check for one. Measured against upstream 8676a0dcd's google-generative-ai.ts
// with @google/genai 2.21.0 under node v26.4.0: all three rows below, request
// never sent.
func TestGoogleRefusesAnEphemeralToken(t *testing.T) {
	var google wireAdapter
	for _, adapter := range wireAdapters() {
		if adapter.name == "google-generative-ai" {
			google = adapter
		}
	}
	const refused = "Ephemeral tokens are only supported by the live API."
	for _, tc := range []struct {
		name    string
		headers ai.ProviderHeaders
		want    string
	}{
		{"no record key", nil, refused},
		{"record key", ai.ProviderHeaders{"x-goog-api-key": strPtr("rec")}, refused},
		{"failing record header first", ai.ProviderHeaders{"X-A": strPtr(string(rune(cjk)))}, byteStringError(0, cjk)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, final := runWire(t, google, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "auth_tokens/abc", Headers: tc.headers,
			}})
			wantNotSent(t, h, final, tc.want)
		})
	}
}
