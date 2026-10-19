package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Providers without mid-conversation system messages fold every system message
// into the leading prompt (upstream 9e05370b2). The three "folds …" cases are
// transliterated from packages/ai/test/transcript-tool-changes.test.ts at the
// sha, same fixture and expected values; they go through ai.StreamSimple, as
// the upstream suite goes through compat.ts streamSimple.

func transcriptTool(name string) ai.Tool {
	return ai.Tool{Name: name, Description: name + " tool", Parameters: ai.Object()}
}

// transcriptChangesContext is the suite's `context` fixture.
func transcriptChangesContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.Sections = ai.SystemSections{
		{Name: "rules", Value: foldStr("<rules>\nold rules\n</rules>")},
		{Name: "docs", Value: foldStr("<docs>\nread docs\n</docs>")},
	}
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	update := ai.NewSystemText("updated guidance", 2)
	update.Sections = ai.SystemSections{
		{Name: "rules", Value: foldStr("<rules>\nnew rules\n</rules>")},
		{Name: "docs"},
	}
	update.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	return ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}
}

func foldStr(s string) *string { return &s }

const foldedPrompt = "base prompt\n\nupdated guidance\n\n<rules>\nnew rules\n</rules>"

// foldModel is the suite's `modelBase` spread with the per-case fields.
func foldModel(id string, api ai.Api, provider string, compat string) *ai.Model {
	m := &ai.Model{
		ID: id, Name: id, Api: api, Provider: provider, BaseURL: "http://127.0.0.1:9",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000,
	}
	if compat != "" {
		m.Compat = json.RawMessage(compat)
	}
	return m
}

// captureStreamSimplePayload runs ai.StreamSimple far enough to build the
// request body, halting from OnPayload (the suite's capturePayload).
func captureStreamSimplePayload(t *testing.T, model *ai.Model, req ai.Context) map[string]any {
	t.Helper()
	var captured any
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "test-key"
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		captured = payload
		return nil, errors.New("payload captured")
	}
	final := ai.StreamSimple(context.Background(), model, req, opts).Result()
	body, _ := captured.(map[string]any)
	if body == nil {
		t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	return body
}

// payloadItems reads a []map[string]any or []any payload field as maps.
func payloadItems(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	switch v := body[key].(type) {
	case nil:
		return nil
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, len(v))
		for i, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("%s[%d] is %T, want an object", key, i, item)
			}
			out[i] = m
		}
		return out
	default:
		t.Fatalf("%s is %T, want an array", key, v)
		return nil
	}
}

func TestFoldsAnthropicUpdatesIntoTheSystemPromptWithoutNativeSupport(t *testing.T) {
	model := foldModel("claude-sonnet-4-5", ai.APIAnthropicMessages, "anthropic", "")
	body := captureStreamSimplePayload(t, model, transcriptChangesContext())

	if betas, _ := body["betas"].([]string); slices.Contains(betas, "inline-tools-2026-09-15") {
		t.Fatalf("betas = %v, must not carry the native tool-changes beta", betas)
	}
	var system []string
	for _, block := range payloadItems(t, body, "system") {
		text, _ := block["text"].(string)
		system = append(system, text)
	}
	if !slices.Equal(system, []string{foldedPrompt}) {
		t.Fatalf("system = %q, want [%q]", system, foldedPrompt)
	}
	var tools []string
	for _, tool := range payloadItems(t, body, "tools") {
		name, _ := tool["name"].(string)
		tools = append(tools, name)
	}
	if !slices.Equal(tools, []string{"late_tool"}) {
		t.Fatalf("tools = %v, want [late_tool]", tools)
	}
	var roles []string
	for _, message := range payloadItems(t, body, "messages") {
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	if !slices.Equal(roles, []string{"user"}) {
		t.Fatalf("message roles = %v, want [user]", roles)
	}
}

func TestFoldsOpenAIUpdatesIntoTheLeadingDeveloperMessageWithoutNativeSupport(t *testing.T) {
	model := foldModel("gpt-4.1", ai.APIOpenAIResponses, "openai", `{"supportsAdditionalTools":true}`)
	body := captureStreamSimplePayload(t, model, transcriptChangesContext())

	var tools []string
	for _, tool := range payloadItems(t, body, "tools") {
		name, _ := tool["name"].(string)
		tools = append(tools, name)
	}
	if !slices.Equal(tools, []string{"late_tool"}) {
		t.Fatalf("tools = %v, want [late_tool]", tools)
	}
	input := payloadItems(t, body, "input")
	var kinds []string
	for _, item := range input {
		kind, _ := item["type"].(string)
		if kind == "" {
			kind, _ = item["role"].(string)
		}
		kinds = append(kinds, kind)
	}
	if !slices.Equal(kinds, []string{"developer", "user"}) {
		t.Fatalf("input kinds = %v, want [developer user]", kinds)
	}
	if got := input[0]["content"]; got != foldedPrompt {
		t.Fatalf("input[0].content = %q, want %q", got, foldedPrompt)
	}
}

func TestFoldsOpenAICompatibleUpdatesIntoTheSystemPromptWithoutNativeSupport(t *testing.T) {
	model := foldModel("custom-model", ai.APIOpenAICompletions, "custom-provider", "")
	model.Name = "Custom model"
	model.Reasoning = false
	body := captureStreamSimplePayload(t, model, transcriptChangesContext())

	var tools []string
	for _, tool := range payloadItems(t, body, "tools") {
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		tools = append(tools, name)
	}
	if !slices.Equal(tools, []string{"late_tool"}) {
		t.Fatalf("tools = %v, want [late_tool]", tools)
	}
	messages := payloadItems(t, body, "messages")
	var roles []string
	for _, message := range messages {
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	if !slices.Equal(roles, []string{"system", "user"}) {
		t.Fatalf("message roles = %v, want [system user]", roles)
	}
	if got := messages[0]["content"]; got != foldedPrompt {
		t.Fatalf("messages[0].content = %q, want %q", got, foldedPrompt)
	}
}

// transcriptAdditionContext is the suite's `additionContext` fixture: purely
// additive tool history, which a native transport could anchor.
func transcriptAdditionContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	update := ai.NewSystemText("updated guidance", 2)
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	return ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}
}

// Additive history does not anchor on a model without mid-conversation system
// messages: the transcript is collapsed before the tools are resolved, so every
// current tool is a request tool. Captured through openai-responses streamSimple
// under node at 9e05370b2: tools [base_tool, late_tool], input [developer
// "base prompt\n\nupdated guidance", user].
func TestAdditiveOpenAIToolsFoldBeforeAnchoringWithoutNativeSupport(t *testing.T) {
	model := foldModel("gpt-4.1", ai.APIOpenAIResponses, "openai", `{"supportsAdditionalTools":true}`)
	body := captureStreamSimplePayload(t, model, transcriptAdditionContext())

	var tools []string
	for _, tool := range payloadItems(t, body, "tools") {
		name, _ := tool["name"].(string)
		tools = append(tools, name)
	}
	if !slices.Equal(tools, []string{"base_tool", "late_tool"}) {
		t.Fatalf("tools = %v, want [base_tool late_tool]", tools)
	}
	input := payloadItems(t, body, "input")
	var kinds []string
	for _, item := range input {
		kind, _ := item["type"].(string)
		if kind == "" {
			kind, _ = item["role"].(string)
		}
		kinds = append(kinds, kind)
	}
	if !slices.Equal(kinds, []string{"developer", "user"}) || input[0]["content"] != "base prompt\n\nupdated guidance" {
		t.Fatalf("input = %v, want [developer %q, user]", input, "base prompt\n\nupdated guidance")
	}
}

// The converters resolve the transcript themselves, as pi's convertMessages and
// convertResponsesMessages do, so a builder handed an unresolved transcript
// still folds it.
func TestConvertersFoldAnUnresolvedTranscript(t *testing.T) {
	completions := foldModel("custom-model", ai.APIOpenAICompletions, "custom-provider", "")
	completions.Reasoning = false
	params, err := buildOpenAIParams(completions, ai.NormalizeContext(transcriptChangesContext()), &OpenAIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := payloadItems(t, params, "messages")
	if len(messages) != 2 || messages[0]["role"] != "system" || messages[0]["content"] != foldedPrompt || messages[1]["role"] != "user" {
		t.Fatalf("completions messages = %v, want [system %q, user]", messages, foldedPrompt)
	}

	responses := foldModel("gpt-4.1", ai.APIOpenAIResponses, "openai", "")
	input, err := responsesInput(responses, ai.NormalizeContext(transcriptChangesContext()), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input) != 2 || input[0].(map[string]any)["role"] != "developer" || input[0].(map[string]any)["content"] != foldedPrompt {
		t.Fatalf("responses input = %v, want [developer %q, user]", input, foldedPrompt)
	}
}

// The leading system message does not advance the msg_pi_<index> fallback ids
// (convertResponsesMessages at 9e05370b2 under node: [developer, user,
// msg_pi_1, msg_pi_1_1, user]).
func TestResponsesLeadingSystemMessageDoesNotAdvanceFallbackIDs(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", Reasoning: true, Input: []string{"text"}}
	req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.TextContent{Text: "first"}, ai.TextContent{Text: "second"}},
			Api:     ai.APIOpenAIResponses, Provider: "openai", Model: "gpt-5", StopReason: ai.StopStop, Timestamp: 2,
		},
		ai.NewUserText("again", 3),
	}}
	input, err := responsesInput(model, ai.NormalizeContext(req), "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range input {
		m := item.(map[string]any)
		if id, ok := m["id"].(string); ok {
			got = append(got, id)
		} else {
			role, _ := m["role"].(string)
			got = append(got, role)
		}
	}
	if want := []string{"developer", "user", "msg_pi_1", "msg_pi_1_1", "user"}; !slices.Equal(got, want) {
		t.Fatalf("input ids = %v, want %v", got, want)
	}
}

// Google always collapses: the replayed prompt becomes systemInstruction, the
// current tools are declared, and the conversation drops the leading system
// message (google-generative-ai.ts / google-shared.ts at 9e05370b2; pi's SDK
// call params captured under node carried systemInstruction = the folded
// prompt, one late_tool declaration, and contents [{role:user, parts:[{text:
// "before"}]}]).
func TestGoogleCollapsesSystemMessagesIntoSystemInstruction(t *testing.T) {
	model := foldModel("gemini-2.5-flash", ai.APIGoogleGenerativeAI, "google", "")
	model.Reasoning = false
	body := captureStreamSimplePayload(t, model, transcriptChangesContext())

	instruction, _ := body["systemInstruction"].(map[string]any)
	parts, _ := instruction["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != foldedPrompt {
		t.Fatalf("systemInstruction = %#v, want the folded prompt", body["systemInstruction"])
	}
	var names []string
	for _, tool := range payloadItems(t, body, "tools") {
		declarations, _ := tool["functionDeclarations"].([]any)
		for _, d := range declarations {
			name, _ := d.(map[string]any)["name"].(string)
			names = append(names, name)
		}
	}
	if !slices.Equal(names, []string{"late_tool"}) {
		t.Fatalf("function declarations = %v, want [late_tool]", names)
	}
	contents, err := json.Marshal(body["contents"])
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"parts":[{"text":"before"}],"role":"user"}]`; string(contents) != want {
		t.Fatalf("contents = %s, want %s", contents, want)
	}
}

// Copilot dynamic headers read the transcript each adapter resolved: after a
// fold the trailing system message is gone, so a request whose last message is
// a system update is still user-initiated (buildCopilotDynamicHeaders over
// normalizedContext.messages at 9e05370b2).
func TestCopilotInitiatorReadsTheCollapsedTranscript(t *testing.T) {
	var initiator string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initiator = r.Header.Get("X-Initiator")
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, openAISSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAICompletions, Provider: "github-copilot", BaseURL: server.URL, Input: []string{"text"}}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1), ai.NewSystemText("later guidance", 2)}}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "k"
	ai.StreamSimple(context.Background(), model, req, opts).Result()
	if initiator != "user" {
		t.Fatalf("X-Initiator = %q, want user (the trailing system message folds into the head)", initiator)
	}
}

// Tools declared only by a later system message are current tools: they turn on
// the fine-grained tool streaming beta for a model without eager input
// streaming (shouldUseFineGrainedToolStreamingBeta over getCurrentTools).
func TestAnthropicFineGrainedBetaCountsTranscriptTools(t *testing.T) {
	model := foldModel("claude-test", ai.APIAnthropicMessages, "anthropic", `{"supportsEagerToolInputStreaming":false}`)
	declared := ai.NewSystemText("", 2)
	declared.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	body := captureStreamSimplePayload(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1), declared}})
	if betas, _ := body["betas"].([]string); !slices.Contains(betas, fineGrainedToolStreamBeta) {
		t.Fatalf("betas = %v, want %s", betas, fineGrainedToolStreamBeta)
	}
}

// Under OAuth the streamed tool_use name maps back onto the CURRENT tools, which
// now come from the transcript's system messages.
func TestAnthropicOAuthMapsToolNamesFromTranscriptTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, anthropicSSE) // streams a tool_use named get_weather
	}))
	defer server.Close()
	model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, Input: []string{"text"}, MaxTokens: 4096}
	declared := ai.NewSystemText("be helpful", 0)
	declared.ToolsAdded = []ai.Tool{transcriptTool("Get_Weather")}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "sk-ant-oat-secret"
	final := ai.StreamSimple(context.Background(), model, ai.Context{Messages: []ai.Message{declared, ai.NewUserText("hi", 1)}}, opts).Result()
	var names []string
	for _, block := range final.Content {
		if call, ok := block.(ai.ToolCall); ok {
			names = append(names, call.Name)
		}
	}
	if !slices.Equal(names, []string{"Get_Weather"}) {
		t.Fatalf("tool call names = %v (stop %s %q), want [Get_Weather]", names, final.StopReason, final.ErrorMessage)
	}
}

// pi-messages posts the normalized transcript, not the raw context: the prompt
// and tools ride a leading system message and the body has no systemPrompt or
// tools keys (A2). Bytes captured from normalizeContext under node at
// 9e05370b2; compared modulo the HTML escaping recorded as D9.
func TestPiMessagesPostsTheNormalizedTranscript(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`))
	}))
	defer server.Close()
	model := &ai.Model{ID: "auto", Api: ai.APIPiMessages, Provider: "radius", BaseURL: server.URL + "/v1", Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 16384}
	req := ai.Context{
		SystemPrompt: "Be terse.",
		Tools:        []ai.Tool{transcriptTool("base_tool")},
		Messages:     []ai.Message{ai.NewUserText("Hello", 7)},
	}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "test-key"
	ai.StreamSimple(context.Background(), model, req, opts).Result()

	var posted struct {
		Context json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(body, &posted); err != nil {
		t.Fatalf("posted body %q: %v", body, err)
	}
	const want = `{"messages":[{"role":"system","content":"Be terse.","toolsAdded":[{"name":"base_tool","description":"base_tool tool","parameters":{"type":"object","properties":{}}}],"timestamp":0},{"role":"user","content":"Hello","timestamp":7}]}`
	if got := strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&").Replace(string(posted.Context)); got != want {
		t.Fatalf("posted context\n got %s\nwant %s", got, want)
	}
}
