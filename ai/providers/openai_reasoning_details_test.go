package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// openai-completions reasoning_details (upstream 4ca636c5e + b7bb00b93).
//
// The sequence a provider streams in `choice.delta.reasoning_details` is
// accumulated into the THINKING block's signature and replayed verbatim on the
// next same-shaped turn. b7bb00b93 replaced 4ca636c5e's per-tool-call
// attachment wholesale, so these tests pin the net state: the validator's
// accept/reject set, arrival order, array-over-legacy precedence, suppression of
// the raw reasoning field, and the three-name field allowlist.

// ---- the validator ----

// The object guard is load-bearing on its own: `typeof null === "object"` and
// `typeof [] === "object"` are the JS traps pi rejects explicitly, and
// json.Unmarshal of `null` into a map succeeds with a nil map rather than
// erroring, so decoding alone would let it through.
func TestReasoningDetailFieldsRejectsNonObjects(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `[{"type":"reasoning.text","text":"t"}]`, `"s"`, `1`, `true`, ``} {
		if _, ok := reasoningDetailFields(json.RawMessage(raw)); ok {
			t.Fatalf("reasoningDetailFields(%s) accepted a non-object", raw)
		}
	}
	if _, ok := reasoningDetailFields(json.RawMessage(`{"a":1}`)); !ok {
		t.Fatalf("reasoningDetailFields rejected an object")
	}
}

func TestIsOpenAIReasoningDetail(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		// The three accepted shapes, at their minimum.
		{"summary", `{"type":"reasoning.summary","summary":"s"}`, true},
		{"summary empty string", `{"type":"reasoning.summary","summary":""}`, true},
		// b7bb00b93 dropped `id` from the encrypted shape: data alone qualifies.
		{"encrypted without id", `{"type":"reasoning.encrypted","data":"d"}`, true},
		{"encrypted empty data", `{"type":"reasoning.encrypted","data":""}`, true},
		{"text", `{"type":"reasoning.text","text":"t"}`, true},
		{"text signature string", `{"type":"reasoning.text","text":"t","signature":"sig"}`, true},
		{"text signature null", `{"type":"reasoning.text","text":"t","signature":null}`, true},

		// Common fields.
		{"id null", `{"type":"reasoning.text","text":"t","id":null}`, true},
		{"id string", `{"type":"reasoning.text","text":"t","id":"i"}`, true},
		{"format string", `{"type":"reasoning.text","text":"t","format":"anthropic-claude-v1"}`, true},
		{"index number", `{"type":"reasoning.text","text":"t","index":0}`, true},
		{"index fractional", `{"type":"reasoning.text","text":"t","index":1.5}`, true},
		// JSON.parse saturates an out-of-range magnitude to ±Infinity, which is
		// still `typeof "number"`. Decoding into a float64 would reject it.
		{"index overflows float64", `{"type":"reasoning.text","text":"t","index":1e400}`, true},
		{"index underflows float64", `{"type":"reasoning.text","text":"t","index":1e-400}`, true},
		{"index huge integer", `{"type":"reasoning.text","text":"t","index":123456789012345678901234567890}`, true},
		{"unknown fields ignored", `{"type":"reasoning.summary","summary":"s","extra":{"a":[1,2]}}`, true},
		{"id number", `{"type":"reasoning.text","text":"t","id":5}`, false},
		{"id bool", `{"type":"reasoning.text","text":"t","id":true}`, false},
		{"format null", `{"type":"reasoning.text","text":"t","format":null}`, false},
		{"format number", `{"type":"reasoning.text","text":"t","format":5}`, false},
		{"index string", `{"type":"reasoning.text","text":"t","index":"0"}`, false},
		{"index null", `{"type":"reasoning.text","text":"t","index":null}`, false},

		// Wrong or missing type.
		{"no type", `{"summary":"s"}`, false},
		{"unknown type", `{"type":"reasoning","summary":"s"}`, false},
		{"non-string type", `{"type":123,"summary":"s"}`, false},

		// Payload field missing or mistyped.
		{"summary missing", `{"type":"reasoning.summary"}`, false},
		{"summary number", `{"type":"reasoning.summary","summary":123}`, false},
		{"summary null", `{"type":"reasoning.summary","summary":null}`, false},
		{"data missing", `{"type":"reasoning.encrypted","id":"i"}`, false},
		{"data number", `{"type":"reasoning.encrypted","id":"i","data":123}`, false},
		{"data object", `{"type":"reasoning.encrypted","id":"i","data":{"k":"v"}}`, false},
		{"text missing", `{"type":"reasoning.text"}`, false},
		{"text number", `{"type":"reasoning.text","text":123}`, false},
		{"text signature number", `{"type":"reasoning.text","text":"t","signature":5}`, false},

		// Not an object at all. `typeof null === "object"` and
		// `typeof [] === "object"` in JS, so both need their own rejection.
		{"null", `null`, false},
		{"array", `[]`, false},
		{"array of details", `[{"type":"reasoning.text","text":"t"}]`, false},
		{"string", `"reasoning.text"`, false},
		{"number", `12`, false},
		{"bool", `true`, false},
		{"malformed", `{`, false},
		{"trailing garbage", `{"type":"reasoning.text","text":"t"}x`, false},
		{"empty", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOpenAIReasoningDetail(json.RawMessage(c.raw)); got != c.want {
				t.Fatalf("isOpenAIReasoningDetail(%s) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// ---- the signature <-> sequence codec ----

func TestParseOpenAIReasoningDetails(t *testing.T) {
	detail := `{"type":"reasoning.text","text":"a"}`
	other := `{"type":"reasoning.summary","summary":"b"}`

	notSequences := []struct {
		name      string
		signature string
	}{
		{"empty", ""},
		// The signatures every other producer writes must not read as a sequence.
		{"reasoning field name", "reasoning"},
		{"responses item id", "rs_abc123"},
		{"base64 blob", "RXJhc3VyZUNvZGVk"},
		{"empty array", "[]"},
		{"null", "null"},
		{"object", detail},
		{"array with an invalid entry", `[` + detail + `,{"type":"reasoning.summary"}]`},
		{"nested array", `[[` + detail + `]]`},
		{"malformed", `[` + detail},
	}
	for _, c := range notSequences {
		t.Run(c.name, func(t *testing.T) {
			if got := parseOpenAIReasoningDetails(c.signature); got != nil {
				t.Fatalf("parseOpenAIReasoningDetails(%q) = %s, want nil", c.signature, got)
			}
		})
	}

	t.Run("round trip preserves order", func(t *testing.T) {
		signature := "[" + detail + "," + other + "]"
		got := parseOpenAIReasoningDetails(signature)
		if len(got) != 2 || string(got[0]) != detail || string(got[1]) != other {
			t.Fatalf("parsed = %v, want [%s %s]", got, detail, other)
		}
		if round := marshalOpenAIReasoningDetails(got); round != signature {
			t.Fatalf("re-serialized = %s, want %s", round, signature)
		}
	})

	t.Run("whitespace is normalized away", func(t *testing.T) {
		got := parseOpenAIReasoningDetails(`[ { "type" : "reasoning.text" , "text" : "a" } ]`)
		if round := marshalOpenAIReasoningDetails(got); round != "["+detail+"]" {
			t.Fatalf("re-serialized = %s, want %s", round, "["+detail+"]")
		}
	})
}

// A detail is replayed as the object pi parsed, re-serialized — NOT as the
// bytes the provider sent. pi never holds those bytes: it reads the detail out
// of a JSON.parse'd SSE line and the SDK's JSON.stringify writes it back, so
// every normalization JSON.stringify applies is part of the wire format.
//
// Want values taken from pi, not from Go: each input was run through
// `JSON.stringify(JSON.parse(input))` in node.
func TestReasoningDetailsRenderedAsPiRoundTripsThem(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"redundant number forms collapse",
			`{"type":"reasoning.text","text":"t","index":1.0}`,
			`{"type":"reasoning.text","text":"t","index":1}`,
		},
		{
			"exponent notation is evaluated",
			`{"type":"reasoning.text","text":"t","index":1e2}`,
			`{"type":"reasoning.text","text":"t","index":100}`,
		},
		{
			"negative zero loses its sign",
			`{"type":"reasoning.text","text":"t","index":-0}`,
			`{"type":"reasoning.text","text":"t","index":0}`,
		},
		{
			// The magnitude survives isOpenAIReasoningDetail as Infinity, and
			// JSON.stringify(Infinity) is `null`.
			"a magnitude float64 cannot hold becomes null",
			`{"type":"reasoning.text","text":"t","index":1e400}`,
			`{"type":"reasoning.text","text":"t","index":null}`,
		},
		{
			"an integer past 2^53 takes its float64 value",
			`{"type":"reasoning.text","text":"t","index":12345678901234567890}`,
			`{"type":"reasoning.text","text":"t","index":12345678901234567000}`,
		},
		{
			// Providers that serialize with Python's ensure_ascii default send
			// every non-ASCII character escaped; JSON.stringify writes them out.
			"escaped non-ASCII is written literally",
			`{"type":"reasoning.text","text":"caf\u00e9 \u4f60\u597d"}`,
			`{"type":"reasoning.text","text":"café 你好"}`,
		},
		{
			"a needlessly escaped solidus loses its backslash",
			`{"type":"reasoning.text","text":"a\/b"}`,
			`{"type":"reasoning.text","text":"a/b"}`,
		},
		{
			"a repeated key is one property holding the last value",
			`{"type":"reasoning.text","text":"a","text":"b"}`,
			`{"type":"reasoning.text","text":"b"}`,
		},
		{
			// OrdinaryOwnPropertyKeys lists array-index keys first, ascending.
			"an integer-like key is hoisted to the front",
			`{"type":"reasoning.summary","summary":"s","0":"first","z":1}`,
			`{"0":"first","type":"reasoning.summary","summary":"s","z":1}`,
		},
		{
			"hoisted keys sort numerically, not lexicographically",
			`{"type":"reasoning.summary","summary":"s","2":"c","10":"j","1":"b"}`,
			`{"1":"b","2":"c","10":"j","type":"reasoning.summary","summary":"s"}`,
		},
		{
			"unknown members keep their place, their nesting and their order",
			rdSummary,
			rdSummary,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Through the stream path...
			sse := `data: {"choices":[{"delta":{"reasoning_details":[` + c.raw + `]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
			if _, sig := thinkingSig(runOpenAIStream(t, sse, nil)); sig != "["+c.want+"]" {
				t.Fatalf("streamed signature =\n%s\nwant\n%s", sig, "["+c.want+"]")
			}
			// ...and through the replay path, which re-renders a stored sequence
			// rather than trusting whatever wrote it.
			msg := replayAssistant(t, replayModel(), ai.ContentList{
				ai.ThinkingContent{Thinking: "", ThinkingSignature: "[" + c.raw + "]"},
				ai.TextContent{Text: "done"},
			})
			if got := wireReasoningDetails(t, msg); got != "["+c.want+"]" {
				t.Fatalf("replayed reasoning_details =\n%s\nwant\n%s", got, "["+c.want+"]")
			}
		})
	}
}

func TestParseLegacyEncryptedReasoningDetail(t *testing.T) {
	// The legacy slot is far narrower than "anything that parses", which is what
	// the pre-b7bb00b93 code replayed out of a tool call's thoughtSignature.
	rejected := []struct {
		name      string
		signature string
	}{
		{"empty", ""},
		{"number", `123`},
		{"string", `"opaque"`},
		{"array", `[{"type":"reasoning.encrypted","id":"i","data":"d"}]`},
		{"wrong type", `{"type":"reasoning.text","text":"t","id":"i"}`},
		{"missing id", `{"type":"reasoning.encrypted","data":"d"}`},
		{"null id", `{"type":"reasoning.encrypted","id":null,"data":"d"}`},
		{"empty id", `{"type":"reasoning.encrypted","id":"","data":"d"}`},
		{"empty data", `{"type":"reasoning.encrypted","id":"i","data":""}`},
		{"malformed", `{"type":"reasoning.encrypted"`},
		// The legacy slot is screened by the full validator first, so the common
		// fields still have to hold.
		{"bad index", `{"type":"reasoning.encrypted","id":"i","data":"d","index":"0"}`},
		{"bad format", `{"type":"reasoning.encrypted","id":"i","data":"d","format":null}`},
	}
	for _, c := range rejected {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := parseLegacyEncryptedReasoningDetail(c.signature); ok {
				t.Fatalf("parseLegacyEncryptedReasoningDetail(%q) = %s, want rejected", c.signature, got)
			}
		})
	}

	t.Run("accepted", func(t *testing.T) {
		signature := `{"type":"reasoning.encrypted","id":"i","data":"d","format":"f"}`
		got, ok := parseLegacyEncryptedReasoningDetail(signature)
		if !ok || string(got) != signature {
			t.Fatalf("got %s (ok=%v), want %s", got, ok, signature)
		}
	})
}

// ---- streaming ----

// rdSummary carries a member pi has no name for, nested values included: the
// detail base type is `Record<string, JsonValue>`, so an unknown key is part of
// the sequence OpenRouter wants back. Every byte assertion below compares
// against these constants, so the unknown key rides through capture, storage
// and replay under assertion rather than merely surviving the validator.
const (
	rdText      = `{"type":"reasoning.text","text":"I should read the file.","signature":"sha256:signed","id":"rt-1","format":"anthropic-claude-v1","index":0}`
	rdEncrypted = `{"type":"reasoning.encrypted","id":"call_1","data":"ENC"}`
	rdSummary   = `{"type":"reasoning.summary","summary":"Decided to read it.","provider_specific":{"nested":[1,2,{"deep":true}]},"index":1}`
)

// OpenRouter requires the sequence back unmodified and IN ORDER, so arrival
// order across deltas — and within one delta's array — is the contract.
func TestOpenAIStreamReasoningDetailsAccumulateInOrder(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning":"I should read the file.","reasoning_details":[` + rdText + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + rdEncrypted + `,` + rdSummary + `]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	events, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopStop {
		t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	think, sig := thinkingSig(final)
	if think != "I should read the file." {
		t.Fatalf("thinking text = %q", think)
	}
	want := "[" + rdText + "," + rdEncrypted + "," + rdSummary + "]"
	if sig != want {
		t.Fatalf("thinking signature =\n%s\nwant\n%s", sig, want)
	}

	// thinking_end is where the sequence reaches the block (upstream 7aab6c26e),
	// so its Partial is the first one that carries the whole thing — including
	// the second delta's two details, which pushed no event of their own.
	var sawEnd bool
	for _, e := range events {
		if e.Type != ai.EventThinkingEnd {
			continue
		}
		sawEnd = true
		if _, partialSig := thinkingSig(e.Partial); partialSig != want {
			t.Fatalf("thinking_end Partial signature =\n%s\nwant\n%s", partialSig, want)
		}
	}
	if !sawEnd {
		t.Fatalf("no thinking_end event: %v", eventTypes(events))
	}
}

// The sequence is serialized into the signature ONCE, when the thinking block
// is finalized — never per delta (upstream 7aab6c26e). That is observable, not
// an implementation detail: pi's `partial` IS the live output object, so while
// pi assigned the signature on every arriving detail a consumer watching the
// stream saw it grow; now nothing published before thinking_end carries it at
// all. Go's Clone-per-event partials have to show the same thing, or the port
// leaks replay metadata into the UI earlier than pi does.
func TestOpenAIStreamReasoningDetailsSignedOnceAtBlockEnd(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + rdSummary + `]}}]}

data: {"choices":[{"delta":{"content":"hello"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	events, final := collectOpenAIEvents(t, sse, nil)
	want := "[" + rdSummary + "]"
	if _, sig := thinkingSig(final); sig != want {
		t.Fatalf("final signature =\n%s\nwant\n%s", sig, want)
	}

	// Until thinking_end the block still wears the signature the reasoning field
	// name gave it; from thinking_end on it wears the sequence. Partials taken
	// before the block exists have nothing to say either way.
	hasThinking := func(m *ai.AssistantMessage) bool {
		if m == nil {
			return false
		}
		for _, c := range m.Content {
			if _, ok := c.(ai.ThinkingContent); ok {
				return true
			}
		}
		return false
	}
	sawEnd := false
	for _, e := range events {
		if e.Type == ai.EventThinkingEnd {
			sawEnd = true
		}
		if !hasThinking(e.Partial) {
			continue
		}
		_, sig := thinkingSig(e.Partial)
		if sawEnd {
			if sig != want {
				t.Errorf("%s partial signature =\n%s\nwant\n%s", e.Type, sig, want)
			}
		} else if sig != "reasoning_content" {
			t.Errorf("%s partial signature = %q, want %q: the sequence must not be published before thinking_end", e.Type, sig, "reasoning_content")
		}
	}
	if !sawEnd {
		t.Fatalf("no thinking_end event: %v", eventTypes(events))
	}
}

// The details alone open a thinking block whose visible thinking stays empty,
// and no tool call is touched — b7bb00b93 deleted that attachment entirely.
func TestOpenAIStreamReasoningDetailsOpenThinkingBlock(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_details":[` + rdEncrypted + `]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	events, final := collectOpenAIEvents(t, sse, nil)
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	if len(final.Content) != 2 {
		t.Fatalf("content = %#v, want a thinking block then a tool call", final.Content)
	}
	think, ok := final.Content[0].(ai.ThinkingContent)
	if !ok || think.Thinking != "" || think.ThinkingSignature != "["+rdEncrypted+"]" {
		t.Fatalf("content[0] = %#v, want an empty thinking block signed with the sequence", final.Content[0])
	}
	tc, ok := final.Content[1].(ai.ToolCall)
	if !ok || tc.ThoughtSignature != "" {
		t.Fatalf("content[1] = %#v, want a tool call with no thoughtSignature", final.Content[1])
	}
	// The block is a real streamed block, not one conjured at finalization.
	var starts, ends int
	for _, e := range events {
		switch e.Type {
		case ai.EventThinkingStart:
			starts++
		case ai.EventThinkingEnd:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("thinking_start/thinking_end = %d/%d, want 1/1 (events %v)", starts, ends, eventTypes(events))
	}
}

// An invalid detail is skipped, not stored, and never opens a thinking block.
func TestOpenAIStreamReasoningDetailsInvalidSkipped(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":123},"nope",null,{"type":"unknown","data":"d"}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	for _, c := range final.Content {
		if _, ok := c.(ai.ThinkingContent); ok {
			t.Fatalf("all-invalid details must not open a thinking block: %#v", final.Content)
		}
	}

	mixed := `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":123},` + rdSummary + `,{"type":"reasoning.text","text":null}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	_, sig := thinkingSig(runOpenAIStream(t, mixed, nil))
	if sig != "["+rdSummary+"]" {
		t.Fatalf("signature = %s, want only the valid detail", sig)
	}
}

// A stream that dies before its thinking block is finalized never reaches the
// thinking_end that signs it, so the failure path has to sign it instead — pi's
// catch block walks output.content doing exactly that (upstream 7aab6c26e).
// Without it the accumulated sequence would simply be dropped, since nothing
// writes the signature per delta any more.
//
// The truncated stream is what makes that observable: a details-only delta
// pushes no event of its own, so the error is the first thing the consumer sees
// after the append.
func TestOpenAIStreamReasoningDetailsSurviveATruncatedStream(t *testing.T) {
	body := `data: {"choices":[{"delta":{"reasoning":"thinking"}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + rdSummary + `]}}]}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// Promise more body than we send, then hang up: the client's read of the
		// truncated body fails and the SSE loop returns that error mid-stream.
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body)+64, body)
		buf.Flush()
	}))
	t.Cleanup(server.Close)

	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, Reasoning: true}
	stream := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}})

	var failed *ai.AssistantMessage
	for e := range stream.Events() {
		if e.Type == ai.EventError {
			failed = e.Error
		}
	}
	if failed == nil {
		t.Fatalf("truncated stream did not fail")
	}
	if failed.StopReason != ai.StopError {
		t.Fatalf("stop reason = %s, want error", failed.StopReason)
	}
	_, sig := thinkingSig(failed)
	if sig != "["+rdSummary+"]" {
		t.Fatalf("aborted message signature =\n%s\nwant\n%s", sig, "["+rdSummary+"]")
	}
}

// pi guards the field with `Array.isArray`, which ignores a non-array value and
// goes on processing the delta. The rest of the delta must survive: this field
// is provider-controlled and loosely specified, and losing a delta's text and
// tool calls to it would silently truncate the turn.
func TestOpenAIStreamReasoningDetailsNonArrayIgnored(t *testing.T) {
	for _, value := range []string{`{}`, `{"a":1}`, `"str"`, `12`, `true`, `null`} {
		t.Run(value, func(t *testing.T) {
			sse := `data: {"choices":[{"delta":{"content":"hello","tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}],"reasoning_details":` + value + `}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
			final := runOpenAIStream(t, sse, nil)
			if final.StopReason != ai.StopToolUse {
				t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
			}
			if len(final.Content) != 2 {
				t.Fatalf("content = %#v, want the text and the tool call to survive", final.Content)
			}
			if text, ok := final.Content[0].(ai.TextContent); !ok || text.Text != "hello" {
				t.Fatalf("content[0] = %#v, want the text block", final.Content[0])
			}
			if tc, ok := final.Content[1].(ai.ToolCall); !ok || tc.ID != "call_1" {
				t.Fatalf("content[1] = %#v, want the tool call", final.Content[1])
			}
		})
	}
}

// The signature slot is single-purpose: once a sequence starts accumulating, the
// reasoning field name that opened the block is gone (and convertMessages stops
// needing it — the raw field is suppressed anyway).
//
// This is also the lock on the accumulator seeding EMPTY. pi used to seed it by
// re-parsing whatever sat in the slot; 7aab6c26e deleted that step, so the slot
// is written, never read. Streaming can only ever put the field name there, so
// the field name is what a seed-from-the-slot implementation would have to
// preserve — and does not.
func TestOpenAIStreamReasoningDetailsReplaceFieldNameSignature(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + rdSummary + `]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	think, sig := thinkingSig(runOpenAIStream(t, sse, nil))
	if think != "thinking" {
		t.Fatalf("thinking = %q", think)
	}
	if sig != "["+rdSummary+"]" {
		t.Fatalf("signature = %s, want the sequence to replace %q", sig, "reasoning_content")
	}
}

// ---- replay (convertMessages) ----

// replayAssistant builds a request body for one assistant turn and returns the
// assistant message it produced, or nil when the turn was skipped entirely.
func replayAssistant(t *testing.T, model *ai.Model, content ai.ContentList, toolResults ...ai.Message) map[string]any {
	t.Helper()
	messages := []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{
			Content:  content,
			Provider: model.Provider, Api: model.Api, Model: model.ID,
			StopReason: ai.StopStop,
		},
	}
	messages = append(messages, toolResults...)
	body := mustBuildOpenAIParams(t, model, ai.Context{Messages: messages}, &OpenAIOptions{})
	msgs, _ := body["messages"].([]map[string]any)
	for _, m := range msgs {
		if m["role"] == "assistant" {
			return m
		}
	}
	return nil
}

func replayModel() *ai.Model {
	return &ai.Model{
		ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Reasoning: true,
	}
}

// wireReasoningDetails renders the reasoning_details value the way it will hit
// the wire, so the assertion is on bytes rather than on a decoded shape.
func wireReasoningDetails(t *testing.T, msg map[string]any) string {
	t.Helper()
	v, ok := msg["reasoning_details"]
	if !ok {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal reasoning_details: %v", err)
	}
	return string(raw)
}

func TestOpenAIReplayPrefersSignedSequenceOverLegacy(t *testing.T) {
	sequence := "[" + rdText + "," + rdSummary + "]"
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "", ThinkingSignature: sequence},
		ai.ToolCall{ID: "call_1", Name: "f", Arguments: map[string]any{}, ThoughtSignature: rdEncrypted},
	}, ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})

	if got := wireReasoningDetails(t, msg); got != sequence {
		t.Fatalf("reasoning_details = %s, want the thinking block's sequence %s", got, sequence)
	}
}

func TestOpenAIReplayFallsBackToLegacyEncryptedDetails(t *testing.T) {
	second := `{"type":"reasoning.encrypted","id":"call_2","data":"ENC2"}`
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		// A thinking block whose signature is not a sequence does not preempt
		// the legacy path.
		ai.ThinkingContent{Thinking: "hm", ThinkingSignature: "reasoning"},
		ai.ToolCall{ID: "call_1", Name: "f", Arguments: map[string]any{}, ThoughtSignature: rdEncrypted},
		ai.ToolCall{ID: "bad", Name: "f", Arguments: map[string]any{}, ThoughtSignature: `{"type":"reasoning.text","text":"t"}`},
		ai.ToolCall{ID: "call_2", Name: "f", Arguments: map[string]any{}, ThoughtSignature: second},
	}, ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})

	want := "[" + rdEncrypted + "," + second + "]"
	if got := wireReasoningDetails(t, msg); got != want {
		t.Fatalf("reasoning_details = %s, want the valid legacy details in tool-call order %s", got, want)
	}
}

// Everything the pre-b7bb00b93 code would have replayed out of a tool call but
// that is not a well-formed encrypted detail is now dropped.
func TestOpenAIReplayLegacyRejectsLooseSignatures(t *testing.T) {
	for _, signature := range []string{
		`123`,
		`"opaque"`,
		`{"other":"shape"}`,
		`{"type":"reasoning.text","text":"t"}`,
		`{"type":"reasoning.encrypted","data":"d"}`,
		`{"type":"reasoning.encrypted","id":"","data":"d"}`,
		`{"type":"reasoning.encrypted","id":"i","data":""}`,
		`not json`,
	} {
		t.Run(signature, func(t *testing.T) {
			msg := replayAssistant(t, replayModel(), ai.ContentList{
				ai.ToolCall{ID: "call_1", Name: "f", Arguments: map[string]any{}, ThoughtSignature: signature},
			}, ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})
			if _, ok := msg["reasoning_details"]; ok {
				t.Fatalf("signature %s must not be replayed, got %s", signature, wireReasoningDetails(t, msg))
			}
		})
	}
}

func TestOpenAIReplayFirstParsingThinkingBlockWins(t *testing.T) {
	first := "[" + rdText + "]"
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "a", ThinkingSignature: "reasoning"},
		ai.ThinkingContent{Thinking: "b", ThinkingSignature: first},
		ai.ThinkingContent{Thinking: "c", ThinkingSignature: "[" + rdSummary + "]"},
		ai.TextContent{Text: "done"},
	})
	if got := wireReasoningDetails(t, msg); got != first {
		t.Fatalf("reasoning_details = %s, want the first parsing block's sequence %s", got, first)
	}
}

// A sequence to replay means the raw reasoning field is not written at all —
// reasoning_details is its structured alternative, not its companion.
func TestOpenAIReplaySuppressesRawReasoningField(t *testing.T) {
	sequence := "[" + rdText + "]"

	suppressed := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "I should read the file.", ThinkingSignature: sequence},
		ai.TextContent{Text: "done"},
	})
	for _, field := range []string{"reasoning", "reasoning_content", "reasoning_text"} {
		if _, ok := suppressed[field]; ok {
			t.Fatalf("%s must not be written alongside reasoning_details: %#v", field, suppressed)
		}
	}
	if got := wireReasoningDetails(t, suppressed); got != sequence {
		t.Fatalf("reasoning_details = %s, want %s", got, sequence)
	}

	// Without a sequence the raw field is still the mechanism.
	plain := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "I should read the file.", ThinkingSignature: "reasoning"},
		ai.TextContent{Text: "done"},
	})
	if plain["reasoning"] != "I should read the file." {
		t.Fatalf("reasoning = %#v, want the thinking text", plain["reasoning"])
	}
	if _, ok := plain["reasoning_details"]; ok {
		t.Fatalf("no sequence stored, so no reasoning_details: %#v", plain)
	}
}

// The opencode-go reasoning -> reasoning_content remap lives inside the
// suppressed branch, so a sequence silences it too.
func TestOpenAIReplaySuppressionCoversOpencodeGoRemap(t *testing.T) {
	model := replayModel()
	model.Provider = "opencode-go"
	msg := replayAssistant(t, model, ai.ContentList{
		ai.ThinkingContent{Thinking: "thought", ThinkingSignature: "reasoning"},
		ai.ThinkingContent{Thinking: "", ThinkingSignature: "[" + rdSummary + "]"},
		ai.TextContent{Text: "done"},
	})
	if _, ok := msg["reasoning_content"]; ok {
		t.Fatalf("opencode-go remap must be suppressed by a sequence: %#v", msg)
	}

	model2 := replayModel()
	model2.Provider = "opencode-go"
	remapped := replayAssistant(t, model2, ai.ContentList{
		ai.ThinkingContent{Thinking: "thought", ThinkingSignature: "reasoning"},
		ai.TextContent{Text: "done"},
	})
	if remapped["reasoning_content"] != "thought" {
		t.Fatalf("opencode-go remap = %#v, want reasoning_content", remapped)
	}
}

// Before b7bb00b93 ANY non-empty signature became an object key, so a Responses
// item id or a base64 blob was sent as a made-up request field. Only the three
// real field names qualify now.
func TestOpenAIReplayReasoningFieldAllowlist(t *testing.T) {
	for _, field := range []string{"reasoning", "reasoning_content", "reasoning_text"} {
		t.Run("allowed/"+field, func(t *testing.T) {
			msg := replayAssistant(t, replayModel(), ai.ContentList{
				ai.ThinkingContent{Thinking: "one", ThinkingSignature: field},
				ai.ThinkingContent{Thinking: "two", ThinkingSignature: field},
				ai.TextContent{Text: "done"},
			})
			if msg[field] != "one\ntwo" {
				t.Fatalf("%s = %#v, want the joined thinking", field, msg[field])
			}
		})
	}

	known := map[string]bool{"role": true, "content": true, "tool_calls": true, "reasoning_details": true}
	for _, signature := range []string{"sig-abc", "rs_68c1f0", "REASONING", "reasoning ", "Reasoning", "eyJhbGciOiJIUzI1NiJ9"} {
		t.Run("rejected/"+signature, func(t *testing.T) {
			msg := replayAssistant(t, replayModel(), ai.ContentList{
				ai.ThinkingContent{Thinking: "one", ThinkingSignature: signature},
				ai.TextContent{Text: "done"},
			})
			for key := range msg {
				if !known[key] {
					t.Fatalf("signature %q leaked onto the wire as field %q: %#v", signature, key, msg)
				}
			}
		})
	}
}

// reasoning_details no longer hangs off the tool_calls branch: a turn that only
// thought still replays its sequence.
func TestOpenAIReplayReasoningDetailsWithoutToolCalls(t *testing.T) {
	sequence := "[" + rdSummary + "]"
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "", ThinkingSignature: sequence},
		ai.TextContent{Text: "done"},
	})
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("no tool calls expected: %#v", msg)
	}
	if got := wireReasoningDetails(t, msg); got != sequence {
		t.Fatalf("reasoning_details = %s, want %s", got, sequence)
	}
}

// The empty-message guard is upstream of all of this: a turn with no text, no
// tool calls and only an empty thinking block is dropped, sequence and all.
func TestOpenAIReplayEmptyTurnStillSkipped(t *testing.T) {
	msg := replayAssistant(t, replayModel(), ai.ContentList{
		ai.ThinkingContent{Thinking: "", ThinkingSignature: "[" + rdSummary + "]"},
	})
	if msg != nil {
		t.Fatalf("empty assistant turn must be skipped, got %#v", msg)
	}
}

// The DeepSeek-style reasoning_content default is independent of the sequence:
// it fills in because the signature path did not write the field.
func TestOpenAIReplayReasoningContentDefaultStillApplies(t *testing.T) {
	model := replayModel()
	model.Compat = json.RawMessage(`{"requiresReasoningContentOnAssistantMessages":true}`)
	sequence := "[" + rdSummary + "]"
	msg := replayAssistant(t, model, ai.ContentList{
		ai.ThinkingContent{Thinking: "thought", ThinkingSignature: sequence},
		ai.TextContent{Text: "done"},
	})
	if msg["reasoning_content"] != "" {
		t.Fatalf("reasoning_content = %#v, want the empty-string default", msg["reasoning_content"])
	}
	if got := wireReasoningDetails(t, msg); got != sequence {
		t.Fatalf("reasoning_details = %s, want %s", got, sequence)
	}
}

// ---- stream then replay, end to end ----

func TestOpenAIReasoningDetailsStreamThenReplay(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning":"I should read the file.","reasoning_details":[` + rdText + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + rdSummary + `]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	msg := replayAssistant(t, replayModel(), final.Content,
		ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})

	want := "[" + rdText + "," + rdSummary + "]"
	if got := wireReasoningDetails(t, msg); got != want {
		t.Fatalf("replayed reasoning_details =\n%s\nwant\n%s", got, want)
	}
	for _, field := range []string{"reasoning", "reasoning_content", "reasoning_text"} {
		if _, ok := msg[field]; ok {
			t.Fatalf("%s must be suppressed on replay: %#v", field, msg)
		}
	}
	if !strings.Contains(wireReasoningDetails(t, msg), `"signature":"sha256:signed"`) {
		t.Fatalf("the provider's own fields must survive verbatim: %s", wireReasoningDetails(t, msg))
	}
}

// ---- delta merging (upstream c5ad7c1b0) ----

// OpenRouter streams reasoning_details as DELTAS, so consecutive same-type
// text/summary entries are ONE logical entry. The merge rules are two different
// JS assignment operators in the same function — `??=` on id/index, `||=` on
// format/signature — and the merged entry is byte-golden twice over: it is the
// thinking block's signature in the session file AND the reasoning_details sent
// back to the provider.
func TestAppendOpenAIReasoningDetailMergesDeltas(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		want   []string
	}{
		{
			// `index ??= source.index`: 0 is falsy but PRESENT, so it stays.
			// `||=` here would let the second delta's 7 through.
			name: "an index of 0 is present and survives",
			deltas: []string{
				`{"type":"reasoning.text","text":"a","index":0}`,
				`{"type":"reasoning.text","text":"b","index":7}`,
			},
			want: []string{`{"type":"reasoning.text","text":"ab","index":0}`},
		},
		{
			// The operators diverge on the empty string: `format ||=` overwrites
			// it, `id ??=` does not.
			name: "an empty format is overwritten, an empty id is not",
			deltas: []string{
				`{"type":"reasoning.text","text":"a","id":"","format":""}`,
				`{"type":"reasoning.text","text":"b","id":"src","format":"fmt"}`,
			},
			want: []string{`{"type":"reasoning.text","text":"ab","id":"","format":"fmt"}`},
		},
		{
			// null is both nullish and falsy, so every fill fires. A key the
			// target lacks is created at the END — `index` lands behind `id`.
			name: "null members are filled and a new key appends",
			deltas: []string{
				`{"type":"reasoning.text","text":"a","signature":null,"id":null}`,
				`{"type":"reasoning.text","text":"b","signature":"sig","id":"i","index":0}`,
			},
			want: []string{`{"type":"reasoning.text","text":"ab","signature":"sig","id":"i","index":0}`},
		},
		{
			// Filling from a member the source does not have assigns undefined,
			// and JSON.stringify omits an undefined value: the key is gone.
			name: "an absent source member erases a null target member",
			deltas: []string{
				`{"type":"reasoning.text","text":"a","signature":null,"id":null,"index":1}`,
				`{"type":"reasoning.text","text":"b"}`,
			},
			want: []string{`{"type":"reasoning.text","text":"ab","index":1}`},
		},
		{
			name: "a non-empty signature is not replaced",
			deltas: []string{
				`{"type":"reasoning.text","text":"a","signature":"first"}`,
				`{"type":"reasoning.text","text":"b","signature":"second"}`,
			},
			want: []string{`{"type":"reasoning.text","text":"ab","signature":"first"}`},
		},
		{
			// The summary branch fills only the three common fields: a signature
			// riding on a summary delta is not carried across, unlike the text
			// branch's dedicated `signature ||=`.
			name: "summary deltas merge without taking a signature",
			deltas: []string{
				`{"type":"reasoning.summary","summary":"a","index":2}`,
				`{"type":"reasoning.summary","summary":"b","format":"f","signature":"dropped"}`,
			},
			want: []string{`{"type":"reasoning.summary","summary":"ab","index":2,"format":"f"}`},
		},
		{
			name: "text and summary do not merge into each other",
			deltas: []string{
				`{"type":"reasoning.text","text":"a"}`,
				`{"type":"reasoning.summary","summary":"b"}`,
			},
			want: []string{
				`{"type":"reasoning.text","text":"a"}`,
				`{"type":"reasoning.summary","summary":"b"}`,
			},
		},
		{
			// Encrypted entries stay opaque and discrete: they never merge with
			// each other, and one between two text deltas breaks the run.
			name: "an encrypted entry breaks a text run",
			deltas: []string{
				`{"type":"reasoning.text","text":"a"}`,
				rdEncrypted,
				rdEncrypted,
				`{"type":"reasoning.text","text":"b"}`,
			},
			want: []string{
				`{"type":"reasoning.text","text":"a"}`,
				rdEncrypted,
				rdEncrypted,
				`{"type":"reasoning.text","text":"b"}`,
			},
		},
		{
			name: "three text deltas fold into one",
			deltas: []string{
				`{"type":"reasoning.text","text":"one "}`,
				`{"type":"reasoning.text","text":"two "}`,
				`{"type":"reasoning.text","text":"three"}`,
			},
			want: []string{`{"type":"reasoning.text","text":"one two three"}`},
		},
		{
			// `push({ ...detail })` stores a copy of the object pi PARSED, so a
			// redundant number form is already normalized away in the entry.
			name: "a pushed entry is rendered, not carried verbatim",
			deltas: []string{
				`{"type":"reasoning.encrypted","data":"ENC","index":1.0}`,
			},
			want: []string{`{"type":"reasoning.encrypted","data":"ENC","index":1}`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var details []json.RawMessage
			for _, delta := range tc.deltas {
				if !isOpenAIReasoningDetail(json.RawMessage(delta)) {
					t.Fatalf("delta is not a valid reasoning detail: %s", delta)
				}
				details = appendOpenAIReasoningDetail(details, json.RawMessage(delta))
			}
			got := marshalOpenAIReasoningDetails(details)
			want := "[" + strings.Join(tc.want, ",") + "]"
			if got != want {
				t.Fatalf("merged sequence =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// Mirrors pi's "merges consecutive text and summary reasoning_details deltas
// before replay" (openai-completions-reasoning-details.test.ts) end to end: the
// same deltas, the same merged sequence in the signature, and the same bytes
// replayed into the next request.
func TestOpenAIStreamMergesReasoningDetailDeltas(t *testing.T) {
	const (
		textDelta              = `{"type":"reasoning.text","text":"The","index":0}`
		textDeltaWithSignature = `{"type":"reasoning.text","text":" user wants the time.","signature":"sha256:text-signature","format":"openai-responses-v1","index":0}`
		summaryDelta           = `{"type":"reasoning.summary","summary":"Looked","index":0}`
		summaryDeltaWithFormat = `{"type":"reasoning.summary","summary":" up time.","format":"openai-responses-v1","index":0}`
		laterSummaryDelta      = `{"type":"reasoning.summary","summary":"After encrypted block.","format":"openai-responses-v1","index":0}`
	)
	// `signature` and `format` sit behind `index` because the first delta of
	// each run carried neither: the merge creates them, in the operators' order.
	want := "[" +
		`{"type":"reasoning.text","text":"The user wants the time.","index":0,"signature":"sha256:text-signature","format":"openai-responses-v1"}` + "," +
		`{"type":"reasoning.summary","summary":"Looked up time.","index":0,"format":"openai-responses-v1"}` + "," +
		rdEncrypted + "," +
		laterSummaryDelta +
		"]"

	sse := `data: {"choices":[{"delta":{"reasoning_details":[` + textDelta + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + textDeltaWithSignature + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + summaryDelta + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + summaryDeltaWithFormat + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + rdEncrypted + `]}}]}

data: {"choices":[{"delta":{"reasoning_details":[` + laterSummaryDelta + `]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	final := runOpenAIStream(t, sse, nil)
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop: %s (%s)", final.StopReason, final.ErrorMessage)
	}
	think, sig := thinkingSig(final)
	if think != "" {
		t.Fatalf("thinking text = %q, want empty", think)
	}
	if sig != want {
		t.Fatalf("thinking signature =\n%s\nwant\n%s", sig, want)
	}

	// The signature slot is also the session format and the replay channel, so
	// the merged bytes have to come back out on the wire unchanged.
	msg := replayAssistant(t, replayModel(), final.Content,
		ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "f", Content: ai.ContentList{ai.TextContent{Text: "ok"}}})
	if got := wireReasoningDetails(t, msg); got != want {
		t.Fatalf("replayed reasoning_details =\n%s\nwant\n%s", got, want)
	}
}

// A delta arriving in the SAME array as its predecessor merges too: pi appends
// every element of choice.delta.reasoning_details through the same path.
func TestOpenAIStreamMergesReasoningDetailDeltasWithinOneDelta(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"one "},{"type":"reasoning.text","text":"two"}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	_, sig := thinkingSig(runOpenAIStream(t, sse, nil))
	want := `[{"type":"reasoning.text","text":"one two"}]`
	if sig != want {
		t.Fatalf("signature =\n%s\nwant\n%s", sig, want)
	}
}
