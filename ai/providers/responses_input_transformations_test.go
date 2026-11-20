package providers

import (
	"encoding/json"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// runResponsesTerminal streams one terminal response event carrying
// inputTransformations (raw JSON, "" for absent) through the Responses
// processor, as pi's openai-responses-input-transformations.test.ts does.
func runResponsesTerminal(t *testing.T, inputTransformations, status string) *ai.AssistantMessage {
	t.Helper()
	eventType, extra := "response.completed", ""
	if status == "incomplete" {
		eventType, extra = "response.incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`
	}
	field := ""
	if inputTransformations != "" {
		field = `,"input_transformations":` + inputTransformations
	}
	sse := `data: {"type":"` + eventType + `","sequence_number":0,"response":{"id":"resp_transformations","status":"` + status + `","output":[]` + extra + field + `}}

`
	return runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
}

func diagnosticsJSON(t *testing.T, msg *ai.AssistantMessage) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, d := range msg.Diagnostics {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

const (
	prefixMismatchJSON       = `{"type":"reasoning","id":"rs_edited","action":"removed","reason":"prefix_mismatch"}`
	organizationMismatchJSON = `{"type":"reasoning","id":"rs_org","action":"removed","reason":"organization_mismatch"}`
	turnBoundaryJSON         = `{"type":"reasoning","id":"rs_old_turn","action":"removed","reason":"turn_boundary"}`
)

// Upstream 03de604b8: removed reasoning items (but turn_boundary ones) become
// an openai_input_transformations diagnostic, each entry {type, id, action, reason}.
func TestResponsesRecordsReasoningRemovals(t *testing.T) {
	msg := runResponsesTerminal(t, "["+prefixMismatchJSON+","+turnBoundaryJSON+","+organizationMismatchJSON+"]", "completed")
	if msg.StopReason != ai.StopStop {
		t.Fatalf("stopReason = %q (%s)", msg.StopReason, msg.ErrorMessage)
	}
	if len(msg.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", msg.Diagnostics)
	}
	d := msg.Diagnostics[0]
	if d.Type != "openai_input_transformations" || d.Timestamp <= 0 || d.Error != nil {
		t.Fatalf("diagnostic = %+v", d)
	}
	details, err := json.Marshal(d.Details)
	if err != nil {
		t.Fatal(err)
	}
	// Byte-exact, key order included: pi writes the destructured literal.
	want := `{"transformations":[` + prefixMismatchJSON + `,` + organizationMismatchJSON + `]}`
	if string(details) != want {
		t.Fatalf("details =\n%s\nwant\n%s", details, want)
	}
}

func TestResponsesRecordsReasoningRemovalsOnIncomplete(t *testing.T) {
	msg := runResponsesTerminal(t, "["+prefixMismatchJSON+"]", "incomplete")
	if msg.StopReason != ai.StopLength {
		t.Fatalf("stopReason = %q", msg.StopReason)
	}
	got := diagnosticsJSON(t, msg)
	if len(got) != 1 || got[0]["type"] != "openai_input_transformations" {
		t.Fatalf("diagnostics = %v", got)
	}
}

// pi keeps `{ type, id, action, reason }` of each kept entry: members in that
// order, extra members dropped, an absent one omitted (undefined) and a null
// one kept, and any reason but "turn_boundary" counts, absent included.
func TestResponsesReasoningRemovalEntryShape(t *testing.T) {
	msg := runResponsesTerminal(t, `[{"reason":"future_reason","extra":1,"action":"removed","id":null,"type":"reasoning"},{"type":"reasoning","action":"removed","id":7}]`, "completed")
	if len(msg.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v", msg.Diagnostics)
	}
	details, _ := json.Marshal(msg.Diagnostics[0].Details)
	want := `{"transformations":[{"type":"reasoning","id":null,"action":"removed","reason":"future_reason"},{"type":"reasoning","id":7,"action":"removed"}]}`
	if string(details) != want {
		t.Fatalf("details =\n%s\nwant\n%s", details, want)
	}
}

func TestResponsesIgnoresOtherTransformations(t *testing.T) {
	for _, report := range []string{
		"", "null", "[]",
		"[" + turnBoundaryJSON + "," + turnBoundaryJSON + "]",
		`{"malformed":true}`,
		`[null,"rs_text",{"type":"message","id":"msg_1","action":"removed","reason":"prefix_mismatch"}]`,
		`[{"type":"reasoning","id":"rs_changed","action":"changed","reason":"prefix_mismatch"}]`,
	} {
		msg := runResponsesTerminal(t, report, "completed")
		if msg.StopReason != ai.StopStop {
			t.Fatalf("%s: stopReason = %q (%s)", report, msg.StopReason, msg.ErrorMessage)
		}
		if msg.Diagnostics != nil {
			t.Fatalf("%s: diagnostics = %+v, want none", report, msg.Diagnostics)
		}
	}
}
