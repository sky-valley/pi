package ai

import (
	"encoding/json"
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
