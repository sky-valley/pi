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
