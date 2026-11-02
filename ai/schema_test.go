package ai

import (
	"encoding/json"
	"reflect"
	"strings"
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
	// TypeBox's serialization of the same Type.Object, from pi-ai 0.99.2's
	// typebox: type, required, properties.
	want := `{"type":"object","required":["command"],"properties":{"command":{"type":"string","description":"the shell command"},"timeout":{"type":"integer","description":"timeout in ms"}}}`
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

// Port of "treats null as omission for optional non-nullable properties"
// (validation.test.ts, upstream 7915cdac6): strict constrained sampling makes
// models emit every property, so a null for an optional property whose schema
// rejects null is deleted before coercion; nullable optionals keep their null.
func TestValidateToolArgumentsTreatsNullAsOmission(t *testing.T) {
	tool := Tool{
		Name:        "echo",
		Description: "Echo tool",
		Parameters: Object(
			Prop("path", String()),
			Opt("offset", Number()),
			Opt("nullable", &Schema{AnyOf: []*Schema{{Type: "string"}, {Type: "null"}}}),
			Prop("metadata", Object(Opt("enabled", Boolean()))),
		),
	}
	tc := ToolCall{
		ID: "tool-1", Name: "echo",
		Arguments: map[string]any{"path": "file.txt", "offset": nil, "nullable": nil, "metadata": map[string]any{"enabled": nil}},
	}

	args, err := ValidateToolArguments(tool, tc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := json.Marshal(args)
	want := `{"metadata":{},"nullable":null,"path":"file.txt"}`
	if string(got) != want {
		t.Fatalf("args = %s, want %s", got, want)
	}
}

// Port of "preserves optional nulls whose referenced schema is nullable"
// (validation.test.ts, upstream 7915cdac6): a $ref property is never treated
// as omission — pi cannot ask the compiled validator about an unresolved
// reference, so the null is kept.
func TestValidateToolArgumentsPreservesRefNull(t *testing.T) {
	var params Schema
	if err := json.Unmarshal([]byte(
		`{"type":"object","properties":{"value":{"$ref":"#/$defs/value"}},"$defs":{"value":{"anyOf":[{"type":"number"},{"type":"null"}]}}}`,
	), &params); err != nil {
		t.Fatal(err)
	}
	tool := Tool{Name: "echo", Description: "Echo tool", Parameters: &params}
	tc := ToolCall{ID: "tool-1", Name: "echo", Arguments: map[string]any{"value": nil}}

	args, err := ValidateToolArguments(tool, tc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, present := args["value"]; !present || v != nil {
		t.Fatalf("value must stay an explicit null: %#v", args)
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

// TestNullableUnionPreservesMatchingValue mirrors pi validation.test.ts: a value
// that already matches an anyOf/oneOf arm survives coercion untouched, while a
// value matching no arm is still coerced.
func TestNullableUnionPreservesMatchingValue(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		in     any
		want   any
	}{
		{"anyOf null", `{"anyOf":[{"type":"number"},{"type":"null"}]}`, nil, nil},
		{"oneOf null", `{"oneOf":[{"type":"number"},{"type":"null"}]}`, nil, nil},
		{"anyOf coerces non-matching", `{"anyOf":[{"type":"number"},{"type":"null"}]}`, "42", float64(42)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var vs Schema
			if err := json.Unmarshal([]byte(c.schema), &vs); err != nil {
				t.Fatal(err)
			}
			tool := Tool{Name: "echo", Parameters: Object(Prop("value", &vs))}
			tc := ToolCall{Name: "echo", Arguments: map[string]any{"value": c.in}}
			args, err := ValidateToolArguments(tool, tc)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := args["value"]; got != c.want {
				t.Errorf("value %#v → %#v, want %#v", c.in, got, c.want)
			}
		})
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

// TestKeywordRoundTrip: promoted keywords must survive unmarshal → marshal
// (they previously round-tripped through Extra; now they are real fields).
func TestKeywordRoundTrip(t *testing.T) {
	src := `{"type":"object","properties":{"v":{"type":"number","minimum":1,"maximum":10,"exclusiveMinimum":0,"exclusiveMaximum":11,"multipleOf":0.5},"s":{"type":"string","minLength":1,"maxLength":4,"pattern":"^x"},"a":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":3},"c":{"type":"string","const":"k"}},"required":["v"]}`
	var s Schema
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"minimum":1`, `"maximum":10`, `"exclusiveMinimum":0`, `"exclusiveMaximum":11`, `"multipleOf":0.5`, `"minLength":1`, `"maxLength":4`, `"pattern":"^x"`, `"minItems":1`, `"maxItems":3`, `"const":"k"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("round-trip lost %s: %s", key, raw)
		}
	}
	if s.Properties["v"].Extra != nil {
		t.Errorf("promoted keywords still landing in Extra: %#v", s.Properties["v"].Extra)
	}
}

// TestSchemaCloneNoAliasing: Clone promises that nothing reachable from the
// copy aliases the receiver. The fixture must populate every reference-typed
// field (pointer/map/slice/any) with a non-zero value — the test fails on any
// zero reference field, so adding a field to Schema without teaching Clone
// (and this fixture) about it fails here instead of silently shallow-copying.
func TestSchemaCloneNoAliasing(t *testing.T) {
	b, f, i := true, 1.5, 2
	src := &Schema{
		Type:              "object",
		Description:       "d",
		Properties:        map[string]*Schema{"p": {Type: "string"}},
		PropertyOrder:     []string{"p"},
		Required:          []string{"p"},
		Items:             &Schema{Type: "number"},
		Enum:              []any{"a", []any{"nested"}},
		Default:           map[string]any{"k": "v"},
		AdditionalAllowed: &b,
		AdditionalSchema:  &Schema{Type: "string"},
		Minimum:           &f,
		Maximum:           &f,
		ExclusiveMinimum:  &f,
		ExclusiveMaximum:  &f,
		MultipleOf:        &f,
		MinLength:         &i,
		MaxLength:         &i,
		Pattern:           "^x",
		MinItems:          &i,
		MaxItems:          &i,
		Const:             []any{"c"},
		HasConst:          true,
		Format:            "uuid",
		Nullable:          true,
		AnyOf:             []*Schema{{Type: "string"}},
		OneOf:             []*Schema{{Type: "number"}},
		AllOf:             []*Schema{{Type: "boolean"}},
		Extra:             map[string]any{"x-note": []any{"e"}},
		KeywordOrder:      []string{"type", "properties"},
	}

	sv := reflect.ValueOf(src).Elem()
	for idx := 0; idx < sv.NumField(); idx++ {
		if sv.Field(idx).IsZero() {
			t.Fatalf("fixture leaves Schema.%s zero; populate it so aliasing is checked", sv.Type().Field(idx).Name)
		}
	}

	cp := src.Clone()
	cv := reflect.ValueOf(cp).Elem()
	for idx := 0; idx < sv.NumField(); idx++ {
		name := sv.Type().Field(idx).Name
		a, c := sv.Field(idx), cv.Field(idx)
		if a.Kind() == reflect.Interface {
			a, c = a.Elem(), c.Elem()
		}
		switch a.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice:
			if a.Pointer() == c.Pointer() {
				t.Errorf("Clone aliases Schema.%s", name)
			}
		}
	}
	if src.Properties["p"] == cp.Properties["p"] {
		t.Error("Clone aliases Properties values")
	}
	if &src.Enum[1] == &cp.Enum[1] || reflect.ValueOf(src.Enum[1]).Pointer() == reflect.ValueOf(cp.Enum[1]).Pointer() {
		t.Error("Clone aliases Enum elements")
	}
	if reflect.ValueOf(src.Extra["x-note"]).Pointer() == reflect.ValueOf(cp.Extra["x-note"]).Pointer() {
		t.Error("Clone aliases Extra values")
	}
	if got, want := mustJSON(t, cp), mustJSON(t, src); got != want {
		t.Errorf("Clone changed schema:\n got: %s\nwant: %s", got, want)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestSchemaRoundTripPreservesPresence: marshal must emit `properties` and
// `required` exactly when the source schema carried the key — pi serializes
// whatever keys the object has, so `{"type":"object"}` must not grow a
// properties key and an explicit `required: []` must survive. Load-bearing for
// strict tool schema conversion (upstream 7915cdac6), which emits
// required: [] for zero-property objects.
func TestSchemaRoundTripPreservesPresence(t *testing.T) {
	for _, src := range []string{
		`{"type":"object"}`,
		`{"type":"object","required":[]}`,
		`{"type":"object","properties":{}}`,
		`{"type":"object","properties":{},"required":[]}`,
	} {
		var s Schema
		if err := json.Unmarshal([]byte(src), &s); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(&s)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != src {
			t.Errorf("round-trip changed schema:\n got: %s\nwant: %s", raw, src)
		}
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

// Port of "reports malformed arguments JSON instead of validating empty
// arguments" (validation.test.ts, upstream 87f9a3641): a call whose arguments
// text was not a JSON object fails validation with its argumentsError, before
// the (empty) arguments are looked at — even against a schema they satisfy,
// and against a tool that declares no parameters.
func TestValidateToolArgumentsReportsArgumentsError(t *testing.T) {
	tc := ToolCall{ID: "tool-1", Name: "echo", Arguments: map[string]any{}, ArgumentsError: "Unexpected token"}
	for _, tool := range []Tool{
		{Name: "echo", Description: "Echo tool", Parameters: Object(Opt("value", String()))},
		{Name: "echo", Description: "Echo tool"},
	} {
		_, err := ValidateToolArguments(tool, tc)
		if err == nil || err.Error() != `Invalid arguments for tool "echo": Unexpected token` {
			t.Errorf("ValidateToolArguments = %v, want pi's Invalid arguments for tool \"echo\": Unexpected token", err)
		}
	}
}
