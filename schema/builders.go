package schema

// The schema builders (builders.ts): short functions that return plain JSON
// Schema objects. Upstream's builders also carry the static type of the
// values a schema accepts (Static, StaticObject); that is TypeScript type
// inference with no Go counterpart, so these return an Object and nothing
// more.
//
// Each builder takes its options as members spread after the keywords it
// writes itself, as upstream's `{ type: "string", ...options }` does: a
// member with a new key is appended, one with a key already written replaces
// its value in place. The option helpers below name every option upstream's
// option types declare.

// Str is a string (builders.ts str), with optional length bounds (in code
// points), pattern and format.
func Str(opts ...Member) Object { return spread(Object{{"type", "string"}}, opts) }

// Num is a number within optional bounds.
func Num(opts ...Member) Object { return spread(Object{{"type", "number"}}, opts) }

// Int is an integer within optional bounds.
func Int(opts ...Member) Object { return spread(Object{{"type", "integer"}}, opts) }

// Bool is a boolean.
func Bool(opts ...Member) Object { return spread(Object{{"type", "boolean"}}, opts) }

// Nul is null.
func Nul(opts ...Member) Object { return spread(Object{{"type", "null"}}, opts) }

// Unknown is any value.
func Unknown(opts ...Member) Object { return spread(Object{}, opts) }

// Lit is exactly value (a string, number, boolean or nil).
func Lit(value any, opts ...Member) Object { return spread(Object{{"const", value}}, opts) }

// OneOf is one of the given values (an `enum`; upstream names it oneOf).
func OneOf(values []any, opts ...Member) Object { return spread(Object{{"enum", values}}, opts) }

// Union is any of the given schemas. A union of objects tagged by a Lit
// property is checked by the tag: only the branch it selects runs, and its
// errors are the ones reported.
func Union(schemas []Object, opts ...Member) Object {
	return spread(Object{{"anyOf", schemaList(schemas)}}, opts)
}

// Nullable is schema or null.
func Nullable(schema Object, opts ...Member) Object {
	return spread(Object{{"anyOf", []any{schema, Object{{"type", "null"}}}}}, opts)
}

// Arr is an array of items, with optional length bounds and unique items.
func Arr(items Object, opts ...Member) Object {
	return spread(Object{{"type", "array"}, {"items", items}}, opts)
}

// Tuple is a fixed-length array whose items match items in order.
func Tuple(items []Object, opts ...Member) Object {
	return spread(Object{
		{"type", "array"},
		{"prefixItems", schemaList(items)},
		{"items", false},
		{"minItems", float64(len(items))},
	}, opts)
}

// Map is an object with string keys and values of value (a record).
func Map(value Object, opts ...Member) Object {
	return spread(Object{{"type", "object"}, {"additionalProperties", value}}, opts)
}

// Bytes is a []byte (a Uint8Array upstream) with optional length bounds: an
// extension of JSON Schema for values that never pass through JSON.
func Bytes(opts ...Member) Object { return spread(Object{{"type", "bytes"}}, opts) }

// Optional marks a property of Obj as optional (builders.ts Optional).
type Optional struct {
	Schema Object
}

// Opt marks an Obj property as optional: Obj(Object{{"name", Opt(Str())}})
// may omit name.
func Opt(schema Object) Optional { return Optional{schema} }

// Obj is a closed object (builders.ts obj): the given properties, each an
// Object (required) or an Optional, and no others. An Open() option allows
// other properties, as tool schemas from other systems often do.
func Obj(properties Object, opts ...Member) Object {
	props := make(Object, 0, len(properties))
	var required []any
	for _, p := range properties {
		if o, ok := p.Value.(Optional); ok {
			props = append(props, Member{p.Key, o.Schema})
			continue
		}
		props = append(props, p)
		required = append(required, p.Key)
	}
	open := false
	annotations := make([]Member, 0, len(opts))
	for _, m := range opts {
		if m.Key == "open" {
			open = m.Value == true
			continue
		}
		annotations = append(annotations, m)
	}
	out := Object{{"type", "object"}, {"properties", props}}
	if len(required) > 0 {
		out = append(out, Member{"required", required})
	}
	if !open {
		out = append(out, Member{"additionalProperties", false})
	}
	return spread(out, annotations)
}

// Open is Obj's `open: true` option.
func Open() Member { return Member{"open", true} }

// Title is the title annotation.
func Title(s string) Member { return Member{"title", s} }

// Description is the description annotation.
func Description(s string) Member { return Member{"description", s} }

// Examples is the examples annotation.
func Examples(values ...any) Member { return Member{"examples", values} }

// Deprecated is the deprecated annotation.
func Deprecated(b bool) Member { return Member{"deprecated", b} }

// Default is the default annotation, for tools and forms; validation never
// inserts it.
func Default(v any) Member { return Member{"default", v} }

// MinLength is a lower length bound of a string (code points) or bytes.
func MinLength(n float64) Member { return Member{"minLength", n} }

// MaxLength is an upper length bound of a string (code points) or bytes.
func MaxLength(n float64) Member { return Member{"maxLength", n} }

// Pattern is a string's pattern, an ECMAScript regular expression.
func Pattern(source string) Member { return Member{"pattern", source} }

// Format is a string's format.
func Format(name string) Member { return Member{"format", name} }

// Minimum is a number's inclusive lower bound.
func Minimum(n float64) Member { return Member{"minimum", n} }

// Maximum is a number's inclusive upper bound.
func Maximum(n float64) Member { return Member{"maximum", n} }

// ExclusiveMinimum is a number's exclusive lower bound.
func ExclusiveMinimum(n float64) Member { return Member{"exclusiveMinimum", n} }

// ExclusiveMaximum is a number's exclusive upper bound.
func ExclusiveMaximum(n float64) Member { return Member{"exclusiveMaximum", n} }

// MultipleOf is the number a number must be a multiple of.
func MultipleOf(n float64) Member { return Member{"multipleOf", n} }

// MinItems is an array's lower length bound.
func MinItems(n float64) Member { return Member{"minItems", n} }

// MaxItems is an array's upper length bound.
func MaxItems(n float64) Member { return Member{"maxItems", n} }

// UniqueItems requires an array's items to be distinct.
func UniqueItems(b bool) Member { return Member{"uniqueItems", b} }

// spread is `{ ...base, ...opts }`.
func spread(base Object, opts []Member) Object {
	for _, m := range opts {
		replaced := false
		for i := range base {
			if base[i].Key == m.Key {
				base[i].Value = m.Value
				replaced = true
				break
			}
		}
		if !replaced {
			base = append(base, m)
		}
	}
	return base
}

func schemaList(schemas []Object) []any {
	out := make([]any, len(schemas))
	for i, s := range schemas {
		out[i] = s
	}
	return out
}
