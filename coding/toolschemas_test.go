package coding

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// TestBuiltinToolSchemasMatchPi compares every built-in tool's parameter and
// output schemas with pi's, byte for byte (testdata/toolschemas, captured from
// pi-coding-agent 1.3.0's createToolDefinition). The parameters reach every
// request's tool declarations, so key order and types are model-visible; the
// output schema describes the structured content programmatic callers such as
// codemode scripts get, and a tool without one declares none.
func TestBuiltinToolSchemasMatchPi(t *testing.T) {
	raw, err := os.ReadFile("testdata/toolschemas/toolschemas-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name         string          `json:"name"`
			Parameters   json.RawMessage `json:"parameters"`
			OutputSchema json.RawMessage `json:"outputSchema"`
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
			if row.OutputSchema == nil {
				if tool.OutputSchema != nil {
					t.Errorf("outputSchema: pi declares none, got %s", mustMarshal(t, tool.OutputSchema))
				}
				return
			}
			want.Reset()
			if err := json.Compact(&want, row.OutputSchema); err != nil {
				t.Fatal(err)
			}
			if tool.OutputSchema == nil {
				t.Fatalf("outputSchema: none declared, pi %s", want.String())
			}
			if got := mustMarshal(t, tool.OutputSchema); got != want.String() {
				t.Errorf("outputSchema\n got %s\n pi  %s", got, want.String())
			}
		})
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
