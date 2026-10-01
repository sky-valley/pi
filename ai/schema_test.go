package ai

import (
	"encoding/json"
	"math"
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

func TestValidateToolArgumentsWrongType(t *testing.T) {
	tool := Tool{Name: "calc", Parameters: Object(Prop("items", ArrayOf(String())))}
	tc := ToolCall{Name: "calc", Arguments: map[string]any{"items": "not-an-array"}}
	_, err := ValidateToolArguments(tool, tc)
	if err == nil {
		t.Fatal("expected validation error for wrong type")
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

// TestJSNumber locks jsNumber to ECMA-262 StringToNumber. Every vector below
// was verified against node (Number(s) with Number.isFinite/isInteger gates).
func TestJSNumber(t *testing.T) {
	inf := math.Inf(1)
	cases := []struct {
		in   string
		want float64
		ok   bool // ok=false means NaN
	}{
		{" 12 ", 12, true},
		{"5", 5, true},
		{"01", 1, true},
		{"08", 8, true},
		{"0x10", 16, true},
		{"0X10", 16, true},
		{"0xFF", 255, true},
		{"0b101", 5, true},
		{"0B11", 3, true},
		{"0o17", 15, true},
		{"0O7", 7, true},
		{"+5", 5, true},
		{"-5", -5, true},
		{"1e3", 1000, true},
		{"1E3", 1000, true},
		{"1e+3", 1000, true},
		{"1.2e-3", 0.0012, true},
		{".5", 0.5, true},
		{"-.5", -0.5, true},
		{"+.5", 0.5, true},
		{"5.", 5, true},
		{"1.", 1, true},
		{"", 0, true},   // Number("") === 0
		{"  ", 0, true}, // Number("  ") === 0
		{" \t\n12 ", 12, true},
		{"\u00a05\u00a0", 5, true}, // NBSP is JS whitespace
		{"\ufeff5", 5, true},       // ZWNBSP/BOM is JS whitespace
		{"Infinity", inf, true},
		{"-Infinity", -inf, true},
		{" Infinity ", inf, true},
		{"1e1000", inf, true}, // overflow → Infinity (then isFinite gates)
		// NaN vectors (ok=false):
		{"1_0", 0, false},
		{"0b1_0", 0, false},
		{"NaN", 0, false},
		{"0x", 0, false},
		{"0b12", 0, false},
		{"0o18", 0, false},
		{"-0x10", 0, false},
		{"+0x10", 0, false},
		{"12abc", 0, false},
		{"0x1p4", 0, false},
		{"0.5.5", 0, false},
		{"+", 0, false},
		{"-", 0, false},
		{".", 0, false},
		{"e3", 0, false},
		{"\u00855", 0, false}, // NEL is NOT JS whitespace (Go unicode.IsSpace says yes)
		{"\u200b5", 0, false}, // ZWSP is not whitespace
	}
	for _, c := range cases {
		got, ok := jsNumber(c.in)
		if ok != c.ok {
			t.Errorf("jsNumber(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("jsNumber(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNumberCoercionJSSemantics locks string→number coercion to pi
// validation.ts:83-92 (Number(value) gated by Number.isFinite, with the
// trim-guard against ""→0). Vectors node-verified.
func TestNumberCoercionJSSemantics(t *testing.T) {
	num := Number()
	cases := []struct {
		in   string
		want any // string means "not coerced"
	}{
		{" 12 ", float64(12)},
		{"0x10", float64(16)},
		{"0b101", float64(5)},
		{"0o17", float64(15)},
		{"+5", float64(5)},
		{"1e3", float64(1000)},
		{".5", 0.5},
		{"5.", float64(5)},
		{"1_0", "1_0"},           // JS Number("1_0") is NaN
		{"Infinity", "Infinity"}, // parses in JS but Number.isFinite rejects
		{"-Infinity", "-Infinity"},
		{"1e1000", "1e1000"}, // overflow → Infinity → rejected
		{"NaN", "NaN"},
		{"", ""},     // trim-guard: empty never coerces (would be 0 in JS)
		{"  ", "  "}, // whitespace-only never coerces
		{"12abc", "12abc"},
	}
	for _, c := range cases {
		if got := num.Coerce(c.in); got != c.want {
			t.Errorf("number Coerce(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// TestIntegerCoercionJSSemantics locks pi validation.ts:93-101
// (Number.isInteger gate).
func TestIntegerCoercionJSSemantics(t *testing.T) {
	integer := Integer()
	cases := []struct {
		in   string
		want any
	}{
		{" 12 ", float64(12)},
		{"0x10", float64(16)},
		{"5.", float64(5)},
		{"1e3", float64(1000)},
		{".5", ".5"}, // 0.5 is not an integer
		{"1.5", "1.5"},
		{"Infinity", "Infinity"}, // Number.isInteger(Infinity) is false
		{"1_0", "1_0"},
		{"NaN", "NaN"},
		{"", ""},
	}
	for _, c := range cases {
		if got := integer.Coerce(c.in); got != c.want {
			t.Errorf("integer Coerce(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// TestMatchesJSONTypeRejectsNonFinite: TypeBox's number guard is
// Number.isFinite (guard.mjs IsNumber), so NaN/±Inf must not validate as
// number or integer.
func TestMatchesJSONTypeRejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if matchesJSONType(v, "number") {
			t.Errorf("matchesJSONType(%v, number) = true, want false", v)
		}
		if matchesJSONType(v, "integer") {
			t.Errorf("matchesJSONType(%v, integer) = true, want false", v)
		}
	}
	if !matchesJSONType(float64(5), "number") || !matchesJSONType(float64(5), "integer") {
		t.Error("finite integral float must match number and integer")
	}
	if matchesJSONType(0.5, "integer") {
		t.Error("0.5 must not match integer")
	}
}

// TestKeywordEnforcement locks G2: TypeBox-checked keywords must be enforced
// with TypeBox en_US message wording.
func TestKeywordEnforcement(t *testing.T) {
	cases := []struct {
		name   string
		schema string // JSON schema of property "v"
		ok     any
		bad    any
		msg    string // expected substring of the validation error
	}{
		{"pattern", `{"type":"string","pattern":"^a+$"}`, "aaa", "bbb", `v: must match pattern "^a+$"`},
		{"minLength", `{"type":"string","minLength":2}`, "ab", "a", "v: must not have fewer than 2 characters"},
		{"maxLength", `{"type":"string","maxLength":2}`, "ab", "abc", "v: must not have more than 2 characters"},
		{"minItems", `{"type":"array","items":{"type":"integer"},"minItems":2}`, []any{float64(1), float64(2)}, []any{float64(1)}, "v: must not have fewer than 2 items"},
		{"maxItems", `{"type":"array","items":{"type":"integer"},"maxItems":1}`, []any{float64(1)}, []any{float64(1), float64(2)}, "v: must not have more than 1 items"},
		{"minimum", `{"type":"number","minimum":5}`, float64(5), float64(4), "v: must be >= 5"},
		{"maximum", `{"type":"number","maximum":5}`, float64(5), float64(6), "v: must be <= 5"},
		{"exclusiveMinimum", `{"type":"number","exclusiveMinimum":5}`, float64(6), float64(5), "v: must be > 5"},
		{"exclusiveMaximum", `{"type":"number","exclusiveMaximum":5}`, float64(4), float64(5), "v: must be < 5"},
		{"multipleOf", `{"type":"number","multipleOf":3}`, float64(9), float64(7), "v: must be multiple of 3"},
		{"multipleOf-float", `{"type":"number","multipleOf":0.1}`, 0.3, 0.35, "v: must be multiple of 0.1"},
		{"const", `{"type":"string","const":"x"}`, "x", "y", "v: must be equal to constant"},
		{"const-null", `{"type":"null","const":null}`, nil, nil, ""},
	}
	for _, c := range cases {
		var prop Schema
		if err := json.Unmarshal([]byte(c.schema), &prop); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		tool := Tool{Name: "t", Parameters: Object(Prop("v", &prop))}

		if _, err := ValidateToolArguments(tool, ToolCall{Name: "t", Arguments: map[string]any{"v": c.ok}}); err != nil {
			t.Errorf("%s: valid value %#v rejected: %v", c.name, c.ok, err)
		}
		if c.msg == "" {
			continue
		}
		_, err := ValidateToolArguments(tool, ToolCall{Name: "t", Arguments: map[string]any{"v": c.bad}})
		if err == nil {
			t.Errorf("%s: invalid value %#v accepted", c.name, c.bad)
		} else if !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: error %q does not contain %q", c.name, err.Error(), c.msg)
		}
	}
}

// TestUnenforceablePatternSkipped: JS-valid patterns Go's RE2 cannot compile
// (lookaround) are skipped without error rather than failing every value.
func TestUnenforceablePatternSkipped(t *testing.T) {
	var prop Schema
	if err := json.Unmarshal([]byte(`{"type":"string","pattern":"^(?=.*a).*$"}`), &prop); err != nil {
		t.Fatal(err)
	}
	tool := Tool{Name: "t", Parameters: Object(Prop("v", &prop))}
	if _, err := ValidateToolArguments(tool, ToolCall{Name: "t", Arguments: map[string]any{"v": "anything"}}); err != nil {
		t.Fatalf("unenforceable pattern should be skipped, got: %v", err)
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
