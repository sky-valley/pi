package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestContentListDiscriminatedJSON(t *testing.T) {
	cl := ContentList{
		TextContent{Text: "hi"},
		ThinkingContent{Thinking: "hmm", ThinkingSignature: "sig"},
		ToolCall{ID: "1", Name: "bash", Arguments: map[string]any{"cmd": "ls"}},
	}
	raw, err := json.Marshal(cl)
	if err != nil {
		t.Fatal(err)
	}
	var back ContentList
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(back))
	}
	if _, ok := back[0].(TextContent); !ok {
		t.Fatalf("block 0 not TextContent: %T", back[0])
	}
	if _, ok := back[1].(ThinkingContent); !ok {
		t.Fatalf("block 1 not ThinkingContent: %T", back[1])
	}
	tc, ok := back[2].(ToolCall)
	if !ok || tc.Name != "bash" {
		t.Fatalf("block 2 not ToolCall: %#v", back[2])
	}
}

// TestContentListHoleIsNull pins a hole in the content array (a nil block):
// it is written null, as JSON.stringify writes one, and a null element reads
// back as a hole, as JSON.parse keeps it. want is node's JSON.stringify of
// `a[1] = {type:"text", text:"a"}` on an empty array.
func TestContentListHoleIsNull(t *testing.T) {
	const want = `[null,{"type":"text","text":"a"}]`
	raw, err := json.Marshal(ContentList{nil, TextContent{Text: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("marshal = %s, want %s", raw, want)
	}
	var back ContentList
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0] != nil || back[1] != (TextContent{Text: "a"}) {
		t.Fatalf("unmarshal = %#v, want a hole then the text block", back)
	}
}

// TestContentBlocksSerializeTypeFirst pins content blocks to pi's literal shape:
// the discriminator first, then the block's fields in declaration order
// (`{type: "text", text}`, `{type: "toolCall", id, name, arguments}`), which is
// also the document order a pi session file carries.
func TestContentBlocksSerializeTypeFirst(t *testing.T) {
	cl := ContentList{
		TextContent{Text: "a", TextSignature: "sig"},
		ThinkingContent{Thinking: "t", ThinkingSignature: "s", Redacted: true},
		ImageContent{Data: "x", MimeType: "image/png"},
		ToolCall{ID: "1", Name: "bash", Arguments: map[string]any{"cmd": "ls"}, ThoughtSignature: "ts", Namespace: "ns"},
		TextContent{Text: ""},
	}
	const want = `[{"type":"text","text":"a","textSignature":"sig"},` +
		`{"type":"thinking","thinking":"t","thinkingSignature":"s","redacted":true},` +
		`{"type":"image","data":"x","mimeType":"image/png"},` +
		`{"type":"toolCall","id":"1","name":"bash","arguments":{"cmd":"ls"},"thoughtSignature":"ts","namespace":"ns"},` +
		`{"type":"text","text":""}]`
	if got := jsJSON(t, cl); got != want {
		t.Fatalf("content blocks\n got %s\nwant %s", got, want)
	}
}

func TestMessageRoleRoundTrip(t *testing.T) {
	msgs := []Message{
		NewUserText("hello", 1),
		AssistantMessage{Content: ContentList{TextContent{Text: "hi"}}, Model: "m", StopReason: StopStop, Timestamp: 2},
		ToolResultMessage{ToolCallID: "1", ToolName: "bash", Content: ContentList{TextContent{Text: "ok"}}, Timestamp: 3},
	}
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		back, err := UnmarshalMessage(raw)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if back.MessageRole() != m.MessageRole() {
			t.Fatalf("role mismatch: %s vs %s", back.MessageRole(), m.MessageRole())
		}
	}
}

func TestUserMessageAcceptsStringContent(t *testing.T) {
	var m UserMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":"plain text","timestamp":5}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(m.Content))
	}
	tc, ok := m.Content[0].(TextContent)
	if !ok || tc.Text != "plain text" {
		t.Fatalf("string content not normalized: %#v", m.Content[0])
	}
}

// TestUserMessageStringContentRoundTrip asserts string-form content is
// re-emitted as a string on marshal (pi: content is string | array, passed
// through untouched), while array-form content stays an array.
func TestUserMessageStringContentRoundTrip(t *testing.T) {
	src := `{"role":"user","content":"plain text","timestamp":5}`
	var m UserMessage
	if err := json.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != src {
		t.Fatalf("string content round-trip changed:\n got: %s\nwant: %s", out, src)
	}

	// Array-form input must stay an array.
	arr := UserMessage{Content: ContentList{TextContent{Text: "hello"}}, Timestamp: 1}
	raw, err := json.Marshal(arr)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Content) == 0 || probe.Content[0] != '[' {
		t.Fatalf("array content serialized as non-array: %s", raw)
	}

	// NewUserText is string-form, like pi's prompt-created user messages
	// (`content` is a plain string on the wire and in session files).
	str, err := json.Marshal(NewUserText("hello", 1))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"user","content":"hello","timestamp":1}`; string(str) != want {
		t.Fatalf("NewUserText must serialize string-form:\n got: %s\nwant: %s", str, want)
	}
}

// TestUserMessageMissingContentTolerated asserts a missing or null content key
// yields empty content rather than an error (JSON.parse tolerance in pi).
func TestUserMessageMissingContentTolerated(t *testing.T) {
	var m UserMessage
	if err := json.Unmarshal([]byte(`{"role":"user","timestamp":5}`), &m); err != nil {
		t.Fatalf("missing content key errored: %v", err)
	}
	if len(m.Content) != 0 || m.Timestamp != 5 {
		t.Fatalf("missing content: got %#v ts=%d", m.Content, m.Timestamp)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":null,"timestamp":5}`), &m); err != nil {
		t.Fatalf("null content errored: %v", err)
	}
	if len(m.Content) != 0 {
		t.Fatalf("null content: got %#v", m.Content)
	}
}

// SystemMessage key order is producer-dependent in pi, and these bytes are
// shared with pi on the pi-messages wire and in session files. Every expected
// string below was captured by running the same producer under node at
// upstream 9e05370b2.

const sysTestToolJSON = `{"name":"a","description":"d","parameters":{"type":"object","properties":{}}}`

func sysTestTool() Tool {
	return Tool{Name: "a", Description: "d", Parameters: &Schema{Type: "object", Properties: map[string]*Schema{}}}
}

func TestSystemMessageDefaultLiteralOrder(t *testing.T) {
	// createInitialSystemMessage / getCurrentSystemMessage literal order:
	// role, content, sections?, toolsAdded?, (toolsRemoved?), timestamp.
	m := NewSystemText("p", 0)
	m.Sections = SystemSections{{Name: "s", Value: strp("S")}}
	m.ToolsAdded = []Tool{sysTestTool()}
	want := `{"role":"system","content":"p","sections":{"s":"S"},"toolsAdded":[` + sysTestToolJSON + `],"timestamp":0}`
	if got := jsJSON(t, m); got != want {
		t.Fatalf("default order\n got %s\nwant %s", got, want)
	}
	// A nil Content is pi's `content: ""`.
	if got, want := jsJSON(t, SystemMessage{Timestamp: 4}), `{"role":"system","content":"","timestamp":4}`; got != want {
		t.Fatalf("zero content\n got %s\nwant %s", got, want)
	}
	// Array content stays an array.
	arr := SystemMessage{Content: ContentList{TextContent{Text: "a"}, TextContent{Text: "b"}}, Timestamp: 1}
	if got, want := jsJSON(t, arr), `{"role":"system","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"timestamp":1}`; got != want {
		t.Fatalf("array content\n got %s\nwant %s", got, want)
	}
	if s, ok := NewSystemText("x", 0).StringContent(); !ok || s != "x" {
		t.Fatalf("StringContent = %q, %v", s, ok)
	}
	if _, ok := arr.StringContent(); ok {
		t.Fatal("array content reported as the string form")
	}
}

func TestSystemMessageWithToolChangesAppendsToolKeys(t *testing.T) {
	// agent-loop.ts withToolChanges: `{...rest, toolsAdded?, toolsRemoved?}`.
	pending := SystemMessage{Sections: SystemSections{{Name: "preamble", Value: strp("p")}}, Timestamp: 9}
	changed := pending.WithToolChanges(ToolStateChanges{
		ToolsAdded:   []Tool{sysTestTool()},
		ToolsRemoved: []ToolReference{{Name: "old"}},
	})
	want := `{"role":"system","content":"","sections":{"preamble":"p"},"timestamp":9,"toolsAdded":[` + sysTestToolJSON + `],"toolsRemoved":[{"name":"old"}]}`
	if got := jsJSON(t, changed); got != want {
		t.Fatalf("withToolChanges\n got %s\nwant %s", got, want)
	}
	if pending.ToolsAdded != nil || jsJSON(t, pending) != `{"role":"system","content":"","sections":{"preamble":"p"},"timestamp":9}` {
		t.Fatalf("WithToolChanges mutated its receiver: %s", jsJSON(t, pending))
	}

	// Empty changes drop the tool keys entirely.
	declared := NewSystemText("x", 9)
	declared.ToolsAdded = []Tool{sysTestTool()}
	if got, want := jsJSON(t, declared.WithToolChanges(ToolStateChanges{})), `{"role":"system","content":"x","timestamp":9}`; got != want {
		t.Fatalf("withToolChanges(no changes)\n got %s\nwant %s", got, want)
	}

	// A second application keeps the already-moved order.
	once := SystemMessage{Timestamp: 9}.WithToolChanges(ToolStateChanges{ToolsAdded: []Tool{sysTestTool()}})
	twice := once.WithToolChanges(ToolStateChanges{ToolsRemoved: []ToolReference{{Name: "a"}}})
	if got, want := jsJSON(t, twice), `{"role":"system","content":"","timestamp":9,"toolsRemoved":[{"name":"a"}]}`; got != want {
		t.Fatalf("withToolChanges twice\n got %s\nwant %s", got, want)
	}
}

func TestSystemMessageDecodedDocumentOrder(t *testing.T) {
	const doc = `{"timestamp":3,"toolsRemoved":[{"name":"x"}],"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"role":"system","sections":{"s":null,"t":"T"}}`
	msg, err := UnmarshalMessage([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := msg.(SystemMessage)
	if !ok {
		t.Fatalf("UnmarshalMessage(system) = %T, want SystemMessage", msg)
	}
	if got := jsJSON(t, sys); got != doc {
		t.Fatalf("document order round trip\n got %s\nwant %s", got, doc)
	}
	if got, want := GetSystemMessageText(sys), "a\nb\n\nT"; got != want {
		t.Fatalf("decoded text = %q, want %q", got, want)
	}
	// withToolChanges over the decoded object keeps its order for the rest.
	want := `{"timestamp":3,"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"role":"system","sections":{"s":null,"t":"T"},"toolsAdded":[` + sysTestToolJSON + `]}`
	if got := jsJSON(t, sys.WithToolChanges(ToolStateChanges{ToolsAdded: []Tool{sysTestTool()}})); got != want {
		t.Fatalf("withToolChanges(decoded)\n got %s\nwant %s", got, want)
	}

	// String content round-trips as a string.
	const str = `{"role":"system","content":"x","toolsAdded":[` + sysTestToolJSON + `],"timestamp":0}`
	var decoded SystemMessage
	if err := json.Unmarshal([]byte(str), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := jsJSON(t, decoded); got != str {
		t.Fatalf("string content round trip\n got %s\nwant %s", got, str)
	}
	// Explicitly empty tool lists are carried, as a JS object carries them.
	const emptyLists = `{"role":"system","content":"","toolsAdded":[],"toolsRemoved":[],"timestamp":2}`
	decoded = SystemMessage{}
	if err := json.Unmarshal([]byte(emptyLists), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := jsJSON(t, decoded); got != emptyLists {
		t.Fatalf("empty tool lists round trip\n got %s\nwant %s", got, emptyLists)
	}
}

func TestSystemMessageNullContentIsEmptyString(t *testing.T) {
	var m SystemMessage
	if err := json.Unmarshal([]byte(`{"role":"system","content":null,"timestamp":5}`), &m); err != nil {
		t.Fatalf("null content errored: %v", err)
	}
	if got, want := jsJSON(t, m), `{"role":"system","content":"","timestamp":5}`; got != want {
		t.Fatalf("null content\n got %s\nwant %s", got, want)
	}
	if s, ok := m.StringContent(); !ok || s != "" {
		t.Fatalf("null content StringContent = %q, %v", s, ok)
	}
	m = SystemMessage{}
	if err := json.Unmarshal([]byte(`{"role":"system","timestamp":5}`), &m); err != nil {
		t.Fatalf("missing content errored: %v", err)
	}
	if GetSystemMessageText(m) != "" || m.Timestamp != 5 {
		t.Fatalf("missing content decoded as %s", jsJSON(t, m))
	}
}

func TestSystemMessageSectionsObjectOrder(t *testing.T) {
	m := NewSystemText("base", 1)
	m.Sections = SystemSections{
		{Name: "z", Value: strp("Z")}, {Name: "2", Value: strp("two")}, {Name: "1", Value: strp("one")},
		{Name: "a", Value: strp("A")}, {Name: "01", Value: strp("zero-one")}, {Name: "-1", Value: strp("neg")},
		{Name: "4294967295", Value: strp("max")}, {Name: "4294967294", Value: strp("maxidx")},
	}
	const want = `{"role":"system","content":"base","sections":{"1":"one","2":"two","4294967294":"maxidx","z":"Z","a":"A","01":"zero-one","-1":"neg","4294967295":"max"},"timestamp":1}`
	if got := jsJSON(t, m); got != want {
		t.Fatalf("integer-like section names\n got %s\nwant %s", got, want)
	}
	const doc = `{"role":"system","content":"","sections":{"b":"1","10":"x","a":null},"timestamp":1}`
	var decoded SystemMessage
	if err := json.Unmarshal([]byte(doc), &decoded); err != nil {
		t.Fatal(err)
	}
	if got, want := jsJSON(t, decoded), `{"role":"system","content":"","sections":{"10":"x","b":"1","a":null},"timestamp":1}`; got != want {
		t.Fatalf("decoded sections\n got %s\nwant %s", got, want)
	}
	if v, ok := decoded.Sections.Get("a"); !ok || v != nil {
		t.Fatalf("decoded null section = %v, %v, want a present removal", v, ok)
	}
}

// JSON.parse gives a repeated name its first slot and its last value.
// Captured under node at 9e05370b2 (JSON.parse, then JSON.stringify,
// getSystemMessageText and getCurrentSystemMessage).
func TestSystemSectionsDecodeRepeatedNameKeepsFirstSlotAndLastValue(t *testing.T) {
	for _, tc := range []struct {
		sections, json, text, current string
	}{
		{
			sections: `{"a":"1","b":"2","a":"3"}`,
			json:     `{"role":"system","content":"base","sections":{"a":"3","b":"2"},"timestamp":1}`,
			text:     "base\n\n3\n\n2",
			current:  `{"role":"system","content":"base","sections":{"a":"3","b":"2"},"timestamp":1}`,
		},
		{
			sections: `{"a":"1","b":"2","a":null}`,
			json:     `{"role":"system","content":"base","sections":{"a":null,"b":"2"},"timestamp":1}`,
			text:     "base\n\n2",
			current:  `{"role":"system","content":"base","sections":{"b":"2"},"timestamp":1}`,
		},
		{
			sections: `{"a":null,"b":"2","a":"3"}`,
			json:     `{"role":"system","content":"base","sections":{"a":"3","b":"2"},"timestamp":1}`,
			text:     "base\n\n3\n\n2",
			current:  `{"role":"system","content":"base","sections":{"a":"3","b":"2"},"timestamp":1}`,
		},
	} {
		msg, err := UnmarshalMessage([]byte(`{"role":"system","content":"base","sections":` + tc.sections + `,"timestamp":1}`))
		if err != nil {
			t.Fatal(err)
		}
		sys := msg.(SystemMessage)
		if got := jsJSON(t, sys); got != tc.json {
			t.Errorf("%s: JSON\n got %s\nwant %s", tc.sections, got, tc.json)
		}
		if got := GetSystemMessageText(sys); got != tc.text {
			t.Errorf("%s: text = %q, want %q", tc.sections, got, tc.text)
		}
		current, _ := GetCurrentSystemMessage([]Message{sys})
		if got := jsJSON(t, current); got != tc.current {
			t.Errorf("%s: current\n got %s\nwant %s", tc.sections, got, tc.current)
		}
		if n := sys.Sections.Len(); n != 2 {
			t.Errorf("%s: Len = %d, want 2", tc.sections, n)
		}
	}
}

// A decoded document and a WithToolChanges result are JS objects: a key set on
// one later is appended after its own keys, even when those are in default
// order. A constructed message places it in its default slot. Captured under
// node at 9e05370b2 (JSON.parse / the agent-loop spread, then `m.sections = …`
// and JSON.stringify).
func TestSystemMessageKeySetLaterAppendsAfterARecordedOrder(t *testing.T) {
	sections := SystemSections{{Name: "s", Value: strp("S")}}
	for _, tc := range []struct{ doc, want string }{
		{`{"role":"system","content":"x","timestamp":1}`, `{"role":"system","content":"x","timestamp":1,"sections":{"s":"S"}}`},
		{`{"content":"x","role":"system","timestamp":1}`, `{"content":"x","role":"system","timestamp":1,"sections":{"s":"S"}}`},
		{`{"role":"system","content":"x","sections":{"s":"old"},"timestamp":1}`, `{"role":"system","content":"x","sections":{"s":"S"},"timestamp":1}`},
	} {
		var m SystemMessage
		if err := json.Unmarshal([]byte(tc.doc), &m); err != nil {
			t.Fatal(err)
		}
		m.Sections = sections
		if got := jsJSON(t, m); got != tc.want {
			t.Errorf("decoded %s, then sections set\n got %s\nwant %s", tc.doc, got, tc.want)
		}
	}

	declared := NewSystemText("x", 9)
	declared.ToolsAdded = []Tool{sysTestTool()}
	changed := declared.WithToolChanges(ToolStateChanges{})
	changed.Sections = sections
	if got, want := jsJSON(t, changed), `{"role":"system","content":"x","timestamp":9,"sections":{"s":"S"}}`; got != want {
		t.Errorf("withToolChanges, then sections set\n got %s\nwant %s", got, want)
	}

	constructed := NewSystemText("x", 1)
	constructed.Sections = sections
	if got, want := jsJSON(t, constructed), `{"role":"system","content":"x","sections":{"s":"S"},"timestamp":1}`; got != want {
		t.Errorf("constructed\n got %s\nwant %s", got, want)
	}
}

func TestTranscriptContextMarshalsEmptyMessages(t *testing.T) {
	if got, want := jsJSON(t, TranscriptContext{}), `{"messages":[]}`; got != want {
		t.Fatalf("empty transcript = %s, want %s", got, want)
	}
}

// ConstrainedSamplingConfig mirrors pi's discriminated union, whose "disabled"
// spelling on the wire is the literal `false`.
func TestConstrainedSamplingConfigJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ConstrainedSamplingConfig
		want string
	}{
		{"disabled", ConstrainedSamplingConfig{}, `false`},
		{
			"json_schema", ConstrainedSamplingConfig{Type: ConstrainedSamplingJSONSchema, Strict: ConstrainedSamplingRequire},
			`{"type":"json_schema","strict":"require"}`,
		},
		{
			"grammar",
			ConstrainedSamplingConfig{Type: ConstrainedSamplingGrammar, Variants: GrammarVariants{OpenAILark: "start: /.+/"}},
			`{"type":"grammar","variants":{"openai_lark":"start: /.+/"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Fatalf("marshal = %s, want %s", raw, tc.want)
			}
			var back ConstrainedSamplingConfig
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if back != tc.cfg {
				t.Fatalf("round trip = %#v, want %#v", back, tc.cfg)
			}
		})
	}

	// A tool carrying `false` decodes to the disabled config, not an error.
	var tool Tool
	if err := json.Unmarshal([]byte(`{"name":"t","description":"d","constrainedSampling":false}`), &tool); err != nil {
		t.Fatalf("constrainedSampling:false must decode: %v", err)
	}
	if tool.ConstrainedSampling == nil || tool.ConstrainedSampling.Type != "" {
		t.Fatalf("constrainedSampling:false = %#v", tool.ConstrainedSampling)
	}
}

// TestConstrainedSamplingUnknownTypeRejected: the discriminant is a typed
// string, and an unrecognized value must be a loud error on BOTH sides rather
// than silently round-tripping to `false` — which would quietly drop the
// caller's constrained-sampling request.
func TestConstrainedSamplingUnknownTypeRejected(t *testing.T) {
	t.Run("marshal", func(t *testing.T) {
		_, err := json.Marshal(ConstrainedSamplingConfig{Type: "bogus"})
		if err == nil {
			t.Fatal("marshalling an unknown type must fail, not emit false")
		}
		if !strings.Contains(err.Error(), "bogus") {
			t.Errorf("error should name the offending type, got %v", err)
		}
	})
	t.Run("unmarshal", func(t *testing.T) {
		var c ConstrainedSamplingConfig
		if err := json.Unmarshal([]byte(`{"type":"bogus"}`), &c); err == nil {
			t.Fatal("unmarshalling an unknown type must fail")
		}
	})
	t.Run("unmarshal true leaks no internal type", func(t *testing.T) {
		var c ConstrainedSamplingConfig
		err := json.Unmarshal([]byte(`true`), &c)
		if err == nil {
			t.Fatal("`true` is not a valid constrainedSampling value")
		}
		if strings.Contains(err.Error(), "alias") {
			t.Errorf("error leaks the private shim type: %v", err)
		}
		if !strings.Contains(err.Error(), "json_schema") {
			t.Errorf("error should hint the valid spellings, got %v", err)
		}
	})
	t.Run("empty strict defaults to prefer on the wire", func(t *testing.T) {
		b, err := json.Marshal(ConstrainedSamplingConfig{Type: ConstrainedSamplingJSONSchema})
		if err != nil {
			t.Fatal(err)
		}
		// pi's union admits only "prefer"|"require"; "" is not a value it accepts.
		if got, want := string(b), `{"type":"json_schema","strict":"prefer"}`; got != want {
			t.Errorf("marshal = %s, want %s", got, want)
		}
	})
}

// TestAssistantDeferredHandleRoundTrip locks pi 382aa641c's message-format
// half: a deferred assistant message persists its stop reason and handle, and
// a message without one is byte-identical to what it was before the field
// existed.
// pi c3e7bc60a carries endTurn as an optional boolean, so an explicit false has
// to survive the wire distinctly from an absent field. No provider this port
// carries sets it, so the shape is all there is to pin.
// ToolCall has a hand-written marshaller with an explicit field list, so a new
// field survives only if it was added there too (pi 02bd2d1c6 `namespace?`).
func TestToolCallNamespaceRoundTrip(t *testing.T) {
	plain := ToolCall{ID: "call_1|fc_1", Name: "calc", Arguments: map[string]any{"x": 1.0}}
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "namespace") {
		t.Fatalf("an unset namespace must be omitted: %s", raw)
	}

	ns := plain
	ns.Namespace = "mcp_math"
	raw, err = json.Marshal(ns)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"namespace":"mcp_math"`) {
		t.Fatalf("namespace not persisted: %s", raw)
	}

	var back ToolCall
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Namespace != "mcp_math" {
		t.Fatalf("namespace round-trip = %q, want mcp_math", back.Namespace)
	}
	if back.ID != ns.ID || back.Name != ns.Name {
		t.Fatalf("namespace displaced a sibling field: %+v", back)
	}
}

func TestAssistantEndTurnRoundTrip(t *testing.T) {
	base := AssistantMessage{
		Content: ContentList{TextContent{Text: "hi"}}, Api: "api", Provider: "p",
		Model: "m", StopReason: StopStop, Timestamp: 5,
	}

	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "endTurn") {
		t.Fatalf("an unset endTurn must be omitted: %s", raw)
	}

	for _, want := range []bool{true, false} {
		msg := base
		msg.EndTurn = &want
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), fmt.Sprintf(`"endTurn":%t`, want)) {
			t.Fatalf("endTurn=%t not persisted: %s", want, raw)
		}
		back, err := UnmarshalMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := back.(AssistantMessage)
		if !ok {
			t.Fatalf("round-trip type = %T, want AssistantMessage", back)
		}
		if got.EndTurn == nil || *got.EndTurn != want {
			t.Fatalf("endTurn round-trip = %v, want %t", got.EndTurn, want)
		}
	}
}

func TestAssistantDeferredHandleRoundTrip(t *testing.T) {
	plain := AssistantMessage{
		Content: ContentList{TextContent{Text: "hi"}}, Api: "api", Provider: "p",
		Model: "m", StopReason: StopStop, Timestamp: 5,
	}
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "deferred") {
		t.Fatalf("a message without a handle must not gain a deferred field: %s", raw)
	}

	deferred := plain
	deferred.Content = ContentList{}
	deferred.StopReason = StopDeferred
	deferred.Deferred = &DeferredHandle{
		Provider: "p", ModelID: "m", Api: "api", ID: "resp-1", PollAfterMs: 25,
		Data: map[string]any{"cursor": "abc"},
	}
	raw, err = json.Marshal(deferred)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stopReason":"deferred"`) {
		t.Fatalf("stop reason not persisted: %s", raw)
	}
	if strings.Contains(string(raw), "expiresAt") {
		t.Fatalf("an unset expiresAt must be omitted: %s", raw)
	}

	back, err := UnmarshalMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := back.(AssistantMessage)
	if !ok {
		t.Fatalf("round-trip type = %T, want AssistantMessage", back)
	}
	if msg.StopReason != StopDeferred || msg.Deferred == nil {
		t.Fatalf("deferred message did not round-trip: %+v", msg)
	}
	if msg.Deferred.ID != "resp-1" || msg.Deferred.PollAfterMs != 25 || msg.Deferred.ModelID != "m" {
		t.Fatalf("handle round-trip = %+v", *msg.Deferred)
	}
	data, ok := msg.Deferred.Data.(map[string]any)
	if !ok || data["cursor"] != "abc" {
		t.Fatalf("opaque handle data did not survive: %#v", msg.Deferred.Data)
	}

	// Cloning must not alias the handle back onto the original message.
	clone := msg.Clone()
	clone.Deferred.ID = "other"
	if msg.Deferred.ID != "resp-1" {
		t.Fatal("Clone must copy the deferred handle, not alias it")
	}
}

// TestModelPromptCacheRoundTrips pins upstream c596d09d9: Model.promptCache is
// catalog data — the best-effort cache lifetime in seconds per retention tier —
// and the catalog regen emits it for direct Anthropic models. A Model that
// cannot carry the key silently drops it on decode, so the port would ship a
// catalog that no longer matches what pi publishes.
func TestModelPromptCacheRoundTrips(t *testing.T) {
	const src = `{"id":"m","name":"M","api":"anthropic-messages","provider":"anthropic",` +
		`"baseUrl":"https://example.invalid","reasoning":false,"input":["text"],` +
		`"cost":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0},` +
		`"promptCache":{"short":300,"long":3600},"contextWindow":1000,"maxTokens":100}`
	var m Model
	if err := json.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), `"promptCache":{"short":300,"long":3600}`) {
		t.Fatalf("promptCache did not survive the round trip: %s", out)
	}
	if m.PromptCache == nil || m.PromptCache.Short == nil || *m.PromptCache.Short != 300 {
		t.Fatalf("short lifetime not decoded: %#v", m.PromptCache)
	}
	if m.PromptCache.Long == nil || *m.PromptCache.Long != 3600 {
		t.Fatalf("long lifetime not decoded: %#v", m.PromptCache)
	}

	// A model with no prompt-cache metadata must not grow an empty key: pi
	// leaves promptCache undefined when the provider's behavior is unknown.
	var plain Model
	if err := json.Unmarshal([]byte(`{"id":"m","name":"M","api":"anthropic-messages","provider":"anthropic","baseUrl":"x","reasoning":false,"input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":1,"maxTokens":1}`), &plain); err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	bare, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("encode plain: %v", err)
	}
	if strings.Contains(string(bare), "promptCache") {
		t.Fatalf("a model without cache metadata must not emit promptCache: %s", bare)
	}
}

// topLevelJSONKeys lists an object's keys in the order they were written.
func topLevelJSONKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", data)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// durationMs sits where pi writes it (upstream 36a686ee8): the agent loop's
// createToolResultMessage puts it between isError and timestamp, and
// AssistantMessageEventStream adds it to a finished response after every other
// key the provider wrote. Key orders from a src run of pi's agentLoop at
// d2e7d8b7c; usage is undefined there, which JSON.stringify drops.
func TestDurationMsKeyPosition(t *testing.T) {
	d := int64(5)
	trm, err := json.Marshal(ToolResultMessage{
		ToolCallID: "c1", ToolName: "echo", Content: ContentList{TextContent{Text: "a"}},
		Details: map[string]any{"value": "a"}, DurationMs: &d, Timestamp: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := topLevelJSONKeys(t, trm), []string{"role", "toolCallId", "toolName", "content", "details", "isError", "durationMs", "timestamp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("toolResult keys = %v, want %v", got, want)
	}
	am, err := json.Marshal(AssistantMessage{Api: "x", Provider: "x", Model: "m", StopReason: StopStop, Timestamp: 1, DurationMs: &d})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := topLevelJSONKeys(t, am), []string{"role", "content", "api", "provider", "model", "usage", "stopReason", "timestamp", "durationMs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant keys = %v, want %v", got, want)
	}
}
