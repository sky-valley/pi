package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/internal/jstext"
)

// Ported from upstream's test/validator.test.ts at 6536f8012. Not ported:
// "treats a property set to undefined as absent" (Go has no undefined), the
// deep-frozen half of "compiles deep-frozen schemas" (Go values cannot be
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
		wantErrors(t, mustCompile(t, js(t, readSchema), Options{}).Errors(js(t, `{"offset":0,"extra":true}`)),
			ValidationError{"/path", "required", "missing"},
			ValidationError{"/offset", "minimum", "expected >= 1"},
			ValidationError{"/extra", "additionalProperties", "unknown field"})
	})
	t.Run("is empty for a valid value", func(t *testing.T) {
		if errs := mustCompile(t, js(t, readSchema), Options{}).Errors(js(t, `{"path":"a.ts"}`)); errs != nil {
			t.Errorf("errors %v", errs)
		}
	})
	t.Run("reports the branch a tagged union's tag selects", func(t *testing.T) {
		v := mustCompile(t, Object{{"type", "array"}, {"items", js(t, blockSchema)}}, Options{})
		wantErrors(t, v.Errors(js(t, `[{"kind":"text","text":"a"},{"kind":"image","width":-1}]`)),
			ValidationError{"/1/width", "minimum", "expected >= 0"})
		wantErrors(t, v.Errors(js(t, `[{"kind":"video"}]`)),
			ValidationError{"/0/kind", "anyOf", `expected one of ["text","image"]`})
	})
	t.Run("escapes ~ and / in paths as JSON Pointer does", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"a/b~c":{"type":"string"}}}`), Options{})
		wantErrors(t, v.Errors(js(t, `{"a/b~c":1}`)), ValidationError{"/a~1b~0c", "type", "expected string, got integer"})
	})
	t.Run("follows $ref into the referenced schema", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"$defs":{"point":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}},"type":"array","items":{"$ref":"#/$defs/point"}}`), Options{})
		wantErrors(t, v.Errors(js(t, `[{"x":1},{}]`)), ValidationError{"/1/x", "required", "missing"})
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
		wantErrors(t, v.Errors(Object{{"data", make([]byte, 5)}}), ValidationError{"/data", "maxLength", "expected at most 4 bytes"})
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
		wantErrors(t, v.Errors(map[string]any{"n": 1.5, "b": 1, "a": 2, "10": 3, "9": 4}),
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
		wantErrors(t, v.Errors(js(t, `{"name":"a","tags":[1]}`)), ValidationError{"/tags/0", "type", "expected string, got integer"})
	})
	t.Run("never modifies a schema", func(t *testing.T) {
		schema := js(t, `{"anyOf":[{"type":"object","properties":{"kind":{"const":"text"},"text":{"type":"string"}},"required":["kind","text"]},{"type":"object","properties":{"kind":{"const":"image"},"width":{"type":"integer","minimum":0}},"required":["kind","width"]}],"$defs":{"s":{"type":"string","pattern":"^a"}}}`)
		before := stringify(schema)
		v := mustCompile(t, schema, Options{Coerce: true})
		if !v.Check(js(t, `{"kind":"text","text":"x"}`)) || len(v.Errors(js(t, `{"kind":"image","width":-1}`))) != 1 {
			t.Error("checks")
		}
		v.Coerce(js(t, `{"kind":"image","width":"1"}`))
		if stringify(schema) != before {
			t.Error("the schema changed")
		}
	})
	t.Run("compiles a recursive schema", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"value":{"type":"number"},"children":{"type":"array","items":{"$ref":"#"}}},"required":["value"]}`), Options{})
		if !v.Check(js(t, `{"value":1,"children":[{"value":2,"children":[{"value":3}]}]}`)) {
			t.Error("valid tree failed")
		}
		wantErrors(t, v.Errors(js(t, `{"value":1,"children":[{"value":2,"children":[{}]}]}`)),
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
		wantErrors(t, mustCompile(t, js(t, dangling), Options{UnresolvedRefs: UnresolvedReject}).Errors(Object{{"t", 1.0}}),
			ValidationError{"/t", "$ref", "unresolved reference #/definitions/Missing"})
	})
	t.Run("leave values at their place uncoerced", func(t *testing.T) {
		v := mustCompile(t, js(t, dangling), Options{UnresolvedRefs: UnresolvedReject, Coerce: true})
		_, errs := v.Parse(Object{{"t", "1"}})
		wantErrors(t, errs, ValidationError{"/t", "$ref", "unresolved reference #/definitions/Missing"})
	})
	t.Run("reject a TypeBox Ref without its definition", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","required":["a"],"properties":{"a":{"$ref":"A"}}}`), Options{UnresolvedRefs: UnresolvedReject})
		wantErrors(t, v.Errors(js(t, `{"a":{"x":"1"}}`)), ValidationError{"/a", "$ref", "unresolved reference A"})
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
		if errs := v.Errors(Object{{"e", "v5000"}}); len(errs) == 0 || errs[0].Message != want {
			t.Errorf("errors %v", errs)
		}
		wantErrors(t, mustCompile(t, js(t, `{"enum":["a","b"]}`), Options{}).Errors("c"), ValidationError{"", "enum", `expected one of ["a","b"]`})
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
			errs := mustCompile(t, js(t, `{"type":"array",`+extra+`}`), Options{}).Errors([]any{"a"})
			if len(errs) == 0 || errs[0].Message != want {
				t.Errorf("%s: %v, want %q", extra, errs, want)
			}
		}
	})
	t.Run("blame a oneOf failure on the branch that declares most of the object's properties", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"where":{"$ref":"#/$defs/F"}},"$defs":{"F":{"oneOf":[
			{"type":"object","properties":{"field":{"type":"string"},"eq":{"type":"string"}},"required":["field","eq"],"additionalProperties":false},
			{"type":"object","properties":{"and":{"type":"array","items":{"$ref":"#/$defs/F"}}},"required":["and"],"additionalProperties":false}]}}}`), Options{})
		wantErrors(t, v.Errors(js(t, `{"where":{"and":[{"field":"a","eq":1,"x":2}]}}`)),
			ValidationError{"/where/and/0/eq", "type", "expected string, got integer"},
			ValidationError{"/where/and/0/x", "additionalProperties", "unknown field"})
	})
	t.Run("explain one object reached through the same reference at two paths at each path", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"a":{"$ref":"#/$defs/P"},"b":{"$ref":"#/$defs/P"}},"$defs":{"P":{"type":"object","properties":{"n":{"type":"integer"}}}}}`), Options{})
		shared := Object{{"n", "x"}}
		wantErrors(t, v.Errors(Object{{"a", shared}, {"b", shared}}),
			ValidationError{"/a/n", "type", "expected integer, got string"},
			ValidationError{"/b/n", "type", "expected integer, got string"})
	})
}

// TestErrorsPanicsOnALazilyCompiledMalformedSchema pins the one panic Errors
// documents: upstream's errors() throws when it meets a malformed subschema
// the check never compiled.
func TestErrorsPanicsOnALazilyCompiledMalformedSchema(t *testing.T) {
	v := mustCompile(t, js(t, `{"if":5,"minimum":3}`), Options{})
	defer func() {
		err, ok := recover().(error)
		if !ok || err.Error() != "A schema must be an object or a boolean, got 5" {
			t.Errorf("panic %v", err)
		}
	}()
	v.Errors(1.0)
	t.Error("no panic")
}

// TestConcurrentUse runs every method from many goroutines (under -race),
// including a schema with a dynamic scope.
func TestConcurrentUse(t *testing.T) {
	dynamic := js(t, `{"$id":"https://example.com/tree","$dynamicAnchor":"node","type":"object","properties":{"data":true,"children":{"type":"array","items":{"$dynamicRef":"#node"}}},"$defs":{"strict":{"$id":"https://example.com/strict-tree","$dynamicAnchor":"node","$ref":"tree","unevaluatedProperties":false}}}`)
	for _, schema := range []any{js(t, readSchema), dynamic} {
		v := mustCompile(t, schema, Options{Coerce: true})
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				value := js(t, fmt.Sprintf(`{"path":%d,"offset":"%d","children":[{"data":1}]}`, i, i))
				v.Check(value)
				v.Errors(value)
				v.Parse(value)
			}()
		}
		wg.Wait()
	}
}

func TestCoerceNeedsTheOption(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	mustCompile(t, true, Options{}).Coerce(1.0)
}
