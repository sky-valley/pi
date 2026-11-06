package ai

import (
	"encoding/json"
	"math/big"
	"os"
	"reflect"
	"testing"
)

// validationErrorsCaptureFile is what testdata/validation-errors/capture.mjs
// recorded from pi-ai's validateToolCall / validateToolArguments at src
// 55a5c6914 (pi-ai on pi-schema since 6536f8012; no build ships it yet).
const validationErrorsCaptureFile = "testdata/validation-errors/validation-errors-55a5c6914.json"

// validationProbesCaptureFile is what testdata/validation-probes/capture.mjs
// recorded from upstream's validation-probes.test.ts at 55a5c6914.
const validationProbesCaptureFile = "testdata/validation-probes/validation-probes-55a5c6914.json"

// knownDivergentRows are capture rows the port answers differently, keyed by
// "<schema> <arguments>", with the reason. The test asserts the port still
// differs there, so a fix shows up.
var knownDivergentRows = map[string]string{
	// RE2 has no lookahead: such a pattern matches every string (package
	// schema's documented divergence), so pi rejects "b" and the port does not.
	`{"type":"object","properties":{"v":{"type":"string","pattern":"^(?=.*a).*$"}},"required":["v"]} {"v":"b"}`: "RE2 lookahead",
	// pi returns parse()'s value, here [{}]; ValidateToolArguments returns
	// the arguments as a map, so a schema that coerces them to a non-object
	// is refused with an error instead of answered with a nil map.
	`{"type":"array"} {}`: "non-object root",
}

// reparseJSON is v as the captures write a returned value:
// JSON.stringify with a bigint as its digits and "n" (capture.mjs's
// replacer), read back.
func reparseJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(bigintsAsText(v))
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func bigintsAsText(v any) any {
	switch x := v.(type) {
	case *big.Int:
		return x.String() + "n"
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = bigintsAsText(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = bigintsAsText(e)
		}
		return out
	}
	return v
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
		if reason, known := knownDivergentRows[key]; known {
			if matches {
				t.Errorf("%s now matches pi (was: %s); drop it from knownDivergentRows", key, reason)
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
			// ai.Schema has no form for a boolean subschema (K41).
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

// A decoded schema is validated as the text it came from, which ai.Schema's
// typed fields cannot always hold (K41), until it is changed: then as it
// marshals. A schema built in Go has no text.
func TestSchemaTextFollowsTheDecodedSourceUntilChanged(t *testing.T) {
	const src = `{"type":"object","properties":{"v":{"type":["null","string"]}}}`
	var s Schema
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	if got, _ := schemaText(&s); string(got) != src {
		t.Errorf("unchanged: schemaText = %s, want the source %s", got, src)
	}
	if got, _ := schemaText(s.Properties["v"]); string(got) == `{"type":["null","string"]}` {
		t.Errorf("a nested schema kept its own text: %s", got)
	}
	cp := s.Clone()
	cp.Description = "changed"
	want, _ := json.Marshal(cp)
	if got, _ := schemaText(cp); string(got) != string(want) {
		t.Errorf("changed: schemaText = %s, want the marshaled %s", got, want)
	}
	built := Object(Prop("v", String()))
	want, _ = json.Marshal(built)
	if got, _ := schemaText(built); string(got) != string(want) {
		t.Errorf("built: schemaText = %s, want %s", got, want)
	}
}

// One *Schema compiles once (pi's validatorCache).
func TestValidatorForCachesPerSchema(t *testing.T) {
	s := Object(Prop("v", String()))
	a, err := validatorFor(s)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := validatorFor(s)
	if a != b {
		t.Error("the same *Schema compiled twice")
	}
	if c, _ := validatorFor(Object(Prop("v", String()))); c == a {
		t.Error("two schemas share a validator")
	}
}
