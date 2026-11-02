package ai

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// validationErrorsCaptureFile is what testdata/validation-errors/capture.mjs
// recorded from pi-ai's validateToolCall / validateToolArguments at src
// 6536f8012 (the first pi-ai on pi-schema; no build ships it yet).
const validationErrorsCaptureFile = "testdata/validation-errors/validation-errors-6536f8012.json"

// validationProbesCaptureFile is what testdata/validation-probes/capture.mjs
// recorded from upstream's validation-probes.test.ts at 6536f8012.
const validationProbesCaptureFile = "testdata/validation-probes/validation-probes-6536f8012.json"

// regexDivergentRows are capture rows whose pattern RE2 cannot express (a
// lookahead): pi rejects the value and the port, which lets such a pattern
// match every string (package schema's documented divergence), accepts it.
// The test asserts the port still differs there, so a fix shows up.
var regexDivergentRows = map[string]bool{
	`{"type":"object","properties":{"v":{"type":"string","pattern":"^(?=.*a).*$"}},"required":["v"]} {"v":"b"}`: true,
}

func reparseJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeCapturedValue(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("captured value %s: %v", text, err)
	}
	return v
}

// The validation error is the tool result the model reads, so its text is
// pi's to the byte: the tool name between plain quotes (no Go escaping),
// pi-schema's messages at dotted paths in pi-schema's order, and the received
// arguments as JSON.stringify(arguments, null, 2) writes them. A call that
// validates returns pi's coerced arguments.
func TestValidationErrorTextMatchesPi(t *testing.T) {
	data, err := os.ReadFile(validationErrorsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Parameters json.RawMessage `json:"parameters"`
		Rows       []struct {
			Name       string   `json:"name"`
			Parameters string   `json:"parameters"`
			Args       string   `json:"args"`
			Tools      []string `json:"tools"`
			Value      *string  `json:"value"`
			Error      string   `json:"error"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", validationErrorsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", validationErrorsCaptureFile)
	}
	for _, row := range capture.Rows {
		params := string(capture.Parameters)
		if row.Parameters != "" {
			params = row.Parameters
		}
		var schema Schema
		if err := json.Unmarshal([]byte(params), &schema); err != nil {
			t.Fatal(err)
		}
		args, order, err := DecodeOrderedObject([]byte(row.Args))
		if err != nil {
			t.Fatal(err)
		}
		tc := ToolCall{ID: "1", Name: row.Name, Arguments: args, ArgumentsOrder: order}
		var got map[string]any
		if row.Tools != nil {
			var tools []Tool
			for _, name := range row.Tools {
				tools = append(tools, Tool{Name: name, Description: "d", Parameters: &schema})
			}
			got, err = ValidateToolCall(tools, tc)
		} else {
			got, err = ValidateToolArguments(Tool{Name: row.Name, Description: "d", Parameters: &schema}, tc)
		}
		key := params + " " + row.Args
		matches := false
		switch {
		case row.Value != nil:
			matches = err == nil && reflect.DeepEqual(reparseJSON(t, got), decodeCapturedValue(t, *row.Value))
		default:
			matches = err != nil && err.Error() == row.Error
		}
		if regexDivergentRows[key] {
			if matches {
				t.Errorf("%s: now matches pi; drop it from regexDivergentRows", key)
			}
			continue
		}
		if !matches {
			want := row.Error
			if row.Value != nil {
				want = *row.Value
			}
			t.Errorf("%q %s %s:\ngot   %v %v\npi    %s", row.Name, params, row.Args, got, err, want)
		}
	}
}

// probeDivergences are upstream's reviewer probes the port answers
// differently, by the reason each cannot match (see the test).
var probeDivergences = map[string]string{
	// ai.Schema keeps one non-null member of a "type" array (K40), so
	// ["string","number"] reaches the validator as "number" and true coerces
	// to 1 where pi's first branch makes it "true".
	`type-array-multi {"v":true}`: "K40: a type array collapses to its last non-null type",
	// A lone surrogate escape decodes to U+FFFD (K39), so the received
	// arguments echo "\uFFFDA" where pi's JSON.stringify writes "\\ud800A".
	`unpaired high surrogate, maxLength 1 {"s":"\ud800A"}`: "K39: a lone surrogate decodes as U+FFFD",
}

// undecodableProbes are probes whose schema holds a boolean subschema, which
// ai.Schema cannot represent, so a tool with it cannot be declared at all.
var undecodableProbes = map[string]bool{
	`mcp-boolean-true-prop {"a":1}`: true,
	`mcp-boolean-true-prop {"b":1}`: true,
}

// TestValidationProbesMatchPi replays upstream's validation-probes.test.ts:
// TypeBox-built and MCP-shaped JSON tool schemas, each read here from the
// JSON pi's schema object serializes to, as any tool schema decoded from JSON
// reaches the port.
func TestValidationProbesMatchPi(t *testing.T) {
	data, err := os.ReadFile(validationProbesCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name       string  `json:"name"`
			Parameters string  `json:"parameters"`
			Args       string  `json:"args"`
			Value      *string `json:"value"`
			Error      string  `json:"error"`
			Mutated    bool    `json:"mutated"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", validationProbesCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", validationProbesCaptureFile)
	}
	seen := map[string]bool{}
	for i, row := range capture.Rows {
		if row.Mutated {
			// The schema object changed after pi cached its validator; the
			// JSON at the call is not the schema pi validated against.
			continue
		}
		key := row.Name + " " + row.Args
		var schema Schema
		if err := json.Unmarshal([]byte(row.Parameters), &schema); err != nil {
			// ai.Schema has no form for a boolean subschema (K40).
			seen[key] = true
			if _, known := undecodableProbes[key]; !known {
				t.Errorf("probe %d %s: schema does not decode: %v", i, key, err)
			}
			continue
		}
		if _, known := undecodableProbes[key]; known {
			t.Errorf("probe %s now decodes; drop it from undecodableProbes", key)
		}
		args, order, err := DecodeOrderedObject([]byte(row.Args))
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		got, err := ValidateToolArguments(Tool{Name: row.Name, Parameters: &schema}, ToolCall{ID: "1", Name: row.Name, Arguments: args, ArgumentsOrder: order})
		matches := false
		if row.Value != nil {
			matches = err == nil && reflect.DeepEqual(reparseJSON(t, got), decodeCapturedValue(t, *row.Value))
		} else {
			matches = err != nil && err.Error() == row.Error
		}
		if reason, known := probeDivergences[key]; known {
			seen[key] = true
			if matches {
				t.Errorf("probe %s now matches pi (was: %s); drop it from probeDivergences", key, reason)
			}
			continue
		}
		if !matches {
			want := row.Error
			if row.Value != nil {
				want = *row.Value
			}
			t.Errorf("probe %d %s\nschema %s\ngot    %v %v\npi     %s", i, key, truncate(row.Parameters, 300), got, err, truncate(want, 400))
		}
	}
	for key := range undecodableProbes {
		if !seen[key] {
			t.Errorf("undecodableProbes lists %s, which the capture no longer holds", key)
		}
	}
	for key := range probeDivergences {
		if !seen[key] {
			t.Errorf("probeDivergences lists %s, which the capture no longer holds", key)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
