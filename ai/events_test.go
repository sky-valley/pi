package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTextDeltaEventContentIndexZeroPresent asserts that a text_delta event with
// contentIndex 0 still serializes the contentIndex key (pi marks it required on
// block events, types.ts:360-368). With omitempty the key was dropped for index 0.
func TestTextDeltaEventContentIndexZeroPresent(t *testing.T) {
	e := AssistantMessageEvent{
		Type:         EventTextDelta,
		ContentIndex: 0,
		Delta:        "hi",
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["contentIndex"]; !ok {
		t.Fatalf("contentIndex key missing for index 0: %s", raw)
	}
	if string(got["contentIndex"]) != "0" {
		t.Fatalf("contentIndex = %s, want 0", got["contentIndex"])
	}
	if _, ok := got["delta"]; !ok {
		t.Fatalf("delta key missing: %s", raw)
	}
}

// TestTextEndEmptyContentKeyPresent asserts content key is present even when empty
// on text_end (pi marks content required there, types.ts:362).
func TestTextEndEmptyContentKeyPresent(t *testing.T) {
	e := AssistantMessageEvent{Type: EventTextEnd, ContentIndex: 0, Content: ""}
	raw, _ := json.Marshal(e)
	if !strings.Contains(string(raw), `"content":""`) {
		t.Fatalf("content key missing for empty content: %s", raw)
	}
}

// TestDoneEventReasonPresent asserts reason key present on done events.
func TestDoneEventReasonPresent(t *testing.T) {
	e := AssistantMessageEvent{Type: EventDone, Reason: StopStop}
	raw, _ := json.Marshal(e)
	if !strings.Contains(string(raw), `"reason":"stop"`) {
		t.Fatalf("reason key missing on done: %s", raw)
	}
}
