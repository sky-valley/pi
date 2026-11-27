package providers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// openai-completions reasoning_opaque (upstream ede8bdca0, pi
// openai-completions-reasoning-opaque.test.ts). GitHub Copilot streams
// reasoning_text deltas followed by one reasoning_opaque delta; the opaque
// value is kept off the stream, stored with the reasoning field in the thinking
// block's signature as `{"field":…,"reasoning_opaque":…}`, and replayed next to
// the text. The signature is persisted in sessions, so it is asserted byte for
// byte.

var opaqueToolCallChunk = opaqueToolCallChunkAt("0", "call_1")

func opaqueToolCallChunkAt(index, id string) string {
	return `data: {"choices":[{"delta":{"tool_calls":[{"index":` + index + `,"id":"` + id + `","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"}}]}}]}

`
}

const opaqueToolCallsFinish = `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// replayStreamed feeds a streamed turn back through convertMessages for the
// model that produced it and returns the replayed assistant message.
func replayStreamed(t *testing.T, final *ai.AssistantMessage) map[string]any {
	t.Helper()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return replayAssistant(t, replayModel(), final.Content)
}

func TestOpenAIStreamReplaysReasoningOpaqueWithItsText(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"role":"assistant","reasoning_text":"Read the readme"}}]}

data: {"choices":[{"delta":{"reasoning_text":" first."}}]}

data: {"choices":[{"delta":{"role":"assistant","reasoning_opaque":"opaque-1"}}]}

` + opaqueToolCallChunk + opaqueToolCallsFinish
	final := runOpenAIStream(t, sse, nil)
	think, sig := thinkingSig(final)
	if think != "Read the readme first." {
		t.Fatalf("thinking = %q", think)
	}
	if want := `{"field":"reasoning_text","reasoning_opaque":"opaque-1"}`; sig != want {
		t.Fatalf("signature = %s, want %s", sig, want)
	}

	msg := replayStreamed(t, final)
	if msg["content"] != nil {
		t.Fatalf("content = %#v, want null", msg["content"])
	}
	if msg["reasoning_text"] != "Read the readme first." || msg["reasoning_opaque"] != "opaque-1" {
		t.Fatalf("replayed = %#v, want reasoning_text and reasoning_opaque", msg)
	}
}

// Interleaved reasoning keeps ONE thinking block: the text concatenates and the
// latest opaque value is the one replayed, as the Copilot VS Code client does.
func TestOpenAIStreamReplaysLatestReasoningOpaque(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_text":"Read the readme."}}]}

data: {"choices":[{"delta":{"reasoning_opaque":"opaque-first"}}]}

` + opaqueToolCallChunkAt("0", "call_1") + `data: {"choices":[{"delta":{"reasoning_text":" Then the manifest."}}]}

data: {"choices":[{"delta":{"reasoning_opaque":"opaque-second"}}]}

` + opaqueToolCallChunkAt("1", "call_2") + opaqueToolCallsFinish
	msg := replayStreamed(t, runOpenAIStream(t, sse, nil))
	if msg["reasoning_text"] != "Read the readme. Then the manifest." || msg["reasoning_opaque"] != "opaque-second" {
		t.Fatalf("replayed = %#v", msg)
	}
}

// An opaque value that arrives without text opens an (empty) thinking block of
// its own and replays alone: no reasoning field, since none carried text.
func TestOpenAIStreamReplaysReasoningOpaqueWithoutText(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"Checking.","reasoning_opaque":"opaque-only"}}]}

` + opaqueToolCallChunk + opaqueToolCallsFinish
	final := runOpenAIStream(t, sse, nil)
	var kinds []string
	for _, c := range final.Content {
		switch c.(type) {
		case ai.ThinkingContent:
			kinds = append(kinds, "thinking")
		case ai.TextContent:
			kinds = append(kinds, "text")
		case ai.ToolCall:
			kinds = append(kinds, "toolCall")
		}
	}
	if got := strings.Join(kinds, ","); got != "thinking,text,toolCall" {
		t.Fatalf("content kinds = %s, want thinking,text,toolCall", got)
	}
	// JSON.stringify omits the undefined field.
	if _, sig := thinkingSig(final); sig != `{"reasoning_opaque":"opaque-only"}` {
		t.Fatalf("signature = %s", sig)
	}

	msg := replayStreamed(t, final)
	if msg["content"] != "Checking." || msg["reasoning_opaque"] != "opaque-only" {
		t.Fatalf("replayed = %#v", msg)
	}
	if has(msg, "reasoning_text") {
		t.Fatalf("reasoning_text must not be replayed: %#v", msg)
	}
}

func TestOpenAIStreamReadsReasoningTextAndOpaqueFromOneChunk(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_text":"Plan.","reasoning_opaque":"opaque-1"}}]}

` + opaqueToolCallChunk + opaqueToolCallsFinish
	msg := replayStreamed(t, runOpenAIStream(t, sse, nil))
	if msg["reasoning_text"] != "Plan." || msg["reasoning_opaque"] != "opaque-1" {
		t.Fatalf("replayed = %#v", msg)
	}
}

func TestOpenAIStreamReasoningDetailsWinOverReasoningOpaque(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_text":"Plan.","reasoning_details":[` + rdEncrypted + `]}}]}

data: {"choices":[{"delta":{"reasoning_opaque":"opaque-1"}}]}

` + opaqueToolCallChunk + opaqueToolCallsFinish
	msg := replayStreamed(t, runOpenAIStream(t, sse, nil))
	if got := wireReasoningDetails(t, msg); got != "["+rdEncrypted+"]" {
		t.Fatalf("reasoning_details = %s", got)
	}
	if has(msg, "reasoning_opaque") || has(msg, "reasoning_text") {
		t.Fatalf("details must replace both the opaque value and the text: %#v", msg)
	}
}

func TestOpenAIStreamKeepsReasoningOpaqueWhenTheStreamFails(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_text":"Plan."}}]}

data: {"choices":[{"delta":{"reasoning_opaque":"opaque-1"}}]}

`
	final := runOpenAIStream(t, sse, nil)
	if final.StopReason != ai.StopError {
		t.Fatalf("stop = %s, want error", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "Stream ended without finish_reason") {
		t.Fatalf("error = %q", final.ErrorMessage)
	}
	think, sig := thinkingSig(final)
	if think != "Plan." || sig != `{"field":"reasoning_text","reasoning_opaque":"opaque-1"}` {
		t.Fatalf("thinking = %q, signature = %s", think, sig)
	}
}

// Thinking from another model is dropped by transformMessages, opaque value and
// all.
func TestOpenAIReasoningOpaqueNotReplayedToAnotherModel(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_text":"Plan."}}]}

data: {"choices":[{"delta":{"reasoning_opaque":"opaque-1"}}]}

data: {"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	model := replayModel()
	body := mustBuildOpenAIParams(t, model, ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content: final.Content, Provider: model.Provider, Api: model.Api, Model: "other-model",
			StopReason: ai.StopStop,
		},
	}}, &OpenAIOptions{})
	var msg map[string]any
	for _, m := range body["messages"].([]map[string]any) {
		if m["role"] == "assistant" {
			msg = m
		}
	}
	if msg == nil || msg["content"] != "Done." {
		t.Fatalf("replayed = %#v", msg)
	}
	if has(msg, "reasoning_opaque") || has(msg, "reasoning_text") {
		t.Fatalf("another model's reasoning must not be replayed: %#v", msg)
	}
}

// ---- stored signatures (replay of what sessions hold) ----

// opencode-go streams `reasoning` but stores and replays reasoning_content; the
// opaque signature carries the remapped name, and an older-shaped one holding
// `reasoning` is remapped on replay like the plain field name is.
func TestOpenAIReplayReasoningOpaqueOpencodeGoRemap(t *testing.T) {
	model := replayModel()
	model.Provider = "opencode-go"
	msg := replayAssistant(t, model, ai.ContentList{
		ai.ThinkingContent{Thinking: "Plan.", ThinkingSignature: `{"field":"reasoning","reasoning_opaque":"o"}`},
		ai.TextContent{Text: "Done."},
	})
	if msg["reasoning_content"] != "Plan." || msg["reasoning_opaque"] != "o" || has(msg, "reasoning") {
		t.Fatalf("replayed = %#v", msg)
	}
}

func TestOpenAIStreamReasoningOpaqueOpencodeGoField(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning":"Plan."}}]}

data: {"choices":[{"delta":{"reasoning_opaque":"o"}}]}

data: {"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, func(m *ai.Model) { m.Provider = "opencode-go" })
	if _, sig := thinkingSig(final); sig != `{"field":"reasoning_content","reasoning_opaque":"o"}` {
		t.Fatalf("signature = %s", sig)
	}
}

// The latest opaque value across blocks wins, and the field comes from the
// opaque signature rather than from the first block with text.
func TestOpenAIReplayReasoningOpaqueLatestBlockWins(t *testing.T) {
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "One.", ThinkingSignature: "reasoning_content"},
		ai.ThinkingContent{Thinking: "Two.", ThinkingSignature: `{"field":"reasoning_text","reasoning_opaque":"first"}`},
		ai.ThinkingContent{Thinking: "", ThinkingSignature: `{"field":"reasoning_text","reasoning_opaque":"second"}`},
		ai.TextContent{Text: "Done."},
	})
	if msg["reasoning_text"] != "One.\nTwo." || msg["reasoning_opaque"] != "second" || has(msg, "reasoning_content") {
		t.Fatalf("replayed = %#v", msg)
	}
}

// A legacy encrypted detail on a tool call is details replay, so it beats an
// opaque signature too.
func TestOpenAIReplayLegacyDetailsBeatReasoningOpaque(t *testing.T) {
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "Plan.", ThinkingSignature: `{"field":"reasoning_text","reasoning_opaque":"o"}`},
		ai.ToolCall{ID: "call_1", Name: "f", Arguments: map[string]any{}, ThoughtSignature: rdEncrypted},
	}, ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})
	if got := wireReasoningDetails(t, msg); got != "["+rdEncrypted+"]" {
		t.Fatalf("reasoning_details = %s", got)
	}
	if has(msg, "reasoning_opaque") || has(msg, "reasoning_text") {
		t.Fatalf("replayed = %#v", msg)
	}
}

// pi only parses a signature whose FIRST character opens the JSON form it
// stores (`[` for details, `{` for an opaque object or a legacy tool-call
// detail); one with leading whitespace is no replay data, where JSON.parse on
// its own would have accepted it.
func TestOpenAIReplaySignaturesMustStartWithTheirJSONForm(t *testing.T) {
	t.Run("details", func(t *testing.T) {
		msg := replayAssistant(t, replayModel(), ai.ContentList{
			ai.ThinkingContent{Thinking: "", ThinkingSignature: " [" + rdSummary + "]"},
			ai.TextContent{Text: "Done."},
		})
		if has(msg, "reasoning_details") {
			t.Fatalf("replayed = %#v", msg)
		}
	})
	t.Run("legacy tool call detail", func(t *testing.T) {
		msg := replayAssistant(t, replayModel(), ai.ContentList{
			ai.ToolCall{ID: "call_1", Name: "f", Arguments: map[string]any{}, ThoughtSignature: " " + rdEncrypted},
		}, ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})
		if has(msg, "reasoning_details") {
			t.Fatalf("replayed = %#v", msg)
		}
	})
}

// ---- the thinkingSignature codec (pi openai-completions-reasoning.test.ts) ----

func TestCompletionsThinkingSignatureRoundTrips(t *testing.T) {
	cases := []struct {
		name    string
		sig     completionsThinkingSignature
		encoded string
	}{
		{"field", completionsThinkingSignature{kind: signatureField, field: "reasoning_content"}, "reasoning_content"},
		{"details", completionsThinkingSignature{kind: signatureDetails, details: []json.RawMessage{json.RawMessage(rdEncrypted)}}, "[" + rdEncrypted + "]"},
		{"opaque with field", completionsThinkingSignature{kind: signatureOpaque, field: "reasoning_text", opaque: "opaque-1"}, `{"field":"reasoning_text","reasoning_opaque":"opaque-1"}`},
		{"opaque without field", completionsThinkingSignature{kind: signatureOpaque, opaque: "opaque-1"}, `{"reasoning_opaque":"opaque-1"}`},
		// JSON.stringify escaping, not encoding/json's (no < for <).
		{"opaque escaping", completionsThinkingSignature{kind: signatureOpaque, field: "reasoning", opaque: "a\"b\\<\n"}, `{"field":"reasoning","reasoning_opaque":"a\"b\\<\n"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			encoded := c.sig.encode()
			if encoded != c.encoded {
				t.Fatalf("encode = %s, want %s", encoded, c.encoded)
			}
			got, ok := decodeThinkingSignature(encoded)
			if !ok || !reflect.DeepEqual(got, c.sig) {
				t.Fatalf("decode(%s) = %#v (ok=%v), want %#v", encoded, got, ok, c.sig)
			}
		})
	}
}

func TestDecodeThinkingSignatureStoredFormats(t *testing.T) {
	if got, ok := decodeThinkingSignature("reasoning"); !ok || got.kind != signatureField || got.field != "reasoning" {
		t.Fatalf("field: %#v", got)
	}
	if got, ok := decodeThinkingSignature("[" + rdEncrypted + "]"); !ok || got.kind != signatureDetails ||
		len(got.details) != 1 || string(got.details[0]) != rdEncrypted {
		t.Fatalf("details: %#v", got)
	}
	want := completionsThinkingSignature{kind: signatureOpaque, field: "reasoning_text", opaque: "o"}
	if got, ok := decodeThinkingSignature(`{"field":"reasoning_text","reasoning_opaque":"o"}`); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("opaque: %#v", got)
	}
	// JSON.parse keeps the last of a repeated key and skips inner whitespace.
	if got, ok := decodeThinkingSignature(`{ "reasoning_opaque" : "x", "field":"reasoning_text", "reasoning_opaque":"o" }`); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("opaque, repeated key: %#v", got)
	}
}

func TestDecodeThinkingSignatureRejects(t *testing.T) {
	for _, signature := range []string{
		"",
		"anthropic-signature",
		"[]",
		"[not json",
		`[{"type":"unknown"}]`,
		`{"reasoning_opaque":""}`,
		`{"field":"content","reasoning_opaque":"o"}`,
		"{not json",
		// Beyond pi's table: a null field is not an absent one, a non-string
		// opaque value is none, and leading whitespace is not a JSON form.
		`{"field":null,"reasoning_opaque":"o"}`,
		`{"reasoning_opaque":1}`,
		`{"field":"reasoning_text"}`,
		` {"reasoning_opaque":"o"}`,
		`{"reasoning_opaque":"o"} trailing`,
	} {
		if got, ok := decodeThinkingSignature(signature); ok {
			t.Errorf("decodeThinkingSignature(%q) = %#v, want no replay data", signature, got)
		}
	}
}

func TestDecodeLegacyToolCallReasoningDetailEmptyID(t *testing.T) {
	if got, ok := decodeLegacyToolCallReasoningDetail(rdEncrypted); !ok || string(got) != rdEncrypted {
		t.Fatalf("got %s (ok=%v)", got, ok)
	}
	if got, ok := decodeLegacyToolCallReasoningDetail(`{"type":"reasoning.encrypted","id":"","data":"ENC"}`); ok {
		t.Fatalf("empty id accepted: %s", got)
	}
}
