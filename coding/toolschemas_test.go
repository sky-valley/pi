package coding

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// TestBuiltinToolSchemasMatchPi compares every built-in tool's parameter schema
// with the one pi sends, byte for byte (testdata/toolschemas, captured from
// pi-coding-agent 0.99.2's createToolDefinition). The schemas reach every
// request's tool declarations, so key order and types are model-visible.
func TestBuiltinToolSchemasMatchPi(t *testing.T) {
	raw, err := os.ReadFile("testdata/toolschemas/toolschemas-0.99.2.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) == 0 {
		t.Fatal("empty capture")
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			tool, err := CreateTool(row.Name, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(tool.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := json.Compact(&want, row.Parameters); err != nil {
				t.Fatal(err)
			}
			if string(got) != want.String() {
				t.Errorf("parameters\n got %s\n pi  %s", got, want.String())
			}
		})
	}
}
