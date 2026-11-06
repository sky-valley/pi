package schema

import (
	"math/big"
	"reflect"
	"slices"
)

// schemaObject is a schema object as the compiler reads it: its keywords in
// own-key order with an index, and an identity. Upstream caches compiled
// checks, base URIs and anchors by the schema object (a Map keyed by
// reference); Compile converts the caller's schema into these once, so a
// pointer is that reference. The tree is never modified after conversion and
// is shared by the compiles of one Validator.
type schemaObject struct {
	members []Member
	index   map[string]int
}

func (s *schemaObject) get(key string) (any, bool) {
	if i, ok := s.index[key]; ok {
		return s.members[i].Value, true
	}
	return nil, false
}

// kw is the keyword's value, nil when absent (`node[k]`, where upstream tests
// `=== undefined`; JSON's null is held as nil too, and every test upstream
// makes of a keyword that may be null also asks for a type, so the two never
// meet).
func (s *schemaObject) kw(key string) any {
	v, _ := s.get(key)
	return v
}

func (s *schemaObject) has(key string) bool {
	_, ok := s.index[key]
	return ok
}

// without is the object less the keys `drop` names (a rest spread).
func (s *schemaObject) without(drop func(string) bool) *schemaObject {
	out := &schemaObject{index: map[string]int{}}
	for _, m := range s.members {
		if !drop(m.Key) {
			out.index[m.Key] = len(out.members)
			out.members = append(out.members, m)
		}
	}
	return out
}

// toTree converts a schema (or a document of options.Schemas) into the tree
// the compiler reads: every object a *schemaObject, every array []any, every
// number float64, including inside `const`, `enum` and other data keywords,
// which only changes how they are held. Objects and arrays reached twice — shared, or in a cycle —
// convert to the one tree node, as upstream sees one JS object.
func toTree(v any) any {
	c := converter{seen: map[identity]any{}}
	return c.convert(v)
}

type converter struct {
	seen map[identity]any
}

func (c *converter) convert(v any) any {
	if f, ok := numberOf(v); ok {
		return f
	}
	switch x := v.(type) {
	case nil, bool, string:
		return v
	case *schemaObject:
		return x
	case *big.Int:
		if x != nil {
			// A copy: a caller changing its bigint after Compile does not
			// change the validator's bounds or const.
			return new(big.Int).Set(x)
		}
	case Object:
		return c.object(v, len(x), func(add func(string, any)) {
			for _, m := range x {
				add(m.Key, m.Value)
			}
		})
	case map[string]any:
		return c.object(v, len(x), func(add func(string, any)) {
			for _, k := range ownKeys(x) {
				add(k, x[k])
			}
		})
	case []any:
		id, _ := identityOf(x)
		if out, ok := c.seen[id]; ok && len(x) > 0 {
			return out
		}
		out := make([]any, len(x))
		if len(x) > 0 {
			c.seen[id] = out
		}
		for i, e := range x {
			out[i] = c.convert(e)
		}
		return out
	}
	// Other Go shapes a hand-written schema may use: a slice of any element
	// type ([]string for `required`), a map with string keys.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if _, ok := v.([]byte); ok {
			return v
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = c.convert(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		slices.SortFunc(keys, compareKeys)
		return c.object(v, len(keys), func(add func(string, any)) {
			for _, k := range keys {
				add(k, rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface())
			}
		})
	}
	return v
}

func (c *converter) object(v any, n int, each func(add func(string, any))) any {
	id, hasID := identityOf(v)
	if hasID && (id.kind == 'm' || id.n > 0) {
		if out, ok := c.seen[id]; ok {
			return out
		}
	} else {
		hasID = false
	}
	out := &schemaObject{members: make([]Member, 0, n), index: make(map[string]int, n)}
	if hasID {
		c.seen[id] = out
	}
	each(func(k string, x any) {
		out.index[k] = len(out.members)
		out.members = append(out.members, Member{k, c.convert(x)})
	})
	return out
}
