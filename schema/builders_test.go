package schema

import "testing"

// Ported from upstream's test/builders.test.ts at 6536f8012. The static-type
// tests ("carry the static type of valid values", "narrow values through a
// compiled check") are TypeScript type inference, which has no Go form.

func TestBuildersProducePlainJSONSchema(t *testing.T) {
	schema := Obj(Object{
		{"path", Str(Description("File path"), MinLength(1))},
		{"offset", Opt(Int(Minimum(1)))},
		{"mode", OneOf([]any{"read", "write"})},
	})
	wantJSON(t, schema, `{"type":"object","properties":{"path":{"type":"string","description":"File path","minLength":1},"offset":{"type":"integer","minimum":1},"mode":{"enum":["read","write"]}},"required":["path","mode"],"additionalProperties":false}`)
}

func TestBuildersProduceEveryLeaf(t *testing.T) {
	for _, c := range []struct {
		got  Object
		want string
	}{
		{Str(), `{"type":"string"}`},
		{Num(), `{"type":"number"}`},
		{Int(), `{"type":"integer"}`},
		{Bool(), `{"type":"boolean"}`},
		{Nul(), `{"type":"null"}`},
		{Unknown(), `{}`},
		{Lit(3.0), `{"const":3}`},
		{Bytes(MaxLength(8)), `{"type":"bytes","maxLength":8}`},
		{Arr(Str(), MaxItems(2)), `{"type":"array","items":{"type":"string"},"maxItems":2}`},
		{Map(Num()), `{"type":"object","additionalProperties":{"type":"number"}}`},
		{Tuple([]Object{Str(), Num()}), `{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}],"items":false,"minItems":2}`},
		{Nullable(Str()), `{"anyOf":[{"type":"string"},{"type":"null"}]}`},
		{Obj(Object{{"a", Str()}}, Open()), `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
		// Options spread after the builder's own keywords, replacing in place.
		{Str(Member{"type", "number"}, Title("t"), Default("x"), Examples("a", "b"), Deprecated(true)), `{"type":"number","title":"t","default":"x","examples":["a","b"],"deprecated":true}`},
		{Union([]Object{Str(), Num()}, Description("d")), `{"anyOf":[{"type":"string"},{"type":"number"}],"description":"d"}`},
		{Num(ExclusiveMinimum(0), ExclusiveMaximum(1), MultipleOf(0.5), Maximum(2)), `{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":1,"multipleOf":0.5,"maximum":2}`},
		{Str(Pattern("^a"), Format("email"), MaxLength(3)), `{"type":"string","pattern":"^a","format":"email","maxLength":3}`},
		{Arr(Int(), MinItems(1), UniqueItems(true)), `{"type":"array","items":{"type":"integer"},"minItems":1,"uniqueItems":true}`},
	} {
		wantJSON(t, c.got, c.want)
	}
}

func TestBuildersCompileWithTheDeclaredSemantics(t *testing.T) {
	v := mustCompile(t, Obj(Object{{"name", Str(MinLength(1))}, {"tags", Opt(Arr(Str(), UniqueItems(true)))}}), Options{})
	if !v.Check(Object{{"name", "a"}}) || !v.Check(Object{{"name", "a"}, {"tags", []any{"x", "y"}}}) {
		t.Error("valid values failed")
	}
	wantErrors(t, v.Errors(Object{{"name", ""}, {"tags", []any{"x", "x"}}, {"other", 1.0}}),
		ValidationError{"/name", "minLength", "expected at least 1 characters"},
		ValidationError{"/tags", "uniqueItems", "expected unique items"},
		ValidationError{"/other", "additionalProperties", "unknown field"})
}

func TestTaggedUnionOfBuilders(t *testing.T) {
	message := Union([]Object{
		Obj(Object{{"kind", Lit("text")}, {"text", Str()}}),
		Obj(Object{{"kind", Lit("image")}, {"url", Str()}, {"alt", Opt(Str())}}),
	})
	v := mustCompile(t, message, Options{})
	if !v.Check(Object{{"kind", "image"}, {"url", "u"}}) {
		t.Error("valid image failed")
	}
	wantErrors(t, v.Errors(Object{{"kind", "image"}, {"text", "t"}}),
		ValidationError{"/url", "required", "missing"},
		ValidationError{"/text", "additionalProperties", "unknown field"})
}
