package schema

import (
	"math/big"
	"strings"
	"testing"
	"time"
)

// Ported from upstream's test/coerce.test.ts at 6536f8012. Not ported: the
// type-level assertions (expectTypeOf), "works on frozen schemas" (Go values
// cannot be frozen) and "still coerces when the Function constructor is
// unavailable" (no eval to fall back from).

func coerceWith(t *testing.T, schema, value any) any {
	t.Helper()
	return mustCoerce(t, mustCompile(t, schema, Options{Coerce: true}), value)
}

// coerceValue is the coerced `value` property of {value} under an object
// schema with one required property `value`.
func coerceValue(t *testing.T, schema, value any) any {
	t.Helper()
	root := Object{{"type", "object"}, {"properties", Object{{"value", schema}}}, {"required", []any{"value"}}}
	return propOf(coerceWith(t, root, Object{{"value", value}}), "value")
}

func wantJSON(t *testing.T, got any, want string) {
	t.Helper()
	if g := stringify(got); g != want {
		t.Errorf("got %s, want %s", g, want)
	}
}

func parseJSON(t *testing.T, v *Validator, value any) string {
	t.Helper()
	coerced, errs := mustParse(t, v, value)
	if errs == nil {
		return `{"ok":true,"value":` + stringify(coerced) + `}`
	}
	return `{"ok":false,"errors":` + stringify(errorList(errs)) + `}`
}

func TestCoerce(t *testing.T) {
	t.Run("never modifies the value", func(t *testing.T) {
		value := js(t, `{"count":"42","tags":"a"}`)
		wantJSON(t, coerceWith(t, Obj(Object{{"count", Num()}, {"tags", Arr(Str())}}), value), `{"count":42,"tags":["a"]}`)
		wantJSON(t, value, `{"count":"42","tags":"a"}`)
	})
	t.Run("returns the value itself when nothing changes", func(t *testing.T) {
		value := js(t, `{"a":{"b":[1,2]},"c":"x"}`)
		schema := Obj(Object{{"a", Obj(Object{{"b", Arr(Num())}})}, {"c", Str()}})
		if !strictEquals(coerceWith(t, schema, value), value) {
			t.Error("a copy came back")
		}
	})
	t.Run("copies only the changed paths", func(t *testing.T) {
		unchanged := js(t, `{"b":[1,2]}`)
		value := Object{{"a", unchanged}, {"c", js(t, `{"d":"1"}`)}}
		schema := Obj(Object{{"a", Obj(Object{{"b", Arr(Num())}})}, {"c", Obj(Object{{"d", Num()}})}})
		coerced := coerceWith(t, schema, value)
		wantJSON(t, coerced, `{"a":{"b":[1,2]},"c":{"d":1}}`)
		if strictEquals(coerced, value) || !strictEquals(propOf(coerced, "a"), unchanged) {
			t.Error("identity")
		}
		wantJSON(t, propOf(value, "c"), `{"d":"1"}`)
	})
	t.Run("keeps a map a map and an Object's order", func(t *testing.T) {
		m := map[string]any{"n": "1", "s": 2}
		got := coerceWith(t, Obj(Object{{"s", Str()}, {"n", Num()}}), m)
		if out, ok := got.(map[string]any); !ok || out["n"] != 1.0 || out["s"] != "2" || m["n"] != "1" {
			t.Errorf("got %#v (input %#v)", got, m)
		}
		wantJSON(t, coerceWith(t, Obj(Object{{"a", Num()}, {"z", Num()}}), Object{{"z", "1"}, {"a", "2"}}), `{"z":1,"a":2}`)
	})
	t.Run("leaves values it cannot coerce for the check to report", func(t *testing.T) {
		v := mustCompile(t, Obj(Object{{"count", Num()}}), Options{Coerce: true})
		coerced := mustCoerce(t, v, Object{{"count", "many"}})
		wantJSON(t, coerced, `{"count":"many"}`)
		if v.Check(coerced) {
			t.Error("check passed")
		}
	})
}

func TestParse(t *testing.T) {
	v := mustCompile(t, Obj(Object{{"count", Num()}, {"tags", Arr(Str())}}), Options{Coerce: true})
	if got := parseJSON(t, v, js(t, `{"count":"42","tags":"a"}`)); got != `{"ok":true,"value":{"count":42,"tags":["a"]}}` {
		t.Error(got)
	}
	v = mustCompile(t, Obj(Object{{"count", Num()}, {"name", Str()}}), Options{Coerce: true})
	if got := parseJSON(t, v, js(t, `{"count":"many","name":7}`)); got != `{"ok":false,"errors":[{"path":"/count","keyword":"type","message":"expected number, got string"}]}` {
		t.Error(got)
	}
}

// validate() (coerce.test.ts, since af79b0c3d) checks and explains without
// coercing; the port's Errors already is that: nil for a valid value (which
// is the caller's own), else its errors, with or without Options.Coerce.
func TestErrorsChecksWithoutCoercing(t *testing.T) {
	schema := Obj(Object{{"count", Num()}, {"tags", Arr(Str())}})
	for _, opts := range []Options{{}, {Coerce: true}} {
		v := mustCompile(t, schema, opts)
		wantErrors(t, mustErrors(t, v, js(t, `{"count":42,"tags":["a"]}`)))
		// Parse would turn "42" into 42 and "a" into ["a"]; Errors reports both.
		wantErrors(t, mustErrors(t, v, js(t, `{"count":"42","tags":"a"}`)),
			ValidationError{"/count", "type", "expected number, got string"},
			ValidationError{"/tags", "type", "expected array, got string"})
	}
}

// bigint and undefined, as TypeBox's Value.Convert (coerce.test.ts, since
// af79b0c3d).
func TestCoerceBigIntAndUndefined(t *testing.T) {
	bigint := Object{{"type", "bigint"}}
	t.Run("converts to a bigint like TypeBox's TryBigInt", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"n":{"type":"bigint","minimum":0}},"required":["n"]}`), Options{Coerce: true})
		coerced, errs := mustParse(t, v, js(t, `{"n":"42"}`))
		if n, ok := propOf(coerced, "n").(*big.Int); errs != nil || !ok || n.Cmp(big.NewInt(42)) != 0 {
			t.Errorf("parse = %v, %v", coerced, errs)
		}
		for _, c := range []struct {
			value any
			want  int64
		}{{"12n", 12}, {"12.9", 12}, {3.7, 3}, {true, 1}} {
			if n, ok := coerceValue(t, bigint, c.value).(*big.Int); !ok || n.Cmp(big.NewInt(c.want)) != 0 {
				t.Errorf("coerce %v = %v, want %dn", c.value, coerceValue(t, bigint, c.value), c.want)
			}
		}
		if got := coerceValue(t, bigint, "x"); got != "x" {
			t.Errorf("coerce x = %v", got)
		}
	})
	t.Run("converts to undefined like TypeBox's TryUndefined", func(t *testing.T) {
		for _, value := range []any{nil, 0.0, big.NewInt(0), false, "", "0", "null", "undefined"} {
			if got := coerceValue(t, Object{{"type", "undefined"}}, value); got != Undefined {
				t.Errorf("coerce %v = %v, want undefined", value, got)
			}
		}
		if got := coerceValue(t, Object{{"type", "undefined"}}, "x"); got != "x" {
			t.Errorf("coerce x = %v", got)
		}
	})
}

func TestOptionalNulls(t *testing.T) {
	t.Run("drops null in optional non-nullable properties, also nested", func(t *testing.T) {
		schema := Obj(Object{
			{"path", Str()},
			{"offset", Opt(Num())},
			{"nullable", Opt(Nullable(Str()))},
			{"metadata", Obj(Object{{"enabled", Opt(Object{{"type", "boolean"}})}})},
		}, Open())
		wantJSON(t, coerceWith(t, schema, js(t, `{"path":"file.txt","offset":null,"nullable":null,"metadata":{"enabled":null}}`)),
			`{"path":"file.txt","nullable":null,"metadata":{}}`)
	})
	t.Run("keeps a null whose property is a $ref", func(t *testing.T) {
		schema := js(t, `{"type":"object","properties":{"value":{"$ref":"#/$defs/value"}},"$defs":{"value":{"anyOf":[{"type":"number"},{"type":"null"}]}}}`)
		wantJSON(t, coerceWith(t, schema, js(t, `{"value":null}`)), `{"value":null}`)
	})
	t.Run("walks array items", func(t *testing.T) {
		schema := Object{{"type", "array"}, {"items", Obj(Object{{"a", Str()}, {"b", Opt(Int())}})}}
		wantJSON(t, coerceWith(t, schema, js(t, `[{"a":"x","b":null}]`)), `[{"a":"x"}]`)
	})
}

func TestConversionRules(t *testing.T) {
	cases := []struct {
		schema, value, want string
	}{
		// converts primitives by type
		{`{"type":"number"}`, `"42"`, `42`},
		{`{"type":"number"}`, `""`, `0`},
		{`{"type":"number"}`, `"true"`, `1`},
		{`{"type":"integer"}`, `"3.5"`, `3`},
		{`{"type":"integer"}`, `1.5`, `1`},
		{`{"type":"boolean"}`, `"1"`, `true`},
		{`{"type":"boolean"}`, `"FALSE"`, `false`},
		{`{"type":"null"}`, `"null"`, `null`},
		{`{"type":"string"}`, `42`, `"42"`},
		// wraps a single value into an array and converts the items
		{`{"type":"array","items":{"type":"number"}}`, `"1"`, `[1]`},
		{`{"type":"array","items":{"type":"string"}}`, `null`, `["null"]`},
		// converts tuple items in place and never wraps for a tuple
		{`{"type":"array","prefixItems":[{"type":"number"},{"type":"boolean"}],"items":false}`, `["1","true"]`, `[1,true]`},
		{`{"type":"array","prefixItems":[{"type":"number"},{"type":"boolean"}],"items":false}`, `"1"`, `"1"`},
		// converts const and enum values only when the result is a member
		{`{"const":1}`, `"1"`, `1`},
		{`{"const":1}`, `"2"`, `"2"`},
		{`{"enum":[1,2,"hello"]}`, `"2"`, `2`},
		{`{"enum":[1,2,"hello"]}`, `"world"`, `"world"`},
		// keeps a value one union branch accepts, else the first branch whose conversion the union accepts
		{`{"anyOf":[{"type":"number"},{"type":"null"}]}`, `null`, `null`},
		{`{"anyOf":[{"type":"number"},{"type":"null"}]}`, `"42"`, `42`},
		{`{"anyOf":[{"type":"number"},{"type":"null"}]}`, `"null"`, `null`},
		{`{"oneOf":[{"type":"boolean"},{"type":"number"}]}`, `"1"`, `true`},
		// pass 3: coerces like Ajv's coerceTypes
		{`{"type":"number"}`, `true`, `1`},
		{`{"type":"number"}`, `null`, `0`},
		{`{"type":"integer"}`, `"42"`, `42`},
		{`{"type":"boolean"}`, `"true"`, `true`},
		{`{"type":"boolean"}`, `0`, `false`},
		{`{"type":"string"}`, `true`, `"true"`},
		{`{"type":"null"}`, `""`, `null`},
		{`{"type":"null"}`, `0`, `null`},
		{`{"type":"null"}`, `false`, `null`},
		// a type array's matching member stays, else the first coercible type
		{`{"type":["number","string"]}`, `"1"`, `"1"`},
		{`{"type":["boolean","number"]}`, `"1"`, `1`},
		{`{"type":["integer","null"]}`, `"5"`, `5`},
		{`{"type":["array","null"],"items":{"type":"string"}}`, `null`, `null`},
		{`{"anyOf":[{"type":"number"},{"type":"null"}]}`, `"42"`, `42`},
		{`{"oneOf":[{"type":"number"},{"type":"null"}]}`, `null`, `null`},
		// where the passes conflict, the conversion rules win
		{`{"type":"string"}`, `null`, `"null"`},
		{`{"type":"number"}`, `""`, `0`},
		{`{"type":"boolean"}`, `"0"`, `false`},
		{`{"type":"integer"}`, `"42.1"`, `42`},
	}
	for _, c := range cases {
		if got := stringify(coerceValue(t, js(t, c.schema), js(t, c.value))); got != c.want {
			t.Errorf("%s coerces %s to %s, want %s", c.schema, c.value, got, c.want)
		}
	}
	t.Run("converts allOf with each member in turn", func(t *testing.T) {
		schema := Object{{"allOf", []any{Obj(Object{{"a", Num()}}, Open()), Obj(Object{{"b", Object{{"type", "boolean"}}}}, Open())}}}
		wantJSON(t, coerceWith(t, schema, js(t, `{"a":"1","b":"true"}`)), `{"a":1,"b":true}`)
	})
	t.Run("follows $ref", func(t *testing.T) {
		schema := js(t, `{"type":"object","properties":{"item":{"$ref":"#/$defs/item"}},"$defs":{"item":{"type":"object","properties":{"id":{"type":"integer"}}}}}`)
		wantJSON(t, coerceWith(t, schema, js(t, `{"item":{"id":"7"}}`)), `{"item":{"id":7}}`)
	})
	t.Run("converts patternProperties and additionalProperties", func(t *testing.T) {
		schema := js(t, `{"type":"object","properties":{"a":{"type":"number"}},"patternProperties":{"^n_":{"type":"number"}},"additionalProperties":{"type":"boolean"}}`)
		wantJSON(t, coerceWith(t, schema, js(t, `{"a":"1","n_x":"2","other":"true"}`)), `{"a":1,"n_x":2,"other":true}`)
	})
	t.Run("leaves values no rule coerces", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`), Options{Coerce: true})
		for _, input := range []string{`"abc"`, `"4x"`, `{}`, `["1","2"]`} {
			if v.Check(mustCoerce(t, v, Object{{"value", js(t, input)}})) {
				t.Errorf("%s coerced to a valid value", input)
			}
		}
	})
	t.Run("drops an optional null before converting it", func(t *testing.T) {
		wantJSON(t, coerceWith(t, Obj(Object{{"name", Opt(Str())}}), js(t, `{"name":null}`)), `{}`)
	})
}

func TestRecursiveCoercion(t *testing.T) {
	tree := js(t, `{"type":"object","properties":{"t":{"$ref":"#/$defs/Tree"}},"$defs":{"Tree":{"anyOf":[{"type":"string"},{"type":"array","items":{"$ref":"#/$defs/Tree"}}]}}}`)
	jsonSchema := js(t, `{"type":"object","properties":{"data":{"$ref":"#/$defs/Json"}},"required":["data"],"$defs":{"Json":{"anyOf":[
		{"type":"string"},{"type":"number"},{"type":"boolean"},{"type":"array","items":{"$ref":"#/$defs/Json"}},
		{"type":"object","additionalProperties":{"$ref":"#/$defs/Json"}}]}}}`)
	parses := func(t *testing.T, v *Validator, value, want string) {
		t.Helper()
		if got := parseJSON(t, v, js(t, value)); got != want {
			t.Errorf("parse %s = %s, want %s", value, got, want)
		}
	}
	fails := func(t *testing.T, v *Validator, value any) {
		t.Helper()
		if _, errs := mustParse(t, v, value); errs == nil {
			t.Errorf("parse %s passed", stringify(value))
		}
	}
	t.Run("convert through a recursive union by its first accepting branch", func(t *testing.T) {
		v := mustCompile(t, tree, Options{Coerce: true})
		parses(t, v, `{"t":["a",["b"]]}`, `{"ok":true,"value":{"t":["a",["b"]]}}`)
		parses(t, v, `{"t":5}`, `{"ok":true,"value":{"t":"5"}}`)
		parses(t, v, `{"t":[5]}`, `{"ok":true,"value":{"t":["5"]}}`)
		parses(t, mustCompile(t, jsonSchema, Options{Coerce: true}), `{"data":{"a":[1,null]}}`, `{"ok":true,"value":{"data":{"a":[1,"null"]}}}`)
	})
	t.Run("leave a value a recursive schema cannot convert as it is", func(t *testing.T) {
		v := mustCompile(t, tree, Options{Coerce: true})
		fails(t, v, js(t, `{"t":{}}`))
		wantJSON(t, mustCoerce(t, v, js(t, `{"t":{"a":1}}`)), `{"t":{"a":1}}`)
	})
	t.Run("convert a scalar-or-array schema that refers to itself", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"value":{"anyOf":[{"type":"number"},{"type":"array","items":{"$ref":"#/properties/value"}}]}},"required":["value"]}`), Options{Coerce: true})
		parses(t, v, `{"value":"42"}`, `{"ok":true,"value":{"value":42}}`)
		parses(t, v, `{"value":["1",["2"]]}`, `{"ok":true,"value":{"value":[1,[2]]}}`)
		fails(t, v, js(t, `{"value":{}}`))
	})
	t.Run("stop at a schema that cycles through shared maps instead of $ref", func(t *testing.T) {
		node := map[string]any{}
		node["anyOf"] = []any{map[string]any{"type": "number"}, map[string]any{"type": "array", "items": node}}
		v := mustCompile(t, node, Options{Coerce: true})
		parses(t, v, `"42"`, `{"ok":true,"value":42}`)
		parses(t, v, `["1",["2"]]`, `{"ok":true,"value":[1,[2]]}`)
		fails(t, v, Object{})
	})
	t.Run("reset the re-entry state between calls and leave the input unchanged", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"anyOf":[{"type":"number"},{"type":"array","items":{"$ref":"#"}}]}`), Options{Coerce: true})
		for range 20 {
			fails(t, v, Object{})
			parses(t, v, `"42"`, `{"ok":true,"value":42}`)
			input := js(t, `[1,"2",["3"]]`)
			if got := parseJSON(t, v, input); got != `{"ok":true,"value":[1,2,[3]]}` {
				t.Error(got)
			}
			wantJSON(t, input, `[1,"2",["3"]]`)
		}
	})
	t.Run("convert and explain a deep value that every branch of a recursive union rejects, in linear time", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"s":{"$ref":"#/$defs/S"}},"$defs":{"S":{"anyOf":[
			{"type":"object","additionalProperties":{"$ref":"#/$defs/S"},"required":["a"]},
			{"type":"object","additionalProperties":{"$ref":"#/$defs/S"},"required":["b"]}]}}}`), Options{Coerce: true})
		value := any(Object{{"x", 1.0}})
		for range 40 {
			value = Object{{"c", value}}
		}
		start := time.Now()
		_, errs := mustParse(t, v, Object{{"s", value}})
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("took %v", elapsed)
		}
		var want []ValidationError
		for i := range 41 {
			want = append(want, ValidationError{"/s" + strings.Repeat("/c", i) + "/a", "required", "missing"})
		}
		want = append(want, ValidationError{"/s" + strings.Repeat("/c", 40) + "/x", "type", "expected object, got integer"})
		wantErrors(t, errs, want...)
	})
	t.Run("convert a TypeBox Cyclic union", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","required":["t"],"properties":{"t":{"$defs":{"Tree":{"anyOf":[{"type":"string"},{"type":"array","items":{"$ref":"Tree"}}],"$id":"Tree"}},"$ref":"Tree"}}}`), Options{Coerce: true})
		parses(t, v, `{"t":[5]}`, `{"ok":true,"value":{"t":["5"]}}`)
		fails(t, v, js(t, `{"t":{"a":1}}`))
	})
}

func TestMemberKeywords(t *testing.T) {
	t.Run("keep a pattern-matched property from additionalProperties in every pass", func(t *testing.T) {
		record := js(t, `{"type":"object","patternProperties":{"^.*$":{"type":"number"}},"additionalProperties":{"type":"string"}}`)
		if got := parseJSON(t, mustCompile(t, record, Options{Coerce: true}), js(t, `{"n1":"1"}`)); got != `{"ok":true,"value":{"n1":1}}` {
			t.Error(got)
		}
		plain := js(t, `{"type":"object","patternProperties":{"^n_":{"type":["integer","null"]}},"additionalProperties":{"type":"string"}}`)
		wantJSON(t, coerceWith(t, plain, js(t, `{"n_a":"5","other":7}`)), `{"n_a":5,"other":"7"}`)
	})
	t.Run("drop optional nulls inside declared, pattern and additional property values alike", func(t *testing.T) {
		item := Obj(Object{{"n", Opt(Int(Minimum(1)))}})
		for _, schema := range []Object{
			Obj(Object{{"entry", item}}),
			Map(item),
			{{"type", "object"}, {"patternProperties", Object{{"^entry$", item}}}, {"additionalProperties", false}},
		} {
			input := js(t, `{"entry":{"n":null}}`)
			if got := parseJSON(t, mustCompile(t, schema, Options{Coerce: true}), input); got != `{"ok":true,"value":{"entry":{}}}` {
				t.Errorf("%s: %s", stringify(schema), got)
			}
			wantJSON(t, input, `{"entry":{"n":null}}`)
		}
	})
	t.Run("keep a null under pattern and additional properties", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","additionalProperties":{"type":["integer","null"]}}`), Options{Coerce: true})
		if got := parseJSON(t, v, js(t, `{"n":null}`)); got != `{"ok":true,"value":{"n":null}}` {
			t.Error(got)
		}
	})
	t.Run("drop optional nulls in tuple items as in array items", func(t *testing.T) {
		item := Obj(Object{{"n", Opt(Int())}})
		wantJSON(t, coerceWith(t, Arr(item), js(t, `[{"n":null}]`)), `[{}]`)
		wantJSON(t, coerceWith(t, Tuple([]Object{item}), js(t, `[{"n":null}]`)), `[{}]`)
	})
	t.Run("coerce tuple items by type in pass 3", func(t *testing.T) {
		schema := js(t, `{"type":"array","prefixItems":[{"type":["integer","null"]},{"type":["boolean","null"]}]}`)
		wantJSON(t, coerceWith(t, schema, js(t, `["5","true"]`)), `[5,true]`)
	})
	t.Run("convert the properties a union node declares besides its branches", func(t *testing.T) {
		v := mustCompile(t, js(t, `{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"anyOf":[{"required":["a"]},{"required":["b"]}]}`), Options{Coerce: true})
		if got := parseJSON(t, v, js(t, `{"a":"3.5"}`)); got != `{"ok":true,"value":{"a":3}}` {
			t.Error(got)
		}
	})
	for _, keyword := range []string{"anyOf", "oneOf"} {
		t.Run("check "+keyword+" candidates after the node's own properties convert", func(t *testing.T) {
			v := mustCompile(t, js(t, `{"type":"object","`+keyword+`":[
				{"type":"object","properties":{"a":{"type":"array","items":{"type":"integer"}}},"required":["a"]},
				{"type":"object","properties":{"b":{"type":"boolean"}},"required":["b"]}],
				"properties":{"c":{"type":"integer"}},"required":["c"]}`), Options{Coerce: true})
			input := js(t, `{"a":"3","c":"2"}`)
			if got := parseJSON(t, v, input); got != `{"ok":true,"value":{"a":[3],"c":2}}` {
				t.Error(got)
			}
			wantJSON(t, input, `{"a":"3","c":"2"}`)
		})
	}
}

// TestCoerceCyclicSchemaObject: a schema built in Go may hold itself (Compile
// keeps the cycle, as upstream sees one JS object). Upstream's coerce
// overflows V8's stack on {type:"number", allOf:[itself]} and throws
// RangeError; on {anyOf:[{type:"string"}, itself]} it returns "5" for 5,
// its check of the cyclic branch throwing inside coerce, which catches it;
// a cycle through properties is recursion into children.
func TestCoerceCyclicSchemaObject(t *testing.T) {
	all := map[string]any{"type": "number"}
	all["allOf"] = []any{all}
	anyOf := map[string]any{}
	anyOf["anyOf"] = []any{map[string]any{"type": "string"}, anyOf}
	props := map[string]any{"type": "object"}
	props["properties"] = map[string]any{"x": props}
	for _, tc := range []struct {
		schema         any
		value          any
		check          bool
		errors, coerce string
		parse          string
	}{
		{all, "5", false, `[{"path":"","keyword":"type","message":"expected number, got string"}]`, "Maximum call stack size exceeded", "Maximum call stack size exceeded"},
		{all, 5.0, false, "Maximum call stack size exceeded", "Maximum call stack size exceeded", "Maximum call stack size exceeded"},
		{anyOf, "s", true, `[]`, `"s"`, `{"ok":true,"value":"s"}`},
		{anyOf, 5.0, false, "Maximum call stack size exceeded", `"5"`, `{"ok":true,"value":"5"}`},
		{props, Object{{"x", Object{{"x", Object{}}}}}, true, `[]`, `{"x":{"x":{}}}`, `{"ok":true,"value":{"x":{"x":{}}}}`},
		{props, Object{{"x", 5.0}}, false, `[{"path":"/x","keyword":"type","message":"expected object, got integer"}]`, `{"x":5}`,
			`{"ok":false,"errors":[{"path":"/x","keyword":"type","message":"expected object, got integer"}]}`},
	} {
		v := mustCompile(t, tc.schema, Options{Coerce: true})
		show := func(v any, err error) string {
			if err != nil {
				return err.Error()
			}
			return stringify(v)
		}
		errs, err := v.Errors(tc.value)
		if errs == nil && err == nil {
			errs = []ValidationError{}
		}
		coerced, coerceErr := v.Coerce(tc.value)
		parsed, parseErrs, parseErr := v.Parse(tc.value)
		parse := `{"ok":true,"value":` + stringify(parsed) + `}`
		if parseErr != nil {
			parse = parseErr.Error()
		} else if parseErrs != nil {
			parse = `{"ok":false,"errors":` + stringify(errorList(parseErrs)) + `}`
		}
		if got := [...]any{v.Check(tc.value), show(errorList(errs), err), show(coerced, coerceErr), parse}; got != [...]any{tc.check, tc.errors, tc.coerce, tc.parse} {
			t.Errorf("%v: got %q", stringify(tc.value), got)
		}
	}
}
