package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/internal/jstext"
)

// Ported from upstream's test/validator.test.ts at 6536f8012, and its cases
// added by af79b0c3d and 55a5c6914. Not ported: the deep-frozen half of "compiles deep-frozen schemas" (Go values cannot be
// frozen; the never-modified half is kept), and "TypeBox's extra formats"
// with "treats format as an annotation" against the real formats (the
// format checks are schema/formats'; the annotation rule is tested here with
// a stand-in check).

// js decodes JSON text into the value model, keeping its key order.
func js(t *testing.T, text string) any {
	t.Helper()
	v, err := Decode([]byte(text))
	if err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return v
}

func mustCompile(t *testing.T, schema any, opts Options) *Validator {
	t.Helper()
	v, err := Compile(schema, opts)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return v
}

// mustErrors is v.Errors(value), failing the test on an error.
func mustErrors(t *testing.T, v *Validator, value any) []ValidationError {
	t.Helper()
	errs, err := v.Errors(value)
	if err != nil {
		t.Fatalf("errors: %v", err)
	}
	return errs
}

// mustCoerce is v.Coerce(value), failing the test on an error.
func mustCoerce(t *testing.T, v *Validator, value any) any {
	t.Helper()
	coerced, err := v.Coerce(value)
	if err != nil {
		t.Fatalf("coerce: %v", err)
	}
	return coerced
}

// mustParse is v.Parse(value), failing the test on an error.
func mustParse(t *testing.T, v *Validator, value any) (any, []ValidationError) {
	t.Helper()
	coerced, errs, err := v.Parse(value)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return coerced, errs
}

func wantErrors(t *testing.T, got []ValidationError, want ...ValidationError) {
	t.Helper()
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("errors:\n got %v\nwant %v", got, want)
	}
}

const readSchema = `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`

const blockSchema = `{"anyOf":[
	{"type":"object","properties":{"kind":{"const":"text"},"text":{"type":"string"}},"required":["kind","text"],"additionalProperties":false},
	{"type":"object","properties":{"kind":{"const":"image"},"width":{"type":"integer","minimum":0}},"required":["kind","width"],"additionalProperties":false}]}`

func TestErrors(t *testing.T) {
	t.Run("reports a missing field and an unknown field at the field", func(t *testing.T) {
		wantErrors(t, mustErrors(t, mustCompile(t, js(t, readSchema), Options{}), js(t, `{"offset":0,"extra":true}`)),
			ValidationError{"/path", "required", "missing"},
			ValidationError{"/offset", "minimum", "expected >= 1"},
			ValidationError{"/extra", "additionalProperties", "unknown field"})
	})
	t.Run("is empty for a valid value", func(t *testing.T) {
		if errs := mustErrors(t, mustCompile(t, js(t, readSchema), Options{}), js(t, `{"path":"a.ts"}`)); errs != nil {
			t.Errorf("errors %v", errs)
		}
	})
	t.Run("reports the branch a tagged union's tag selects", func(t *testing.T) {
		v := mustCompile(t, Object{{"type", "array"}, {"items", js(t, blockSchema)}}, Options{})
		wantErrors(t, mustErrors(t, v, js(t, `[{"kind":"text","text":"a"},{"kind":"image","width":-1}]`)),
			ValidationError{"/1/width", "minimum", "expected >= 0"})
		wantErrors(t, mustErrors(t, v, js(t, `[{"kind":"video"}]`)),
			ValidationError{"/0/kind", "anyOf", `expected one of ["text","image"]`})
	})
	t.Run("escapes ~ and / in paths as JSON Pointer does", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"a/b~c":{"type":"string"}}}`), Options{})
		wantErrors(t, mustErrors(t, v, js(t, `{"a/b~c":1}`)), ValidationError{"/a~1b~0c", "type", "expected string, got integer"})
	})
	t.Run("follows $ref into the referenced schema", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"$defs":{"point":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}},"type":"array","items":{"$ref":"#/$defs/point"}}`), Options{})
		wantErrors(t, mustErrors(t, v, js(t, `[{"x":1},{}]`)), ValidationError{"/1/x", "required", "missing"})
	})
}

func TestValues(t *testing.T) {
	t.Run("checks bytes as []byte with length bounds", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"data":{"type":"bytes","maxLength":4}}}`), Options{})
		for _, c := range []struct {
			value any
			want  bool
		}{
			{Object{{"data", make([]byte, 4)}}, true},
			{Object{{"data", make([]byte, 5)}}, false},
			{Object{{"data", []any{1.0, 2.0}}}, false},
		} {
			if got := v.Check(c.value); got != c.want {
				t.Errorf("check %v = %v", c.value, got)
			}
		}
		wantErrors(t, mustErrors(t, v, Object{{"data", make([]byte, 5)}}), ValidationError{"/data", "maxLength", "expected at most 4 bytes"})
	})
	t.Run("does not treat bytes as an object", func(t *testing.T) {
		if mustCompile(t, Object{{"type", "object"}}, Options{}).Check(make([]byte, 1)) {
			t.Error("bytes passed as an object")
		}
	})
	t.Run("checks properties named like Object.prototype members as own properties", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"constructor":{"type":"string"},"toString":{"type":"number"}},"required":["constructor"],"additionalProperties":false}`), Options{})
		if v.Check(Object{}) || !v.Check(Object{{"constructor", "a"}}) || v.Check(Object{{"constructor", "a"}, {"toString", "b"}}) {
			t.Error("prototype-named properties")
		}
	})
	t.Run("does not accept every value for a type named like an Object.prototype member", func(t *testing.T) {
		if mustCompile(t, Object{{"type", "constructor"}}, Options{}).Check(1.0) {
			t.Error("type constructor accepted 1")
		}
	})
	t.Run("checks a detached check function", func(t *testing.T) {
		check := mustCompile(t, js(t, readSchema), Options{}).Check
		values := []any{Object{{"path", "a"}}, Object{{"path", 1.0}}}
		got := slices.DeleteFunc(values, func(v any) bool { return !check(v) })
		if len(got) != 1 || stringify(got[0]) != `{"path":"a"}` {
			t.Errorf("filtered %v", got)
		}
	})
	t.Run("reads Go numbers and maps", func(t *testing.T) {
		v := mustCompile(t, map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer", "minimum": 2}}, "required": []string{"n"}}, Options{})
		for _, value := range []any{map[string]any{"n": 2}, map[string]any{"n": int64(3)}, Object{{"n", uint8(4)}}, map[string]any{"n": float32(5)}, map[string]any{"n": json.Number("6")}} {
			if !v.Check(value) {
				t.Errorf("check %v = false", value)
			}
		}
		wantErrors(t, mustErrors(t, v, map[string]any{"n": 1.5, "b": 1, "a": 2, "10": 3, "9": 4}),
			ValidationError{"/n", "type", "expected integer, got number"})
	})
}

func TestOptions(t *testing.T) {
	t.Run("treats format as an annotation unless formats are given", func(t *testing.T) {
		schema := js(t, `{"type":"string","format":"email"}`)
		formats := map[string]FormatCheck{"email": func(s string) bool { return strings.Contains(s, "@") }}
		if !mustCompile(t, schema, Options{}).Check("not an email") {
			t.Error("format asserted without formats")
		}
		if mustCompile(t, schema, Options{Formats: formats}).Check("not an email") || !mustCompile(t, schema, Options{Formats: formats}).Check("a@example.com") {
			t.Error("format not asserted with formats")
		}
	})
	t.Run("resolves $ref to registered documents", func(t *testing.T) {
		v := mustCompile(t, Object{{"$ref", "https://example.com/point.json"}}, Options{Schemas: map[string]any{
			"https://example.com/point.json": js(t, `{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}`),
		}})
		if !v.Check(Object{{"x", 1.0}}) || v.Check(Object{}) {
			t.Error("registered document")
		}
	})
	t.Run("throws for a $ref that names nothing", func(t *testing.T) {
		for _, ref := range []string{"https://example.com/missing.json", "#/$defs/missing"} {
			if _, err := Compile(Object{{"$ref", ref}}, Options{}); err == nil || !strings.HasPrefix(err.Error(), "Unresolved $ref") {
				t.Errorf("%s: %v", ref, err)
			}
		}
	})
	t.Run("applies draft 7 semantics: $ref overrides its siblings", func(t *testing.T) {
		schema := js(t, `{"definitions":{"s":{"type":"string"}},"$ref":"#/definitions/s","maxLength":1}`)
		if !mustCompile(t, schema, Options{Draft: Draft7}).Check("ab") || mustCompile(t, schema, Options{}).Check("ab") {
			t.Error("draft 7 $ref")
		}
	})
}

func TestSchemas(t *testing.T) {
	t.Run("compiles TypeBox schemas as they are", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","required":["name","tags"],"properties":{"name":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}}}`), Options{})
		if !v.Check(js(t, `{"name":"a","tags":[]}`)) {
			t.Error("valid value failed")
		}
		wantErrors(t, mustErrors(t, v, js(t, `{"name":"a","tags":[1]}`)), ValidationError{"/tags/0", "type", "expected string, got integer"})
	})
	t.Run("never modifies a schema", func(t *testing.T) {
		schema := js(t, `{"anyOf":[{"type":"object","properties":{"kind":{"const":"text"},"text":{"type":"string"}},"required":["kind","text"]},{"type":"object","properties":{"kind":{"const":"image"},"width":{"type":"integer","minimum":0}},"required":["kind","width"]}],"$defs":{"s":{"type":"string","pattern":"^a"}}}`)
		before := stringify(schema)
		v := mustCompile(t, schema, Options{Coerce: true})
		if !v.Check(js(t, `{"kind":"text","text":"x"}`)) || len(mustErrors(t, v, js(t, `{"kind":"image","width":-1}`))) != 1 {
			t.Error("checks")
		}
		mustCoerce(t, v, js(t, `{"kind":"image","width":"1"}`))
		if stringify(schema) != before {
			t.Error("the schema changed")
		}
	})
	t.Run("compiles a recursive schema", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"value":{"type":"number"},"children":{"type":"array","items":{"$ref":"#"}}},"required":["value"]}`), Options{})
		if !v.Check(js(t, `{"value":1,"children":[{"value":2,"children":[{"value":3}]}]}`)) {
			t.Error("valid tree failed")
		}
		wantErrors(t, mustErrors(t, v, js(t, `{"value":1,"children":[{"value":2,"children":[{}]}]}`)),
			ValidationError{"/children/0/children/0/value", "required", "missing"})
	})
	t.Run("throws for a malformed schema", func(t *testing.T) {
		_, err := Compile(js(t, `{"properties":{"a":1}}`), Options{})
		if err == nil || !strings.Contains(err.Error(), "must be an object or a boolean") {
			t.Errorf("error %v", err)
		}
	})
}

func TestUnresolvedReferences(t *testing.T) {
	dangling := `{"type":"object","properties":{"t":{"$ref":"#/definitions/Missing"}}}`
	external := `{"type":"object","properties":{"t":{"$ref":"https://example.com/schemas/t.json"}}}`
	t.Run("throw when the schema compiles, by default", func(t *testing.T) {
		for schema, want := range map[string]string{dangling: "Unresolved $ref #/definitions/Missing", external: "Unresolved $ref https://example.com/schemas/t.json"} {
			if _, err := Compile(js(t, schema), Options{}); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want %q", err, want)
			}
		}
	})
	t.Run("reject only values at their place with unresolvedRefs: reject", func(t *testing.T) {
		for _, schema := range []string{dangling, external} {
			v := mustCompile(t, js(t, schema), Options{UnresolvedRefs: UnresolvedReject})
			if !v.Check(Object{}) || v.Check(Object{{"t", 1.0}}) {
				t.Errorf("%s: reject", schema)
			}
		}
		wantErrors(t, mustErrors(t, mustCompile(t, js(t, dangling), Options{UnresolvedRefs: UnresolvedReject}), Object{{"t", 1.0}}),
			ValidationError{"/t", "$ref", "unresolved reference #/definitions/Missing"})
	})
	t.Run("leave values at their place uncoerced", func(t *testing.T) {
		v := mustCompile(t, js(t, dangling), Options{UnresolvedRefs: UnresolvedReject, Coerce: true})
		_, errs := mustParse(t, v, Object{{"t", "1"}})
		wantErrors(t, errs, ValidationError{"/t", "$ref", "unresolved reference #/definitions/Missing"})
	})
	t.Run("reject a TypeBox Ref without its definition", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","required":["a"],"properties":{"a":{"$ref":"A"}}}`), Options{UnresolvedRefs: UnresolvedReject})
		wantErrors(t, mustErrors(t, v, js(t, `{"a":{"x":"1"}}`)), ValidationError{"/a", "$ref", "unresolved reference A"})
	})
}

func TestStringLengths(t *testing.T) {
	high, low := jstext.CodeUnit(0xD800), jstext.CodeUnit(0xDC00)
	for _, c := range []struct {
		schema string
		value  string
		want   bool
	}{
		{`{"type":"string","minLength":2}`, high + "A", true},
		{`{"type":"string","maxLength":1}`, high + "A", false},
		{`{"type":"string","maxLength":1}`, high, true},
		{`{"type":"string","maxLength":1}`, "😀", true},
		{`{"type":"string","minLength":2}`, "😀", false},
		{`{"type":"string","minLength":2}`, low + high, true},
	} {
		if got := mustCompile(t, js(t, c.schema), Options{}).Check(c.value); got != c.want {
			t.Errorf("%s check %q = %v", c.schema, c.value, got)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	t.Run("list at most 20 enum values", func(t *testing.T) {
		values := make([]any, 5000)
		for i := range values {
			values[i] = fmt.Sprintf("v%d", i)
		}
		v := mustCompile(t, Object{{"type", "object"}, {"properties", Object{{"e", Object{{"type", "string"}, {"enum", values}}}}}}, Options{})
		if !v.Check(Object{{"e", "v4999"}}) {
			t.Error("member failed")
		}
		listed := stringify(values[:20])
		want := "expected one of " + listed[:len(listed)-1] + ", ... and 4980 more]"
		if errs := mustErrors(t, v, Object{{"e", "v5000"}}); len(errs) == 0 || errs[0].Message != want {
			t.Errorf("errors %v", errs)
		}
		wantErrors(t, mustErrors(t, mustCompile(t, js(t, `{"enum":["a","b"]}`), Options{}), "c"), ValidationError{"", "enum", `expected one of ["a","b"]`})
	})
	t.Run("accept a 10,000-value enum", func(t *testing.T) {
		values := make([]any, 10000)
		for i := range values {
			values[i] = fmt.Sprintf("member%d", i)
		}
		if !mustCompile(t, Object{{"enum", values}}, Options{}).Check("member9999") {
			t.Error("member failed")
		}
	})
	t.Run("describe contains bounds in words", func(t *testing.T) {
		for extra, want := range map[string]string{
			`"contains":{"type":"number"}`:                                 "expected at least 1 matching item",
			`"contains":{"type":"number"},"minContains":2`:                 "expected at least 2 matching items",
			`"contains":{"type":"string"},"maxContains":0,"minContains":0`: "expected at most 0 matching items",
			`"contains":{"type":"number"},"minContains":1,"maxContains":3`: "expected between 1 and 3 matching items",
		} {
			errs := mustErrors(t, mustCompile(t, js(t, `{"type":"array",`+extra+`}`), Options{}), []any{"a"})
			if len(errs) == 0 || errs[0].Message != want {
				t.Errorf("%s: %v, want %q", extra, errs, want)
			}
		}
	})
	t.Run("blame a oneOf failure on the branch that declares most of the object's properties", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"where":{"$ref":"#/$defs/F"}},"$defs":{"F":{"oneOf":[
			{"type":"object","properties":{"field":{"type":"string"},"eq":{"type":"string"}},"required":["field","eq"],"additionalProperties":false},
			{"type":"object","properties":{"and":{"type":"array","items":{"$ref":"#/$defs/F"}}},"required":["and"],"additionalProperties":false}]}}}`), Options{})
		wantErrors(t, mustErrors(t, v, js(t, `{"where":{"and":[{"field":"a","eq":1,"x":2}]}}`)),
			ValidationError{"/where/and/0/eq", "type", "expected string, got integer"},
			ValidationError{"/where/and/0/x", "additionalProperties", "unknown field"})
	})
	t.Run("explain one object reached through the same reference at two paths at each path", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"a":{"$ref":"#/$defs/P"},"b":{"$ref":"#/$defs/P"}},"$defs":{"P":{"type":"object","properties":{"n":{"type":"integer"}}}}}`), Options{})
		shared := Object{{"n", "x"}}
		wantErrors(t, mustErrors(t, v, Object{{"a", shared}, {"b", shared}}),
			ValidationError{"/a/n", "type", "expected integer, got string"},
			ValidationError{"/b/n", "type", "expected integer, got string"})
	})
}

// TestErrorsReturnsALazilyCompiledSchemasThrow: where upstream's errors()
// throws on a malformed subschema the check never compiled, Errors and Parse
// return that error, and keep returning it: the failed compile is not
// cached as a half-built check (upstream's second call throws "slot.check is
// not a function" instead).
func TestErrorsReturnsALazilyCompiledSchemasThrow(t *testing.T) {
	_, syntaxError := sharedRegex("(")
	for _, tc := range []struct{ schema, want string }{
		{`{"if":5,"minimum":3}`, "A schema must be an object or a boolean, got 5"},
		{`{"if":{"pattern":"("},"minimum":3}`, syntaxError.Error()},
		{`{"type":"object","properties":{"a":{"type":"string","minLength":5,"if":{"pattern":"("}}}}`, syntaxError.Error()},
	} {
		v := mustCompile(t, js(t, tc.schema), Options{Coerce: true})
		value := js(t, `{"a":"x"}`)
		if !strings.HasPrefix(tc.schema, `{"type"`) {
			value = 1.0
		}
		for i := range 3 {
			if errs, err := v.Errors(value); err == nil || err.Error() != tc.want || errs != nil {
				t.Errorf("%s: Errors call %d = %v, %v; want the error %q", tc.schema, i, errs, err, tc.want)
			}
			if coerced, errs, err := v.Parse(value); err == nil || err.Error() != tc.want || coerced != nil || errs != nil {
				t.Errorf("%s: Parse call %d = %v, %v, %v; want the error %q", tc.schema, i, coerced, errs, err, tc.want)
			}
		}
	}
}

// TestCycleOnOneValue: a schema that applies itself to the same value
// compiles; a value that reaches the cycle is invalid to Check, and Errors
// and Parse return ErrStackOverflow, where upstream's check, errors and
// parse throw RangeError "Maximum call stack size exceeded" (the golden
// rows "ref to root" and on record every case). Only such a schema's Check
// is serialized.
func TestCycleOnOneValue(t *testing.T) {
	for _, tc := range []struct {
		schema string
		serial bool
	}{
		{`{"$ref":"#"}`, true},
		{`{"type":"object","allOf":[{"$ref":"#"}]}`, true},
		{`{"$defs":{"A":{"$ref":"#/$defs/A"}},"$ref":"#/$defs/A"}`, true},
		{`{"anyOf":[{"type":"object"},{"$ref":"#/$defs/Y"}],"properties":{"p":{"$ref":"#/$defs/Y"}},"$defs":{"Y":{"$ref":"#"}}}`, true},
		{`{"type":"object","properties":{"x":{"$ref":"#"}}}`, false},
		{`{"$defs":{"T":{"anyOf":[{"type":"string"},{"type":"array","items":{"$ref":"#/$defs/T"}}]}},"$ref":"#/$defs/T"}`, false},
	} {
		v := mustCompile(t, js(t, tc.schema), Options{Coerce: true})
		if v.serial != tc.serial {
			t.Errorf("%s: serial %v, want %v", tc.schema, v.serial, tc.serial)
		}
		if !tc.serial {
			continue
		}
		value := js(t, `{"p":5}`)
		for range 2 {
			if v.Check(value) {
				t.Errorf("%s: Check passes", tc.schema)
			}
			if _, err := v.Errors(value); !errors.Is(err, ErrStackOverflow) {
				t.Errorf("%s: Errors error %v", tc.schema, err)
			}
			if _, _, err := v.Parse(value); !errors.Is(err, ErrStackOverflow) {
				t.Errorf("%s: Parse error %v", tc.schema, err)
			}
			if coerced, err := v.Coerce(value); err != nil || stringify(coerced) != `{"p":5}` {
				t.Errorf("%s: Coerce = %v, %v; want the value", tc.schema, coerced, err)
			}
		}
		if len(v.c.active) != 0 || len(*v.c.scope) != 0 {
			t.Errorf("%s: a throw left the guards %v, the scope %v", tc.schema, v.c.active, *v.c.scope)
		}
	}
}

// TestDeepValueUnderRecursiveSchema: recursion that steps into children is
// bounded by the value, and Go's stack grows past V8's, so a value nested
// far deeper than upstream can check validates (doc.go).
func TestDeepValueUnderRecursiveSchema(t *testing.T) {
	v := mustCompile(t, js(t, `{"type":"object","properties":{"x":{"$ref":"#"}}}`), Options{})
	value := any(Object{})
	for range 2000 {
		value = Object{{"x", value}}
	}
	if !v.Check(value) {
		t.Error("a deep valid value fails")
	}
	value = Object{{"x", 1.0}}
	for range 2000 {
		value = Object{{"x", value}}
	}
	if errs := mustErrors(t, v, value); len(errs) != 1 || len(errs[0].Path) != 2*2001 {
		t.Errorf("errors %v", errs)
	}
}

// TestConcurrentUse runs every method from many goroutines (under -race),
// including a schema with a dynamic scope, and compares each result with a
// serial run's.
func TestConcurrentUse(t *testing.T) {
	dynamic := js(t, `{"$id":"https://example.com/tree","$dynamicAnchor":"node","type":"object","properties":{"data":true,"children":{"type":"array","items":{"$dynamicRef":"#node"}}},"$defs":{"strict":{"$id":"https://example.com/strict-tree","$dynamicAnchor":"node","$ref":"tree","unevaluatedProperties":false}}}`)
	const n = 16
	values := make([]any, n)
	for i := range values {
		values[i] = js(t, fmt.Sprintf(`{"path":%d,"offset":"%d","children":[{"data":1}]}`, i, i))
	}
	// result is one value's results, as JSON text.
	result := func(v *Validator, value any) string {
		errs, err := v.Errors(value)
		coerced, parseErrs, parseErr := v.Parse(value)
		return fmt.Sprint(v.Check(value), stringify(errorList(errs)), err, stringify(coerced), stringify(errorList(parseErrs)), parseErr)
	}
	cyclic := js(t, `{"anyOf":[{"type":"string"},{"$ref":"#"}]}`)
	for _, schema := range []any{js(t, readSchema), dynamic, cyclic} {
		serial := mustCompile(t, schema, Options{Coerce: true})
		want := make([]string, n)
		for i, value := range values {
			want[i] = result(serial, value)
		}
		v := mustCompile(t, schema, Options{Coerce: true})
		got := make([]string, n)
		var wg sync.WaitGroup
		for i, value := range values {
			wg.Go(func() { got[i] = result(v, value) })
		}
		wg.Wait()
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("value %d: concurrent %s, serial %s", i, got[i], want[i])
			}
		}
	}
}

func TestCoerceNeedsTheOption(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	_, _ = mustCompile(t, true, Options{}).Coerce(1.0)
}

// TypeBox's JavaScript types (validator.test.ts, since af79b0c3d): a bigint
// is a *big.Int, undefined is Undefined.
func TestTypeBoxJavaScriptTypes(t *testing.T) {
	n := big.NewInt
	checks := func(t *testing.T, v *Validator, want bool, values ...any) {
		t.Helper()
		for _, value := range values {
			if got := v.Check(value); got != want {
				t.Errorf("check %s = %v, want %v", stringify(tag(value)), got, want)
			}
		}
	}
	t.Run("check Type.BigInt with its bounds, as TypeBox does", func(t *testing.T) {
		v := mustCompile(t, Object{{"anyOf", []any{Object{{"type", "bigint"}, {"minimum", n(0)}}, Object{{"type", "null"}}}}}, Options{})
		checks(t, v, true, n(5), n(0), nil)
		checks(t, v, false, n(-1), 5.0, "5", Undefined)
		checks(t, mustCompile(t, Object{{"type", "bigint"}, {"maximum", n(10)}, {"multipleOf", n(5)}}, Options{}), true, n(10))
		checks(t, mustCompile(t, Object{{"type", "bigint"}, {"multipleOf", n(5)}}, Options{}), false, n(7))
		checks(t, mustCompile(t, Object{{"type", "bigint"}, {"exclusiveMaximum", n(3)}}, Options{}), false, n(3))
		wantErrors(t, mustErrors(t, mustCompile(t, Object{{"type", "bigint"}, {"minimum", n(0)}}, Options{}), n(-2)),
			ValidationError{"", "minimum", "expected >= 0"})
		wantErrors(t, mustErrors(t, mustCompile(t, Object{{"type", "bigint"}}, Options{}), 2.0),
			ValidationError{"", "type", "expected bigint, got integer"})
	})
	t.Run("check Type.Undefined: only undefined, and a required property with it must be present", func(t *testing.T) {
		checks(t, mustCompile(t, Object{{"type", "undefined"}}, Options{}), true, Undefined)
		checks(t, mustCompile(t, Object{{"type", "undefined"}}, Options{}), false, nil)
		v := mustCompile(t, js(t, `{"type":"object","properties":{"selection":{"const":"average"},"principalId":{"type":"undefined"}},"required":["selection","principalId"]}`), Options{})
		checks(t, v, true, Object{{"selection", "average"}, {"principalId", Undefined}})
		checks(t, v, false, Object{{"selection", "average"}}, Object{{"selection", "average"}, {"principalId", "x"}}, Object{{"selection", "average"}, {"principalId", nil}})
		wantErrors(t, mustErrors(t, v, Object{{"selection", "average"}}), ValidationError{"/principalId", "required", "missing"})
		wantErrors(t, mustErrors(t, v, Object{{"selection", "average"}, {"principalId", "x"}}),
			ValidationError{"/principalId", "type", "expected undefined, got string"})
	})
	t.Run("check a required property set to undefined by its schema, as TypeBox does", func(t *testing.T) {
		// Present and undefined: valid when the schema takes undefined
		// (Undefined in a union, Unknown, Any: the last two are {}).
		for _, property := range []any{js(t, `{"anyOf":[{"type":"string"},{"type":"undefined"}]}`), Object{}} {
			for _, closed := range []bool{false, true} {
				schema := Object{{"type", "object"}, {"properties", Object{{"a", property}}}, {"required", []any{"a"}}}
				if closed {
					schema = append(schema, Member{"additionalProperties", false})
				}
				v := mustCompile(t, schema, Options{})
				checks(t, v, true, Object{{"a", Undefined}}, map[string]any{"a": Undefined})
				checks(t, v, false, Object{})
			}
		}
		// A required property whose schema does not take undefined stays
		// missing when set to undefined.
		name := mustCompile(t, js(t, `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`), Options{})
		checks(t, name, false, Object{{"name", Undefined}})
		wantErrors(t, mustErrors(t, name, Object{{"name", Undefined}}), ValidationError{"/name", "required", "missing"})
		// An optional property set to undefined still counts as absent.
		checks(t, mustCompile(t, js(t, `{"type":"object","properties":{"name":{"type":"string"}}}`), Options{}), true, Object{{"name", Undefined}})
	})
	t.Run("treats a property set to undefined as absent", func(t *testing.T) {
		v := mustCompile(t, js(t, readSchema), Options{})
		checks(t, v, true, Object{{"path", "a.ts"}, {"offset", Undefined}})
		checks(t, v, false, Object{{"path", Undefined}})
	})
	t.Run("check multipleOf of bigints exactly, however large", func(t *testing.T) {
		five := mustCompile(t, Object{{"type", "bigint"}, {"multipleOf", n(5)}}, Options{})
		huge := new(big.Int).Exp(n(10), n(400), nil)
		checks(t, five, true, huge)
		checks(t, five, false, new(big.Int).Add(huge, n(1)))
		// A bigint is no multiple of 0 or of a fraction.
		checks(t, mustCompile(t, Object{{"multipleOf", n(0)}}, Options{}), false, n(4))
		checks(t, mustCompile(t, Object{{"multipleOf", 0.5}}, Options{}), false, n(3))
	})
}

// validator.test.ts (since 55a5c6914): a number is finite, as in TypeBox.
func TestRejectsNaNAndInfinityAsANumberOrAnInteger(t *testing.T) {
	schemas := []string{
		`{"type":"number"}`,
		`{"type":"integer"}`,
		`{"type":"number","minimum":0}`,
		`{"type":["number","string"]}`,
		`{"anyOf":[{"type":"number"},{"type":"null"}]}`,
		`{"type":"object","properties":{"a":{"type":"number"}}}`,
	}
	for _, text := range schemas {
		v := mustCompile(t, js(t, text), Options{})
		for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			value := any(f)
			if strings.Contains(text, `"object"`) {
				value = Object{{"a", f}}
			}
			if v.Check(value) {
				t.Errorf("%s: check %v = true", text, f)
			}
		}
	}
	wantErrors(t, mustErrors(t, mustCompile(t, Object{{"type", "number"}}, Options{}), math.NaN()),
		ValidationError{"", "type", "expected number, got NaN"})
}
