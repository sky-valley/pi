package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// openai-completions models with compat.supportsMidConvoSystemMessages take
// later system messages in place, and with supportsMidConvoToolAdditions too
// they anchor additive tool declarations in Kimi's tool-bearing system messages
// (upstream 9e05370b2). The two Kimi cases are transliterated from
// packages/ai/test/transcript-tool-changes.test.ts at the sha, same fixture and
// expected values, through ai.StreamSimple as the suite goes through compat.ts
// streamSimple. Every body is also compared whole to pi's, captured under node
// at 95fbc0499 by testdata/transcript/capture-completions-native.mts.

const completionsNativeCaptureFile = "testdata/transcript/completions-native-95fbc0499.json"

// completionsNativeCase is one captured case: pi's model, its raw context, and
// the body pi built from them — or, for a stream that failed before building
// one, how it ended; or, for a stream answered with SSE, the response and the
// content pi parsed from it.
type completionsNativeCase struct {
	Model        json.RawMessage `json:"model"`
	Context      json.RawMessage `json:"context"`
	Body         json.RawMessage `json:"body"`
	SSE          string          `json:"sse"`
	Content      json.RawMessage `json:"content"`
	StopReason   ai.StopReason   `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

func completionsNativeCapture(t *testing.T, name string) completionsNativeCase {
	t.Helper()
	data, err := os.ReadFile(completionsNativeCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]json.RawMessage
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", completionsNativeCaptureFile, err)
	}
	raw, ok := capture[name]
	if !ok {
		t.Fatalf("%s has no %q entry; rerun capture-completions-native.mts", completionsNativeCaptureFile, name)
	}
	var c completionsNativeCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s %q: %v", completionsNativeCaptureFile, name, err)
	}
	return c
}

// decode returns the case's model and raw context as the Go types.
func (c completionsNativeCase) decode(t *testing.T) (*ai.Model, ai.Context) {
	t.Helper()
	var model ai.Model
	if err := json.Unmarshal(c.Model, &model); err != nil {
		t.Fatalf("model: %v", err)
	}
	var raw struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(c.Context, &raw); err != nil {
		t.Fatalf("context: %v", err)
	}
	var req ai.Context
	for i, m := range raw.Messages {
		message, err := ai.UnmarshalMessage(m)
		if err != nil {
			t.Fatalf("context.messages[%d]: %v", i, err)
		}
		req.Messages = append(req.Messages, message)
	}
	return &model, req
}

// completionsPayload reads the parts of a chat-completions body the suite
// inspects. A nil Tools pointer is a message without the key.
type completionsPayload struct {
	Tools    []completionsPayloadTool `json:"tools"`
	Messages []struct {
		Role    string                    `json:"role"`
		Content *string                   `json:"content"`
		Tools   *[]completionsPayloadTool `json:"tools"`
	} `json:"messages"`
}

type completionsPayloadTool struct {
	Function *struct {
		Name string `json:"name"`
	} `json:"function"`
}

func decodeCompletionsPayload(t *testing.T, body map[string]any) completionsPayload {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var payload completionsPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("payload %s: %v", data, err)
	}
	return payload
}

// functionNames is `tools.map((value) => value.function?.name)`.
func functionNames(tools []completionsPayloadTool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		if tool.Function != nil {
			names[i] = tool.Function.Name
		}
	}
	return names
}

// kimiModel is the suite's Kimi model: modelBase with a moonshotai provider.
func kimiModel(id, name, compat string) *ai.Model {
	model := foldModel(id, ai.APIOpenAICompletions, "moonshotai", compat)
	model.Name = name
	return model
}

func TestAnchorsKimiAdditionsInToolBearingSystemMessages(t *testing.T) {
	model := kimiModel("kimi-k3", "Kimi K3", `{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolAdditions":true}`)
	body := captureStreamSimplePayload(t, model, transcriptAdditionContext())
	payload := decodeCompletionsPayload(t, body)

	if got := functionNames(payload.Tools); !slices.Equal(got, []string{"base_tool"}) {
		t.Fatalf("tools = %v, want [base_tool]", got)
	}
	var anchored []string
	for _, message := range payload.Messages {
		if message.Tools != nil {
			anchored = functionNames(*message.Tools)
			break
		}
	}
	if !slices.Equal(anchored, []string{"late_tool"}) {
		t.Fatalf("tool-bearing message tools = %v, want [late_tool]", anchored)
	}
	var systemContents []*string
	for _, message := range payload.Messages {
		if message.Role == "system" {
			systemContents = append(systemContents, message.Content)
		}
	}
	if len(systemContents) != 3 || systemContents[0] == nil || *systemContents[0] != "base prompt" ||
		systemContents[1] != nil || systemContents[2] == nil || *systemContents[2] != "updated guidance" {
		t.Fatalf("system message contents = %s, want [\"base prompt\", undefined, \"updated guidance\"]", mustJSON(t, systemContents))
	}

	assertRequestBody(t, body, completionsNativeCapture(t, "kimi-k3-anchors-additions").Body)
}

func TestKeepsKimiK2SystemTextInlineWithoutDynamicToolMessages(t *testing.T) {
	model := kimiModel("kimi-k2.7-code", "Kimi K2.7 Code", `{"supportsMidConvoSystemMessages":true}`)
	body := captureStreamSimplePayload(t, model, transcriptAdditionContext())
	payload := decodeCompletionsPayload(t, body)

	if got := functionNames(payload.Tools); !slices.Equal(got, []string{"base_tool", "late_tool"}) {
		t.Fatalf("tools = %v, want [base_tool late_tool]", got)
	}
	var systemContents []string
	for _, message := range payload.Messages {
		if message.Tools != nil {
			t.Fatalf("message %+v carries tools; no message may", message)
		}
		if message.Role == "system" {
			if message.Content == nil {
				t.Fatalf("system message without content: %+v", message)
			}
			systemContents = append(systemContents, *message.Content)
		}
	}
	if !slices.Equal(systemContents, []string{"base prompt", "updated guidance"}) {
		t.Fatalf("system message contents = %q, want [base prompt, updated guidance]", systemContents)
	}

	assertRequestBody(t, body, completionsNativeCapture(t, "kimi-k2-text-inline").Body)
}

// a3f265b1c moves kimi-coding onto Chat Completions at /coding/v1 (the 1.3.0
// regen carries it). Its two cases in transcript-tool-changes.test.ts read the
// catalog models, so these do too, through ai.StreamSimple as the suite goes
// through compat.ts streamSimple. "anchors additions for Kimi Coding %s": K3,
// K3-256K and Kimi For Coding take added tools in a tool-bearing system message
// and keep them out of the top-level list.
func TestAnchorsKimiCodingAdditions(t *testing.T) {
	for _, id := range []string{"k3", "k3-256k", "kimi-for-coding"} {
		t.Run(id, func(t *testing.T) {
			model := ai.GetModel("kimi-coding", id)
			if model == nil {
				t.Fatalf("catalog has no kimi-coding/%s", id)
			}
			payload := decodeCompletionsPayload(t, captureStreamSimplePayload(t, model, transcriptAdditionContext()))
			if got := functionNames(payload.Tools); !slices.Equal(got, []string{"base_tool"}) {
				t.Fatalf("tools = %v, want [base_tool]", got)
			}
			var anchored []string
			for _, message := range payload.Messages {
				if message.Tools != nil {
					anchored = functionNames(*message.Tools)
					break
				}
			}
			if !slices.Equal(anchored, []string{"late_tool"}) {
				t.Fatalf("tool-bearing message tools = %v, want [late_tool]", anchored)
			}
		})
	}
}

// "keeps Kimi Coding Highspeed additions in top-level tools": Kimi Code
// Highspeed rejects tool-bearing system messages.
func TestKeepsKimiCodingHighspeedAdditionsTopLevel(t *testing.T) {
	model := ai.GetModel("kimi-coding", "kimi-for-coding-highspeed")
	if model == nil {
		t.Fatal("catalog has no kimi-coding/kimi-for-coding-highspeed")
	}
	payload := decodeCompletionsPayload(t, captureStreamSimplePayload(t, model, transcriptAdditionContext()))
	if got := functionNames(payload.Tools); !slices.Equal(got, []string{"base_tool", "late_tool"}) {
		t.Fatalf("tools = %v, want [base_tool late_tool]", got)
	}
	for _, message := range payload.Messages {
		if message.Tools != nil {
			t.Fatalf("message %+v carries tools; no message may", message)
		}
	}
}

// Copilot dynamic headers read the transcript as the stream resolved it: kept
// in place, a trailing system message is the last message, so the request is
// agent-initiated (the collapsed counterpart is
// TestCopilotInitiatorReadsTheCollapsedTranscript). pi at 9e05370b2 under node,
// same model and messages: X-Initiator "agent", messages [user "hi", system
// "later guidance"].
func TestCopilotInitiatorReadsTheInPlaceTranscript(t *testing.T) {
	var initiator string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initiator = r.Header.Get("X-Initiator")
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, openAISSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: "gpt-5", Api: ai.APIOpenAICompletions, Provider: "github-copilot", BaseURL: server.URL, Input: []string{"text"},
		Compat: json.RawMessage(`{"supportsMidConvoSystemMessages":true}`),
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1), ai.NewSystemText("later guidance", 2)}}
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "k"
	ai.StreamSimple(context.Background(), model, req, opts).Result()
	if initiator != "agent" {
		t.Fatalf("X-Initiator = %q, want agent (the trailing system message stays last)", initiator)
	}
	var posted struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &posted); err != nil {
		t.Fatalf("posted body %q: %v", body, err)
	}
	if got := mustJSON(t, posted.Messages); got != `[{"content":"hi","role":"user"},{"content":"later guidance","role":"system"}]` {
		t.Fatalf("messages = %s", got)
	}
}

// The cases the suite leaves open, each compared whole to pi's body:
//   - developer-updates-non-additive: with reasoning and supportsDeveloperRole
//     later messages are developer messages too; the index-0 message renders
//     its full text and a later one the framed update; a removal makes the
//     history non-additive, so nothing anchors and the current tools are sent.
//   - kimi-k3-addition-without-leading-message: "later" is the position in the
//     transformed transcript, so a system message at index 1 anchors its
//     additions and renders as an update; its empty text sends no message, and
//     with no leading message there are no request tools.
//   - bridge-after-tool-result / no-bridge-after-(empty-)system-update: a
//     system message between tool results and a user message sets lastRole to
//     "system" (pi's branch falls through to `lastRole = msg.role`), so
//     requiresAssistantAfterToolResult inserts no bridge, even when the system
//     message sends nothing.
//   - grammar-call-replays-declared-tool / grammar-call-collapsed-current-tools:
//     grammar tool calls replay against the tools the resolved transcript
//     declared, so a grammar tool removed later still replays as a custom call
//     in place, and as a function call once the transcript is collapsed.
//   - (kimi-k3-)update-after-empty-leading-message: "later" is the index in the
//     transformed transcript, not the count of messages pushed: after a
//     tools-only leading message that sends nothing, the next system message
//     still anchors its additions and renders as an update.
//   - developer-updates-anchor-additions / anchored-grammar-tool-and-later-additions:
//     the Kimi tools message keeps role "system" beside developer update text,
//     and converts its tools with the provider's compat (strict, grammar tools).
//   - cache-control-first-instruction-is-kimi-tools-message: cache control marks
//     only the first system message, even a content-less Kimi tools message.
//   - kimi-k3-redeclaration-is-non-additive: redeclaring a leading tool anchors
//     nothing and sends the redefined current tools.
//   - grammar-call-additions-flag-alone-collapses: supportsMidConvoToolAdditions
//     without supportsMidConvoSystemMessages collapses the transcript, so a
//     removed grammar tool is no longer declared.
//   - whitespace-only-system-update: whitespace is text; the update is sent.
func TestCompletionsMidConversationSystemMessageRequestBodies(t *testing.T) {
	for _, name := range []string{
		"developer-updates-non-additive",
		"kimi-k3-addition-without-leading-message",
		"bridge-after-tool-result",
		"no-bridge-after-system-update",
		"no-bridge-after-empty-system-update",
		"grammar-call-replays-declared-tool",
		"grammar-call-collapsed-current-tools",
		"kimi-k3-update-after-empty-leading-message",
		"update-after-empty-leading-message",
		"developer-updates-anchor-additions",
		"anchored-grammar-tool-and-later-additions",
		"cache-control-first-instruction-is-kimi-tools-message",
		"kimi-k3-redeclaration-is-non-additive",
		"grammar-call-additions-flag-alone-collapses",
		"whitespace-only-system-update",
	} {
		t.Run(name, func(t *testing.T) {
			c := completionsNativeCapture(t, name)
			model, req := c.decode(t)
			assertRequestBody(t, captureStreamSimplePayload(t, model, req), c.Body)
		})
	}
}

// The stream parses a custom tool call against the tools the resolved transcript
// declared, as the replay does: a grammar tool removed in place still maps the
// streamed input to its own property rather than the "input" fallback.
func TestCompletionsStreamParsesCustomCallsAgainstDeclaredTools(t *testing.T) {
	c := completionsNativeCapture(t, "streamed-custom-call-to-removed-grammar-tool")
	model, req := c.decode(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, c.SSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "test-key"
	final := ai.StreamSimple(context.Background(), model, req, opts).Result()
	if final.StopReason != c.StopReason || final.ErrorMessage != c.ErrorMessage {
		t.Fatalf("stream ended %s %q, want %s %q", final.StopReason, final.ErrorMessage, c.StopReason, c.ErrorMessage)
	}
	content, err := json.Marshal(final.Content)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c.Content, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("content differs from pi's\n got %s\nwant %s", content, mustJSON(t, want))
	}
}

// A Kimi-anchored addition is converted like a request tool: a tool the
// provider cannot honor (moonshot has no strict mode, the tool requires it)
// fails the stream with pi's message, although every request tool converts.
func TestKimiAnchoredToolConversionFailureFailsTheStream(t *testing.T) {
	c := completionsNativeCapture(t, "kimi-k3-anchored-tool-conversion-fails")
	model, req := c.decode(t)
	var built bool
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "test-key"
	opts.OnPayload = func(any, *ai.Model) (any, error) {
		built = true
		return nil, errors.New("payload captured")
	}
	final := ai.StreamSimple(context.Background(), model, req, opts).Result()
	if built {
		t.Fatal("a request body was built; pi fails before building one")
	}
	if final.StopReason != c.StopReason || final.ErrorMessage != c.ErrorMessage {
		t.Fatalf("stream ended %s %q, want %s %q", final.StopReason, final.ErrorMessage, c.StopReason, c.ErrorMessage)
	}
}
