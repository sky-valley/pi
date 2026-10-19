package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Anthropic models that accept system messages and tool changes
// mid-conversation get later system messages in place and tool changes as
// tool_removal/tool_addition blocks (upstream 9e05370b2), each later tool
// defined inline (upstream b271b0a52). The first four tests transliterate the
// anthropic cases of packages/ai/test/transcript-tool-changes.test.ts at
// b271b0a52 (the fifth, "folds … without native support", is
// TestFoldsAnthropicUpdatesInto…); the full request bodies are pi's, captured
// under node at dc939913f by testdata/transcript/capture-anthropic-native.mts
// (at b271b0a52^ it reproduces the 9e05370b2 capture; at dc939913f^ the
// b271b0a52 one, and dc939913f moves only nativeEffort, now "off").

const anthropicNativeCaptureFile = "testdata/transcript/anthropic-native-dc939913f.json"

const wantToolChangesBeta = "inline-tools-2026-09-15"

// anthropicNativeModel is the suite's anthropicNativeModel.
func anthropicNativeModel() *ai.Model {
	m := foldModel("claude-opus-5", ai.APIAnthropicMessages, "anthropic",
		`{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true}`)
	m.Name = "Claude Opus 5"
	return m
}

// captureAnthropicPayload is captureStreamSimplePayload with a chosen key, so
// an OAuth token can select the Claude Code request shape.
func captureAnthropicPayload(t *testing.T, model *ai.Model, req ai.Context, apiKey string, reasoning ai.ThinkingLevel) map[string]any {
	t.Helper()
	var captured map[string]any
	opts := &ai.SimpleStreamOptions{Reasoning: reasoning}
	opts.APIKey = apiKey
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		captured, _ = payload.(map[string]any)
		return nil, errors.New("payload captured")
	}
	final := ai.StreamSimple(context.Background(), model, req, opts).Result()
	if captured == nil {
		t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	return captured
}

// anthropicNativeCapture returns one captured body.
func anthropicNativeCapture(t *testing.T, key string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(anthropicNativeCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]json.RawMessage
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", anthropicNativeCaptureFile, err)
	}
	raw, ok := capture[key]
	if !ok {
		t.Fatalf("%s has no %q entry; rerun capture-anthropic-native.mts", anthropicNativeCaptureFile, key)
	}
	return raw
}

// payloadJSON decodes a built body into plain JSON values, the shape pi's
// payload assertions read.
func payloadJSON(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// jsonItems reads a decoded JSON array of objects.
func jsonItems(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, len(list))
	for i, item := range list {
		out[i], _ = item.(map[string]any)
	}
	return out
}

func payloadBetas(body map[string]any) []string {
	var betas []string
	list, _ := body["betas"].([]any)
	for _, beta := range list {
		s, _ := beta.(string)
		betas = append(betas, s)
	}
	return betas
}

// toolReferenceName reads block.tool.name.
func toolReferenceName(block map[string]any) string {
	tool, _ := block["tool"].(map[string]any)
	name, _ := tool["name"].(string)
	return name
}

func TestSendsAnthropicUpdatesAndToolChangesInNativeSystemMessages(t *testing.T) {
	model := anthropicNativeModel()
	payload := payloadJSON(t, captureStreamSimplePayload(t, model, transcriptChangesContext()))

	if betas := payloadBetas(payload); !slices.Contains(betas, wantToolChangesBeta) {
		t.Fatalf("betas = %v, want %s", betas, wantToolChangesBeta)
	}
	var system []string
	for _, block := range jsonItems(payload["system"]) {
		text, _ := block["text"].(string)
		system = append(system, text)
	}
	if want := []string{"base prompt\n\n<rules>\nold rules\n</rules>\n\n<docs>\nread docs\n</docs>"}; !slices.Equal(system, want) {
		t.Fatalf("system = %q, want %q", system, want)
	}
	if betas := payloadBetas(payload); slices.Contains(betas, "mid-conversation-tool-changes-2026-07-01") {
		t.Fatalf("betas = %v, must not carry the retired mid-conversation-tool-changes beta", betas)
	}
	// The top-level list holds only the initial tools (with the cache breakpoint)
	// and the deferred placeholder; later tools never appear there.
	tools := jsonItems(payload["tools"])
	var names []string
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	if want := []string{"base_tool", "__pi_deferred_placeholder__"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if cc, _ := tools[0]["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("tools[0].cache_control = %v, want {type: ephemeral}", tools[0]["cache_control"])
	}
	if tools[1]["defer_loading"] != true {
		t.Fatalf("tools[1].defer_loading = %v, want true", tools[1]["defer_loading"])
	}
	if _, ok := tools[0]["defer_loading"]; ok {
		t.Fatalf("tools[0].defer_loading = %v, want undefined", tools[0]["defer_loading"])
	}
	if _, ok := tools[1]["cache_control"]; ok {
		t.Fatalf("tools[1].cache_control = %v, want undefined", tools[1]["cache_control"])
	}
	messages := jsonItems(payload["messages"])
	if len(messages) == 0 {
		t.Fatalf("messages = [], want the update as a system message")
	}
	update := messages[len(messages)-1]
	content := jsonItems(update["content"])
	if update["role"] != "system" || len(content) != 3 ||
		content[0]["type"] != "text" ||
		content[1]["type"] != "tool_removal" || toolReferenceName(content[1]) != "base_tool" ||
		content[2]["type"] != "tool_addition" {
		t.Fatalf("last message = %v, want a system message [text, tool_removal base_tool, tool_addition]", update)
	}
	if ref, _ := content[1]["tool"].(map[string]any); ref["type"] != "tool_reference" {
		t.Fatalf("tool_removal.tool = %v, want a tool_reference", content[1]["tool"])
	}
	// The addition defines late_tool by value; cache control goes on the block,
	// never on the inline definition, and nothing is deferred.
	addition, _ := content[2]["tool"].(map[string]any)
	definition, _ := addition["definition"].(map[string]any)
	if addition["type"] != "tool_definition" || definition["name"] != "late_tool" || definition["description"] != "late_tool tool" {
		t.Fatalf("tool_addition.tool = %v, want a tool_definition of late_tool", addition)
	}
	if cc, _ := content[2]["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("tool_addition.cache_control = %v, want {type: ephemeral}", content[2]["cache_control"])
	}
	for _, key := range []string{"cache_control", "defer_loading"} {
		if _, ok := definition[key]; ok {
			t.Fatalf("definition.%s = %v, want undefined", key, definition[key])
		}
	}
	text, _ := content[0]["text"].(string)
	for _, want := range []string{"updated guidance", "<rules>\nnew rules\n</rules>", `Removed system prompt section "docs"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("update text %q does not contain %q", text, want)
		}
	}

	// The placeholder is declared before any change so its scaffolding is
	// cached from request one.
	full := transcriptChangesContext()
	initial := payloadJSON(t, captureStreamSimplePayload(t, model, ai.Context{Messages: full.Messages[:2]}))
	names = nil
	for _, tool := range jsonItems(initial["tools"]) {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	if want := []string{"base_tool", "__pi_deferred_placeholder__"}; !slices.Equal(names, want) {
		t.Fatalf("initial tools = %v, want %v", names, want)
	}
}

// redefinitionContext is the suite's same-name redefinition: the update
// removes base_tool and re-adds it with a new description.
func redefinitionContext() ai.Context {
	baseTool := transcriptTool("base_tool")
	redefinedTool := baseTool
	redefinedTool.Description = "changed"
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{baseTool}
	redefinition := ai.NewSystemText("", 2)
	redefinition.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	redefinition.ToolsAdded = []ai.Tool{redefinedTool}
	return ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), redefinition}}
}

// nativeCaptureFallbackContexts are the capture's redefinition and
// fallbackNoInitialTools: the two histories that fell back at 9e05370b2, with
// no user message. Since b271b0a52 only the second still falls back.
func nativeCaptureFallbackContexts() (redefinition, noInitialTool ai.Context) {
	baseTool := transcriptTool("base_tool")
	redefinedTool := baseTool
	redefinedTool.Description = "changed"

	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{baseTool}
	update := ai.NewSystemText("updated guidance", 2)
	update.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	update.ToolsAdded = []ai.Tool{redefinedTool}

	addition := ai.NewSystemText("updated guidance", 2)
	addition.ToolsAdded = []ai.Tool{redefinedTool}
	return ai.Context{Messages: []ai.Message{leading, update}},
		ai.Context{Messages: []ai.Message{ai.NewSystemText("base prompt", 0), addition}}
}

func TestRedefinesAnAnthropicToolByValueUnderTheSameName(t *testing.T) {
	payload := payloadJSON(t, captureStreamSimplePayload(t, anthropicNativeModel(), redefinitionContext()))
	if betas := payloadBetas(payload); !slices.Contains(betas, wantToolChangesBeta) {
		t.Fatalf("betas = %v, want %s", betas, wantToolChangesBeta)
	}
	// The top-level declaration keeps the original definition, so the cached
	// prefix survives.
	tools := jsonItems(payload["tools"])
	if len(tools) != 2 || tools[0]["name"] != "base_tool" || tools[0]["description"] != "base_tool tool" ||
		tools[1]["name"] != "__pi_deferred_placeholder__" {
		t.Fatalf("tools = %v, want [base_tool (base_tool tool), __pi_deferred_placeholder__]", tools)
	}
	// The new definition replaces the old one in place; no removal block is
	// sent for it.
	messages := jsonItems(payload["messages"])
	last := messages[len(messages)-1]
	content := jsonItems(last["content"])
	addition, _ := content[0]["tool"].(map[string]any)
	definition, _ := addition["definition"].(map[string]any)
	if last["role"] != "system" || len(content) != 1 || content[0]["type"] != "tool_addition" ||
		addition["type"] != "tool_definition" || definition["name"] != "base_tool" || definition["description"] != "changed" {
		t.Fatalf("last message = %v, want a system message [tool_addition defining base_tool (changed)]", last)
	}
}

// Anthropic rejects a tool list where every tool (here: the placeholder) is
// deferred, so a history without an initial tool sends the current list.
func TestSendsTheCurrentAnthropicToolListWithoutAnInitialTool(t *testing.T) {
	leading := ai.NewSystemText("base prompt", 0)
	update := ai.NewSystemText("updated guidance", 2)
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	req := ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}

	payload := payloadJSON(t, captureStreamSimplePayload(t, anthropicNativeModel(), req))
	if betas := payloadBetas(payload); slices.Contains(betas, wantToolChangesBeta) {
		t.Fatalf("betas = %v, must not carry %s", betas, wantToolChangesBeta)
	}
	tools := jsonItems(payload["tools"])
	if len(tools) != 1 || tools[0]["name"] != "late_tool" {
		t.Fatalf("tools = %v, want [late_tool]", tools)
	}
	if cc, _ := tools[0]["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("tools[0].cache_control = %v, want {type: ephemeral}", tools[0]["cache_control"])
	}
	if _, ok := tools[0]["defer_loading"]; ok {
		t.Fatalf("tools[0].defer_loading = %v, want undefined", tools[0]["defer_loading"])
	}
	messages := jsonItems(payload["messages"])
	var kinds []string
	for _, block := range jsonItems(messages[len(messages)-1]["content"]) {
		kind, _ := block["type"].(string)
		kinds = append(kinds, kind)
	}
	if !slices.Equal(kinds, []string{"text"}) {
		t.Fatalf("last message block types = %v, want [text]", kinds)
	}
}

func TestRequiresBothAnthropicCapabilitiesForNativeToolChanges(t *testing.T) {
	model := foldModel("claude-opus-5", ai.APIAnthropicMessages, "anthropic", `{"supportsMidConvoToolChanges":true}`)
	model.Name = "Claude Opus 5"
	payload := payloadJSON(t, captureStreamSimplePayload(t, model, transcriptChangesContext()))

	if betas := payloadBetas(payload); slices.Contains(betas, wantToolChangesBeta) {
		t.Fatalf("betas = %v, must not carry %s", betas, wantToolChangesBeta)
	}
	var names []string
	for _, tool := range jsonItems(payload["tools"]) {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	if !slices.Equal(names, []string{"late_tool"}) {
		t.Fatalf("tools = %v, want [late_tool]", names)
	}
	var roles []string
	for _, message := range jsonItems(payload["messages"]) {
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	if !slices.Equal(roles, []string{"user"}) {
		t.Fatalf("message roles = %v, want [user]", roles)
	}
}

// The leading system message is the top-level `system` prompt and its tools
// are the request's initial tools; on a native model it is not sent again as a
// {role:"system"} message, which would re-add every initial tool.
func TestAnthropicNativeLeadingSystemMessageIsNotResent(t *testing.T) {
	full := transcriptChangesContext()
	payload := payloadJSON(t, captureStreamSimplePayload(t, anthropicNativeModel(), ai.Context{Messages: full.Messages[:2]}))
	var roles []string
	for _, message := range jsonItems(payload["messages"]) {
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	if !slices.Equal(roles, []string{"user"}) {
		t.Fatalf("message roles = %v, want [user]", roles)
	}
}

// anthropicNativeOAuthContext is the capture's oauthContext: Claude Code tool
// names, and an update landing between a tool call and its result.
func anthropicNativeOAuthContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("read"), transcriptTool("base_tool")}
	update := ai.NewSystemText("updated guidance", 3)
	update.ToolsRemoved = []ai.ToolReference{{Name: "read"}}
	update.ToolsAdded = []ai.Tool{transcriptTool("bash")}
	return ai.Context{Messages: []ai.Message{
		leading,
		ai.NewUserText("before", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "toolu_1", Name: "read", Arguments: map[string]any{"path": "a.txt"}}},
			Api:     ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-opus-5",
			StopReason: ai.StopToolUse, Timestamp: 2,
		},
		update,
		ai.ToolResultMessage{ToolCallID: "toolu_1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "contents"}}, Timestamp: 4},
		ai.NewUserText("after", 5),
	}}
}

// anthropicNativeEffortContext is the capture's effortContext.
func anthropicNativeEffortContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	mid := ai.NewSystemText("mid guidance", 2)
	mid.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	closing := ai.NewSystemText("closing guidance", 5)
	closing.ToolsRemoved = []ai.ToolReference{{Name: "late_tool"}}
	return ai.Context{Messages: []ai.Message{
		leading,
		ai.NewUserText("first", 1),
		mid,
		ai.AssistantMessage{
			Content: ai.ContentList{ai.TextContent{Text: "ok"}},
			Api:     ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-opus-5",
			ProviderThinkingLevel: "low", StopReason: ai.StopStop, Timestamp: 3,
		},
		ai.NewUserText("second", 4),
		closing,
	}}
}

// anthropicPlainReply is the capture's plainReply.
func anthropicPlainReply(text string, timestamp int64) ai.AssistantMessage {
	return ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: text}},
		Api:     ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-sonnet-4-5",
		StopReason: ai.StopStop, Timestamp: timestamp,
	}
}

// anthropicUserContentFormsContext is the capture's userContentForms.
func anthropicUserContentFormsContext() ai.Context {
	return ai.Context{SystemPrompt: "base prompt", Messages: []ai.Message{
		ai.NewUserText("first", 1),
		anthropicPlainReply("ok", 2),
		ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "array form"}}, Timestamp: 3},
		ai.NewUserText("   ", 4),
		anthropicPlainReply("ok again", 5),
		ai.NewUserText("last", 6),
	}}
}

// anthropicToolsOnlyUpdateContext is the capture's
// systemMessagesOnlyToolsOnlyUpdate (initial tool declared) and
// fallbackToolsOnlyUpdate (none): a tools-only update renders no text, so
// without native tool changes it converts to no blocks and is not sent.
func anthropicToolsOnlyUpdateContext(initialTool bool) ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	if initialTool {
		leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	}
	update := ai.NewSystemText("", 2)
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	return ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}
}

// anthropicAPIKeyClaudeCodeNamesContext is the capture's
// apiKeyClaudeCodeNamesContext: tool references keep their names without
// OAuth, even names Claude Code knows.
func anthropicAPIKeyClaudeCodeNamesContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("read")}
	update := ai.NewSystemText("", 2)
	update.ToolsRemoved = []ai.ToolReference{{Name: "read"}}
	update.ToolsAdded = []ai.Tool{transcriptTool("bash")}
	return ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}
}

// anthropicNativeEmptyAssistantContext is the capture's emptyAssistantContext:
// the held update is flushed on reaching an assistant message that converts to
// no blocks, so it precedes the next user message.
func anthropicNativeEmptyAssistantContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	mid := ai.NewSystemText("mid guidance", 2)
	mid.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	return ai.Context{Messages: []ai.Message{
		leading,
		ai.NewUserText("first", 1),
		mid,
		ai.AssistantMessage{
			Content: ai.ContentList{ai.TextContent{Text: "   "}},
			Api:     ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-opus-5",
			StopReason: ai.StopStop, Timestamp: 3,
		},
		ai.NewUserText("second", 4),
	}}
}

// anthropicUserStringSurrogatesContext is the capture's userStringSurrogates,
// with the WTF-8 spelling of anthropicNativeSurrogateContext: string-form user
// content goes through sanitizeSurrogates, whether it stays a string (not last)
// or becomes the cache_control block (last).
func anthropicUserStringSurrogatesContext() ai.Context {
	return ai.Context{SystemPrompt: "base prompt", Messages: []ai.Message{
		ai.NewUserText("lone \xed\xa0\xbd here", 1),
		anthropicPlainReply("ok", 2),
		ai.NewUserText("paired \xed\xa0\xbd\xed\xb9\x88 last", 3),
	}}
}

// anthropicNativeSurrogateContext is the capture's surrogateContext. A JS
// string's lone and paired UTF-16 surrogates are WTF-8 byte triples here: a lone
// high U+D83D, and U+D83D U+DE48 (an emoji once paired).
func anthropicNativeSurrogateContext() ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	update := ai.NewSystemText("lone \xed\xa0\xbd here", 2)
	update.Sections = ai.SystemSections{{Name: "rules", Value: foldStr("paired \xed\xa0\xbd\xed\xb9\x88 rules")}}
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	return ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}
}

// Every key and value of the built body equals pi's.
func TestAnthropicNativeRequestBodies(t *testing.T) {
	native := anthropicNativeModel()
	effort := anthropicNativeModel()
	effort.Compat = json.RawMessage(`{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true,"supportsMidConvoEffort":true}`)
	systemOnly := anthropicNativeModel()
	systemOnly.Compat = json.RawMessage(`{"supportsMidConvoSystemMessages":true}`)
	plain := foldModel("claude-sonnet-4-5", ai.APIAnthropicMessages, "anthropic", "")
	plain.Name = "Claude Sonnet 4.5"
	noToolCacheControl := anthropicNativeModel()
	noToolCacheControl.Compat = json.RawMessage(`{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true,"supportsCacheControlOnTools":false}`)
	configuredBetas := anthropicNativeModel()
	configuredBetas.Headers = ai.ProviderHeaders{"anthropic-beta": foldStr("x-beta, x-beta ,y-beta")}
	full := transcriptChangesContext()
	captureRedefinition, captureNoInitialTool := nativeCaptureFallbackContexts()

	cases := []struct {
		key       string
		model     *ai.Model
		req       ai.Context
		apiKey    string
		reasoning ai.ThinkingLevel
	}{
		{"native", native, full, "test-key", ""},
		{"nativeInitial", native, ai.Context{Messages: full.Messages[:2]}, "test-key", ""},
		{"nativeOAuth", native, anthropicNativeOAuthContext(), "sk-ant-oat-test", ""},
		{"nativeEffort", effort, anthropicNativeEffortContext(), "test-key", ""},
		{"nativeEffortReasoning", effort, anthropicNativeEffortContext(), "test-key", ai.ThinkingLow},
		{"systemMessagesOnly", systemOnly, full, "test-key", ""},
		{"redefinition", native, captureRedefinition, "test-key", ""},
		{"fallbackNoInitialTools", native, captureNoInitialTool, "test-key", ""},
		{"userContentForms", plain, anthropicUserContentFormsContext(), "test-key", ""},
		{"nativeSurrogates", native, anthropicNativeSurrogateContext(), "test-key", ""},
		{"systemMessagesOnlyToolsOnlyUpdate", systemOnly, anthropicToolsOnlyUpdateContext(true), "test-key", ""},
		{"fallbackToolsOnlyUpdate", native, anthropicToolsOnlyUpdateContext(false), "test-key", ""},
		{"nativeApiKeyClaudeCodeNames", native, anthropicAPIKeyClaudeCodeNamesContext(), "test-key", ""},
		{"nativeNoToolCacheControl", noToolCacheControl, full, "test-key", ""},
		{"nativeEmptyAssistant", native, anthropicNativeEmptyAssistantContext(), "test-key", ""},
		{"userStringSurrogates", plain, anthropicUserStringSurrogatesContext(), "test-key", ""},
		{"nativeConfiguredBetaHeader", configuredBetas, full, "test-key", ""},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			body := captureAnthropicPayload(t, tc.model, tc.req, tc.apiKey, tc.reasoning)
			assertRequestBody(t, body, anthropicNativeCapture(t, tc.key))
		})
	}
}

// Copilot dynamic headers read the transcript the adapter resolved: on a model
// with native system messages a trailing update stays in place, so the request
// is agent-initiated, while the same transcript on a model without them folds
// the update into the head and stays user-initiated
// (buildCopilotDynamicHeaders over normalizedContext.messages at 9e05370b2).
func TestAnthropicCopilotInitiatorReadsTheResolvedTranscript(t *testing.T) {
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1), ai.NewSystemText("later guidance", 2)}}
	for _, tc := range []struct {
		compat string
		want   string
	}{
		{`{"supportsMidConvoSystemMessages":true}`, "agent"},
		{``, "user"},
	} {
		model := foldModel("claude-test", ai.APIAnthropicMessages, "github-copilot", tc.compat)
		opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "copilot-token"}}}
		headers, _ := anthropicCapture(t, model, req, opts, anthropicSSE)
		if got := headers.Get("X-Initiator"); got != tc.want {
			t.Fatalf("compat %q: X-Initiator = %q, want %q", tc.compat, got, tc.want)
		}
	}
}
