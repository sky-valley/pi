package schema

import (
	"fmt"
	"testing"
)

// Upstream 20c637b1a ("leaf checks inlined by object and array loops",
// test/validator.test.ts). The commit is a speed change inside pi's compiler:
// object and array loops run const, small enums, type lists, number bounds,
// string lengths and patterns inline instead of calling a closure, a closed
// object looks keys up in declared order first, and anyOf of bare types runs
// as one type test. None of it changes a result, and the reason for it — the
// cost of a JavaScript closure call per property — has no Go counterpart, so
// the port takes the tests and not the restructuring. CHARACTERIZATION: these
// pass on the port as it stood; they pin pi's standalone answer for each
// value (pi's compile(schema).check at 20c637b1a, run under node) and that the
// leaf gives the same answer as a property of a closed and of an open object,
// and as an array item.
func TestLeafChecksAgreeInObjectsAndArrays(t *testing.T) {
	leaves := []struct {
		schema string
		values []any
		pi     []bool
	}{
		{`{"const":"a"}`, []any{"a", "b", 1.0, nil}, []bool{true, false, false, false}},
		{`{"const":null}`, []any{nil, 0.0, false, "null"}, []bool{true, false, false, false}},
		{`{"enum":["a",1,null]}`, []any{"a", 1.0, nil, "b", 2.0, true}, []bool{true, true, true, false, false, false}},
		{`{"type":["string","null"]}`, []any{"a", nil, 1.0, Object{}, []any{}}, []bool{true, true, false, false, false}},
		{`{"type":["integer","boolean"]}`, []any{1.0, 1.5, true, "1"}, []bool{true, false, true, false}},
		{`{"type":["number","array"]}`, []any{1.5, 2.0, []any{}, Object{}, make([]byte, 1)}, []bool{true, true, true, false, false}},
		{`{"type":["object","bytes"]}`, []any{Object{}, make([]byte, 1), []any{}, nil}, []bool{true, true, false, false}},
		{`{"anyOf":[{"type":"string"},{"type":"null"}]}`, []any{"a", nil, 1.0}, []bool{true, true, false}},
		{`{"anyOf":[{"type":"integer"},{"type":"string","minLength":2}]}`, []any{1.0, 1.5, "ab", "a"}, []bool{true, false, true, false}},
		{`{"type":"integer","minimum":1,"exclusiveMaximum":5}`, []any{1.0, 4.0, 5.0, 0.0, 2.5, "2"}, []bool{true, true, false, false, false, false}},
		{`{"type":"number","exclusiveMinimum":0,"maximum":1}`, []any{0.5, 1.0, 0.0, 1.5, "0.5"}, []bool{true, true, false, false, false}},
		{`{"type":"integer","multipleOf":2}`, []any{2.0, 3.0, 2.5}, []bool{true, false, false}},
		{`{"type":"string","minLength":2,"maxLength":3}`, []any{"ab", "abc", "a", "abcd", "😀😀", 12.0}, []bool{true, true, false, false, true, false}},
		{`{"type":"string","pattern":"^a+$"}`, []any{"aa", "ab", 1.0}, []bool{true, false, false}},
		{`{"minimum":1}`, []any{1.0, 0.0, "x", nil}, []bool{true, false, true, true}},
	}
	for _, leaf := range leaves {
		schema := js(t, leaf.schema)
		alone := mustCompile(t, schema, Options{})
		closed := mustCompile(t, Object{{"type", "object"}, {"properties", Object{{"p", schema}}}, {"additionalProperties", false}}, Options{})
		open := mustCompile(t, Object{{"type", "object"}, {"properties", Object{{"p", schema}}}, {"required", []any{"q"}}}, Options{})
		items := mustCompile(t, Object{{"type", "array"}, {"items", schema}}, Options{})
		for i, v := range leaf.values {
			want := alone.Check(v)
			if want != leaf.pi[i] {
				t.Errorf("%s: Check(%#v) = %v, pi %v", leaf.schema, v, want, leaf.pi[i])
			}
			got := [3]bool{closed.Check(Object{{"p", v}}), open.Check(Object{{"p", v}, {"q", 1.0}}), items.Check([]any{v})}
			if got != [3]bool{want, want, want} {
				t.Errorf("%s with %s: closed, open, item = %v, standalone %v", leaf.schema, fmt.Sprintf("%#v", v), got, want)
			}
		}
	}
}

// pi's "find properties in any order" (validator.test.ts at 55a5c6914).
func TestClosedObjectFindsPropertiesInAnyOrder(t *testing.T) {
	v := mustCompile(t, js(t, `{"type":"object","properties":{"a":{"const":1},"b":{"type":"string"},"c":{"type":"integer","minimum":0}},"required":["a","c"],"additionalProperties":false}`), Options{})
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{`{"a":1,"b":"x","c":0}`, true},
		{`{"c":0,"b":"x","a":1}`, true},
		{`{"b":"x","c":0,"a":1}`, true},
		{`{"a":1,"c":0,"a2":1}`, false},
		{`{"c":-1,"a":1}`, false},
		{`{"c":0,"b":1,"a":1}`, false},
		{`{"c":0,"b":"x"}`, false},
	} {
		if got := v.Check(js(t, tc.value)); got != tc.want {
			t.Errorf("Check(%s) = %v, want %v", tc.value, got, tc.want)
		}
	}
	// {c: 0, b: undefined, a: 1}: an optional property set to undefined.
	if !v.Check(Object{{"c", 0.0}, {"b", Undefined}, {"a", 1.0}}) {
		t.Error("Check({c: 0, b: undefined, a: 1}) = false, want true")
	}
}
