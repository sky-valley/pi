package ai

import (
	"encoding/json"
	"testing"
)

func TestSchemaMarshalDeterministic(t *testing.T) {
	s := Object(
		Prop("command", String("the shell command")),
		Opt("timeout", Integer("timeout in ms")),
	)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"object","properties":{"command":{"type":"string","description":"the shell command"},"timeout":{"type":"integer","description":"timeout in ms"}},"required":["command"]}`
	if string(raw) != want {
		t.Fatalf("schema JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

func TestValidateToolArgumentsCoercesAndValidates(t *testing.T) {
	tool := Tool{
		Name:       "calc",
		Parameters: Object(Prop("n", Integer()), Prop("flag", Boolean())),
	}
	// LLM passed strings for typed fields; coercion should fix them.
	tc := ToolCall{Name: "calc", Arguments: map[string]any{"n": "42", "flag": "true"}}
	args, err := ValidateToolArguments(tool, tc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f, ok := args["n"].(float64); !ok || f != 42 {
		t.Fatalf("n not coerced to number: %#v", args["n"])
	}
	if b, ok := args["flag"].(bool); !ok || !b {
		t.Fatalf("flag not coerced to bool: %#v", args["flag"])
	}
}

func TestValidateToolArgumentsMissingRequired(t *testing.T) {
	tool := Tool{Name: "calc", Parameters: Object(Prop("n", Integer()))}
	tc := ToolCall{Name: "calc", Arguments: map[string]any{}}
	_, err := ValidateToolArguments(tool, tc)
	if err == nil {
		t.Fatal("expected validation error for missing required field")
	}
}

func TestValidateToolArgumentsWrongType(t *testing.T) {
	tool := Tool{Name: "calc", Parameters: Object(Prop("items", ArrayOf(String())))}
	tc := ToolCall{Name: "calc", Arguments: map[string]any{"items": "not-an-array"}}
	_, err := ValidateToolArguments(tool, tc)
	if err == nil {
		t.Fatal("expected validation error for wrong type")
	}
}

func TestJSNumberToString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1000000, "1000000"},
		{0.0000001, "1e-7"},
		{1e21, "1e+21"},
		{1e20, "100000000000000000000"},
		{1e-6, "0.000001"},
		{1e-7, "1e-7"},
		{123.456, "123.456"},
		{-0.5, "-0.5"},
		{0, "0"},
		{42, "42"},
		{1.5e22, "1.5e+22"},
		{1234567890123456789, "1234567890123456800"},
	}
	for _, c := range cases {
		if got := jsNumberToString(c.in); got != c.want {
			t.Errorf("jsNumberToString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStringCoercionMatchesJSNumberFormat mirrors pi validation.ts:135 String(value).
func TestStringCoercionMatchesJSNumberFormat(t *testing.T) {
	tool := Tool{Name: "echo", Parameters: Object(Prop("value", String()))}
	cases := []struct {
		in   float64
		want string
	}{
		{1000000, "1000000"},
		{0.0000001, "1e-7"},
	}
	for _, c := range cases {
		tc := ToolCall{Name: "echo", Arguments: map[string]any{"value": c.in}}
		args, err := ValidateToolArguments(tool, tc)
		if err != nil {
			t.Fatalf("unexpected error for %v: %v", c.in, err)
		}
		if got, _ := args["value"].(string); got != c.want {
			t.Errorf("coerce %v → %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUnionCoercionCases mirrors pi validation.test.ts union cases.
func TestUnionCoercionCases(t *testing.T) {
	cases := []struct {
		types []string
		in    any
		want  any
	}{
		{[]string{"number", "string"}, "1", "1"},
		{[]string{"boolean", "number"}, "1", float64(1)},
	}
	for _, c := range cases {
		// Build the value schema via raw JSON so type:[".."] is parsed properly.
		raw := `{"type":["` + c.types[0] + `","` + c.types[1] + `"]}`
		var vs Schema
		if err := json.Unmarshal([]byte(raw), &vs); err != nil {
			t.Fatal(err)
		}
		tool := Tool{Name: "echo", Parameters: Object(Prop("value", &vs))}
		tc := ToolCall{Name: "echo", Arguments: map[string]any{"value": c.in}}
		args, err := ValidateToolArguments(tool, tc)
		if err != nil {
			t.Fatalf("union %v input %v: unexpected error: %v", c.types, c.in, err)
		}
		if got := args["value"]; got != c.want {
			t.Errorf("union %v input %v → %#v, want %#v", c.types, c.in, got, c.want)
		}
	}
}

// TestAdditionalPropertiesFalseRejectsExtraKey mirrors TypeBox .Check rejecting
// extra keys when additionalProperties:false.
func TestAdditionalPropertiesFalseRejectsExtraKey(t *testing.T) {
	deny := false
	params := Object(Prop("n", Integer()))
	params.AdditionalAllowed = &deny

	tool := Tool{Name: "calc", Parameters: params}
	tc := ToolCall{Name: "calc", Arguments: map[string]any{"n": float64(1), "extra": "nope"}}
	_, err := ValidateToolArguments(tool, tc)
	if err == nil {
		t.Fatal("expected validation error for extra key with additionalProperties:false")
	}

	// Without the extra key it should pass.
	tc2 := ToolCall{Name: "calc", Arguments: map[string]any{"n": float64(1)}}
	if _, err := ValidateToolArguments(tool, tc2); err != nil {
		t.Fatalf("unexpected error without extra key: %v", err)
	}
}

func TestSchemaRoundTripPreservesOrder(t *testing.T) {
	s := Object(
		Prop("a", String()),
		Prop("b", Integer()),
		Prop("c", Boolean()),
	)
	raw, _ := json.Marshal(s)
	var back Schema
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	raw2, _ := json.Marshal(&back)
	if string(raw) != string(raw2) {
		t.Fatalf("round-trip changed schema:\n %s\n %s", raw, raw2)
	}
}
