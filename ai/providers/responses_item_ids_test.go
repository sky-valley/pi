package providers

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// itemIDsCaptureFile is what testdata/responses-item-ids/capture.mjs recorded
// from pi-ai 1.3.0: the openai-responses request input replaying a tool call
// whose id carries an fc_, ctc_, other or no item id, with grammar tool
// support on and off, from the same model, another model of the provider and
// a foreign provider's model.
const itemIDsCaptureFile = "testdata/responses-item-ids/item-ids-1.3.0.json"

// A replayed call keeps its item id only when the id's prefix matches the item
// type it replays as — fc_ for a function_call, ctc_ for a custom_tool_call —
// and never for a reply from another model of the provider (upstream
// bc2d8dc1c). A foreign provider's ids are normalized to fc_ first, so a
// grammar tool's call from one must lose it (earendil-works/radius#115).
func TestResponsesReplayedItemIDsMatchPi(t *testing.T) {
	data, err := os.ReadFile(itemIDsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	type origin struct{ API, Provider, Model string }
	var capture struct {
		Origins map[string]origin `json:"origins"`
		Rows    []struct {
			Grammar bool   `json:"grammar"`
			Origin  string `json:"origin"`
			ID      string `json:"id"`
			Input   string `json:"input"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", itemIDsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", itemIDsCaptureFile)
	}
	for _, row := range capture.Rows {
		model := reasoningModel()
		if row.Grammar {
			model.Compat = json.RawMessage(`{"supportsOpenAIGrammarTools":true}`)
		}
		o, ok := capture.Origins[row.Origin]
		if !ok {
			t.Fatalf("row origin %q is not among the capture's origins", row.Origin)
		}
		req := ai.Context{
			Messages: []ai.Message{
				ai.NewUserText("hi", 0),
				ai.AssistantMessage{
					Content:    ai.ContentList{ai.ToolCall{ID: row.ID, Name: "gram", Arguments: map[string]any{"query": "SELECT 1"}}},
					Api:        o.API,
					Provider:   o.Provider,
					Model:      o.Model,
					StopReason: ai.StopToolUse,
				},
				ai.ToolResultMessage{ToolCallID: row.ID, ToolName: "gram", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
			},
			Tools: []ai.Tool{grammarSamplingTool()},
		}
		got, err := json.Marshal(mustResponsesInput(t, model, req))
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		_ = json.Unmarshal(got, &gotValue)
		if err := json.Unmarshal([]byte(row.Input), &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("grammar %v, %s reply, id %q: input\n%s\npi\n%s", row.Grammar, row.Origin, row.ID, got, row.Input)
		}
	}
}
