package ai

import (
	"encoding/json"
	"slices"
	"sort"
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

// TestEventMarshalPerVariantKeys is the per-variant golden assertion for A6:
// every event variant must serialize exactly the keys its pi union variant
// declares (types.ts AssistantMessageEvent) — required keys always present
// (contentIndex:0 included), keys from other variants never leak (no
// "reason":"" on start, no "contentIndex":0 on done/error, ...).
func TestEventMarshalPerVariantKeys(t *testing.T) {
	partial := &AssistantMessage{Model: "m"}
	// Populate EVERY flat-struct field so leaks are detectable.
	full := AssistantMessageEvent{
		ContentIndex: 0,
		Delta:        "d",
		Content:      "c",
		ToolCall:     &ToolCall{ID: "t1", Name: "bash"},
		Partial:      partial,
		Reason:       StopStop,
		Message:      partial,
		Error:        partial,
	}
	cases := []struct {
		typ  EventType
		keys []string
	}{
		{EventStart, []string{"partial", "type"}},
		{EventTextStart, []string{"contentIndex", "partial", "type"}},
		{EventTextDelta, []string{"contentIndex", "delta", "partial", "type"}},
		{EventTextEnd, []string{"content", "contentIndex", "partial", "type"}},
		{EventThinkingStart, []string{"contentIndex", "partial", "type"}},
		{EventThinkingDelta, []string{"contentIndex", "delta", "partial", "type"}},
		{EventThinkingEnd, []string{"content", "contentIndex", "partial", "type"}},
		{EventToolCallStart, []string{"contentIndex", "partial", "type"}},
		{EventToolCallDelta, []string{"contentIndex", "delta", "partial", "type"}},
		{EventToolCallEnd, []string{"contentIndex", "partial", "toolCall", "type"}},
		{EventDone, []string{"message", "reason", "type"}},
		{EventError, []string{"error", "reason", "type"}},
	}
	for _, c := range cases {
		e := full
		e.Type = c.typ
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !slices.Equal(keys, c.keys) {
			t.Errorf("%s keys = %v, want %v (raw: %s)", c.typ, keys, c.keys, raw)
		}
	}
}

// TestStartEventNoSpuriousValues asserts the regression A6 fixed: start/done
// events must not carry zero-valued top-level keys from other variants.
func TestStartEventNoSpuriousValues(t *testing.T) {
	topKeys := func(e AssistantMessageEvent) map[string]bool {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		keys := map[string]bool{}
		for k := range m {
			keys[k] = true
		}
		return keys
	}

	start := topKeys(AssistantMessageEvent{Type: EventStart, Partial: &AssistantMessage{}})
	for _, bad := range []string{"reason", "contentIndex", "delta", "content", "message", "error", "toolCall"} {
		if start[bad] {
			t.Errorf("start event leaked %q key", bad)
		}
	}
	done := topKeys(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: &AssistantMessage{}})
	for _, bad := range []string{"contentIndex", "delta", "partial", "content", "error", "toolCall"} {
		if done[bad] {
			t.Errorf("done event leaked %q key", bad)
		}
	}
}
