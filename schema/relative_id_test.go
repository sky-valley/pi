package schema

import "testing"

// A relative root $id with a directory part is applied once (upstream
// 2bd2c2d0a, #11697/#11726). The root base was the $id applied to the
// default base, and the walkers applied it again from there, so local
// references resolved under https://json-schema.invalid/schemas/schemas/:
// compile threw "Unresolved $ref #/$defs/count", and with unresolvedRefs
// "reject" — how pi-ai validates tool arguments — valid arguments failed.
func TestRelativeRootIDAppliedOnce(t *testing.T) {
	t.Run("local references", func(t *testing.T) {
		schema := js(t, `{"$id":"schemas/tool.json","type":"object","properties":{"count":{"$ref":"#/$defs/count"}},"required":["count"],"$defs":{"count":{"type":"integer"}}}`)
		if !mustCompile(t, schema, Options{}).Check(js(t, `{"count":2}`)) {
			t.Error("check failed")
		}
		v := mustCompile(t, schema, Options{UnresolvedRefs: UnresolvedReject})
		if !v.Check(js(t, `{"count":2}`)) {
			t.Error("reject: check failed")
		}
		wantErrors(t, mustErrors(t, v, js(t, `{"count":"a"}`)), ValidationError{"/count", "type", "expected integer, got string"})
		coerced, errs := mustParse(t, mustCompile(t, schema, Options{Coerce: true}), js(t, `{"count":"2"}`))
		if len(errs) != 0 || stringify(coerced) != `{"count":2}` {
			t.Errorf("parse = %s, %v", stringify(coerced), errs)
		}
	})
	t.Run("unevaluated keywords", func(t *testing.T) {
		props := js(t, `{"$id":"schemas/tool.json","type":"object","properties":{"count":{"$ref":"#/$defs/count"}},"$defs":{"count":{"type":"integer"}},"unevaluatedProperties":false}`)
		v := mustCompile(t, props, Options{UnresolvedRefs: UnresolvedReject})
		if !v.Check(js(t, `{"count":2}`)) {
			t.Error("properties: check failed")
		}
		wantErrors(t, mustErrors(t, v, js(t, `{"count":2,"extra":1}`)), ValidationError{"/extra", "unevaluatedProperties", "unknown field"})
		items := js(t, `{"$id":"schemas/list.json","type":"array","prefixItems":[{"$ref":"#/$defs/count"}],"$defs":{"count":{"type":"integer"}},"unevaluatedItems":false}`)
		if !mustCompile(t, items, Options{UnresolvedRefs: UnresolvedReject}).Check(js(t, `[2]`)) {
			t.Error("items: check failed")
		}
		if mustCompile(t, items, Options{}).Check(js(t, `[2,3]`)) {
			t.Error("items: an unevaluated item passed")
		}
	})
	t.Run("explained under nested resources", func(t *testing.T) {
		cloned := js(t, `{"$id":"https://example.com/root.json","allOf":[{"$ref":"#/$defs/child"}],"$defs":{"child":{"$id":"schemas/tool.json","$ref":"#/$defs/props","unevaluatedProperties":false,"$defs":{"props":{"properties":{"count":{"type":"integer"}}}}}}}`)
		wantErrors(t, mustErrors(t, mustCompile(t, cloned, Options{}), js(t, `{"count":2,"extra":1}`)), ValidationError{"/extra", "unevaluatedProperties", "unknown field"})
		branch := js(t, `{"$id":"https://example.com/root.json","anyOf":[{"$id":"a/b.json","$ref":"#/$defs/n","$defs":{"n":{"type":"integer"}}},{"type":"boolean"}]}`)
		wantErrors(t, mustErrors(t, mustCompile(t, branch, Options{}), js(t, `{}`)), ValidationError{"", "type", "expected integer, got object"})
	})
	// Characterization: upstream's guard for its coercer hunk (baseFor in
	// place of a direct baseOf read), which broke the first coerce of a
	// $dynamicRef schema in pi once the root base changed. The port's
	// compile has already indexed the schema by then, so reverting that hunk
	// does not turn this red here; it locks the behaviour all the same.
	t.Run("coercion follows a relative ref on the first call with $dynamicRef", func(t *testing.T) {
		schema := js(t, `{"$id":"https://example.com/root.json","$dynamicAnchor":"node","type":"object","properties":{"count":{"$ref":"defs/n.json"},"children":{"type":"array","items":{"$dynamicRef":"#node"}}}}`)
		opts := Options{Coerce: true, Schemas: map[string]any{"https://example.com/defs/n.json": js(t, `{"type":"integer"}`)}}
		if got := stringify(mustCoerce(t, mustCompile(t, schema, opts), js(t, `{"count":"2"}`))); got != `{"count":2}` {
			t.Errorf("coerce = %s", got)
		}
		v := mustCompile(t, schema, opts)
		for i := range 2 {
			coerced, errs := mustParse(t, v, js(t, `{"count":"2"}`))
			if len(errs) != 0 || stringify(coerced) != `{"count":2}` {
				t.Errorf("parse %d = %s, %v", i, stringify(coerced), errs)
			}
		}
	})
}
