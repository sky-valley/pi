package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// A context that declares tools but no system prompt normalizes to a leading
// {role:"system", content:"", toolsAdded} message. Every adapter sends the tools
// and nothing for the empty prompt text: no empty system/developer message, no
// empty system block, no empty systemInstruction. The expected bodies are pi's,
// captured under node at upstream 95fbc0499 by
// testdata/transcript/capture-tools-only.mts (fixture and models from
// packages/ai/test/transcript-tool-changes.test.ts).

const toolsOnlyCaptureFile = "testdata/transcript/tools-only-95fbc0499.json"

// toolsOnlyContext is the capture's context: tools, no systemPrompt.
func toolsOnlyContext() ai.Context {
	return ai.Context{Tools: []ai.Tool{transcriptTool("base_tool")}, Messages: []ai.Message{ai.NewUserText("before", 1)}}
}

// toolsOnlyCapture returns one captured entry.
func toolsOnlyCapture(t *testing.T, key string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(toolsOnlyCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]json.RawMessage
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", toolsOnlyCaptureFile, err)
	}
	raw, ok := capture[key]
	if !ok {
		t.Fatalf("%s has no %q entry; rerun capture-tools-only.mts", toolsOnlyCaptureFile, key)
	}
	return raw
}

// assertRequestBody compares a built body to pi's as JSON values: every key and
// value, at every depth.
func assertRequestBody(t *testing.T, body map[string]any, want json.RawMessage) {
	t.Helper()
	gotJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var got, expected any
	if err := json.Unmarshal(gotJSON, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		wantJSON, _ := json.Marshal(expected)
		t.Fatalf("request body differs from pi's\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// The leading message's bytes, key order included, as pi-messages posts them.
func TestToolsOnlyContextNormalizesToAnEmptyLeadingSystemMessage(t *testing.T) {
	got, err := json.Marshal(ai.NormalizeContext(toolsOnlyContext()))
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, toolsOnlyCapture(t, "context")); err != nil {
		t.Fatal(err)
	}
	if string(got) != want.String() {
		t.Fatalf("normalized context\n got %s\nwant %s", got, want.String())
	}
}

func TestToolsOnlyContextAnthropicRequestBody(t *testing.T) {
	model := foldModel("claude-sonnet-4-5", ai.APIAnthropicMessages, "anthropic", "")
	model.Name = "Claude Sonnet 4.5"
	body := captureStreamSimplePayload(t, model, toolsOnlyContext())
	assertRequestBody(t, body, toolsOnlyCapture(t, "anthropic-messages"))
}

// Under OAuth the Claude Code identity block is the whole system prompt.
func TestToolsOnlyContextAnthropicOAuthRequestBody(t *testing.T) {
	model := foldModel("claude-sonnet-4-5", ai.APIAnthropicMessages, "anthropic", "")
	model.Name = "Claude Sonnet 4.5"
	var captured map[string]any
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "sk-ant-oat-test"
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		captured, _ = payload.(map[string]any)
		return nil, errors.New("payload captured")
	}
	final := ai.StreamSimple(context.Background(), model, toolsOnlyContext(), opts).Result()
	if captured == nil {
		t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	assertRequestBody(t, captured, toolsOnlyCapture(t, "anthropic-messages-oauth"))
}

func TestToolsOnlyContextOpenAIResponsesRequestBody(t *testing.T) {
	model := foldModel("gpt-4.1", ai.APIOpenAIResponses, "openai", `{"supportsAdditionalTools":true}`)
	model.Name = "GPT-4.1"
	body := captureStreamSimplePayload(t, model, toolsOnlyContext())
	assertRequestBody(t, body, toolsOnlyCapture(t, "openai-responses"))
}

func TestToolsOnlyContextOpenAICompletionsRequestBody(t *testing.T) {
	model := foldModel("custom-model", ai.APIOpenAICompletions, "custom-provider", "")
	model.Name = "Custom model"
	model.Reasoning = false
	body := captureStreamSimplePayload(t, model, toolsOnlyContext())
	assertRequestBody(t, body, toolsOnlyCapture(t, "openai-completions"))
}

// pi's google onPayload sees the @google/genai call params, so the capture is
// the REST body the SDK POSTs; the Go adapter's OnPayload body is that REST body.
func TestToolsOnlyContextGoogleRequestBody(t *testing.T) {
	model := foldModel("gemini-2.5-flash", ai.APIGoogleGenerativeAI, "google", "")
	model.Reasoning = false
	body := captureStreamSimplePayload(t, model, toolsOnlyContext())
	assertRequestBody(t, body, toolsOnlyCapture(t, "google-generative-ai"))
}

// faux.ts messageToText drops empty parts (`.filter(part => part.length > 0)`),
// so an empty prompt contributes no line before the tool declarations.
func TestFauxToolsOnlySystemMessageDropsEmptyText(t *testing.T) {
	var want string
	if err := json.Unmarshal(toolsOnlyCapture(t, "fauxSerializedContext"), &want); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(promptTexts(ai.NormalizeContext(toolsOnlyContext())), "\n\n"); got != want {
		t.Fatalf("joined prompt\n got %q\nwant %q", got, want)
	}
}
