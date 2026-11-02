package ai

import (
	"encoding/json"
	"iter"
	"slices"
	"sort"
	"strings"
)

// Schema is a JSON Schema node used for tool parameters. It is the Go analogue
// of pi's TypeBox schemas: it serializes to standard JSON Schema for providers,
// and ValidateToolArguments compiles that JSON with package schema (pi-schema)
// to coerce and validate decoded tool-call arguments.
//
// Property order is preserved (PropertyOrder) so serialization is deterministic,
// which keeps provider prompt caches stable across requests.
type Schema struct {
	Type          string
	Description   string
	Properties    map[string]*Schema
	PropertyOrder []string
	Required      []string
	Items         *Schema
	Enum          []any
	Default       any
	// AdditionalAllowed maps to additionalProperties: bool (when AdditionalSchema is nil).
	AdditionalAllowed *bool
	AdditionalSchema  *Schema
	Minimum           *float64
	Maximum           *float64
	ExclusiveMinimum  *float64
	ExclusiveMaximum  *float64
	MultipleOf        *float64
	MinLength         *int
	MaxLength         *int
	Pattern           string
	MinItems          *int
	MaxItems          *int
	// Const is the JSON Schema "const" keyword; HasConst distinguishes
	// const:null from no const.
	Const    any
	HasConst bool
	Format   string
	// Nullable adds "null" to the JSON type (type becomes [Type, "null"]).
	Nullable bool
	AnyOf    []*Schema
	OneOf    []*Schema
	AllOf    []*Schema
	// Extra holds passthrough keywords not modeled above, and modeled ones
	// whose value the typed field cannot hold ("minItems": 1.5, "format": 5,
	// "minimum": null), so they reach the wire and the strict-mode keyword
	// checks as pi sends them instead of being dropped.
	Extra map[string]any
	// KeywordOrder is the order the keywords serialize in. UnmarshalJSON
	// records the source's, and the builders record TypeBox's, so a schema
	// reaches the wire in the order pi sends it. Keywords present but not
	// listed follow in the default order.
	KeywordOrder []string
}

// Field is a named object property used by Object.
type Field struct {
	Name     string
	Schema   *Schema
	Optional bool
}

// Prop declares a required object property.
func Prop(name string, s *Schema) Field { return Field{Name: name, Schema: s} }

// Opt declares an optional object property.
func Opt(name string, s *Schema) Field { return Field{Name: name, Schema: s, Optional: true} }

// Object builds an object schema from ordered fields. Non-optional fields are
// marked required.
func Object(fields ...Field) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for _, f := range fields {
		s.Properties[f.Name] = f.Schema
		s.PropertyOrder = append(s.PropertyOrder, f.Name)
		if !f.Optional {
			s.Required = append(s.Required, f.Name)
		}
	}
	// TypeBox's Type.Object emits {type, required, properties}, with no
	// required key when every property is optional; one added later (strict
	// conversion) then lands after properties, as a new JS key does.
	if len(s.Required) > 0 {
		s.KeywordOrder = []string{"type", "required", "properties"}
	}
	return s
}

// String builds a string schema with an optional description.
func String(desc ...string) *Schema { return &Schema{Type: "string", Description: first(desc)} }

// Number builds a number schema.
func Number(desc ...string) *Schema { return &Schema{Type: "number", Description: first(desc)} }

// Integer builds an integer schema.
func Integer(desc ...string) *Schema { return &Schema{Type: "integer", Description: first(desc)} }

// Boolean builds a boolean schema.
func Boolean(desc ...string) *Schema { return &Schema{Type: "boolean", Description: first(desc)} }

// ArrayOf builds an array schema with the given item schema.
func ArrayOf(item *Schema, desc ...string) *Schema {
	// TypeBox's Type.Array emits {type, items, ...options}.
	return &Schema{Type: "array", Items: item, Description: first(desc), KeywordOrder: []string{"type", "items"}}
}

// EnumOf builds a string enum schema.
func EnumOf(values ...string) *Schema {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	return &Schema{Type: "string", Enum: vals}
}

// Describe sets the description and returns the schema (chainable).
func (s *Schema) Describe(desc string) *Schema { s.Description = desc; return s }

// WithDefault sets a default value and returns the schema (chainable).
func (s *Schema) WithDefault(v any) *Schema { s.Default = v; return s }

func first(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// Keywords yields the schema's keywords with their values in serialization
// order: KeywordOrder first, then any present keyword it does not list, in the
// default order (type, description, properties, required, items, enum, const,
// default, additionalProperties, the numeric bounds, pattern, the item counts,
// format, anyOf, oneOf, allOf, then Extra sorted). It is the one walk behind
// MarshalJSON and the strict-mode keyword checks, the analogue of pi's
// Object.entries over the same schema object. "properties" yields a value
// that marshals in OrderedProperties order.
func (s *Schema) Keywords() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		all := s.defaultKeywords()
		emitted := make([]bool, len(all))
		for _, key := range s.KeywordOrder {
			for i, kw := range all {
				if !emitted[i] && kw.key == key {
					emitted[i] = true
					if !yield(kw.key, kw.value) {
						return
					}
				}
			}
		}
		for i, kw := range all {
			if !emitted[i] && !yield(kw.key, kw.value) {
				return
			}
		}
	}
}

type schemaKeyword struct {
	key   string
	value any
}

func (s *Schema) defaultKeywords() []schemaKeyword {
	var out []schemaKeyword
	add := func(key string, value any) { out = append(out, schemaKeyword{key, value}) }
	if s.Type != "" {
		if s.Nullable {
			add("type", []string{s.Type, "null"})
		} else {
			add("type", s.Type)
		}
	}
	if s.Description != "" {
		add("description", s.Description)
	}
	if s.Properties != nil {
		add("properties", orderedProperties{s})
	}
	if s.Required != nil {
		add("required", s.Required)
	}
	if s.Items != nil {
		add("items", s.Items)
	}
	if len(s.Enum) > 0 {
		add("enum", s.Enum)
	}
	if s.HasConst {
		add("const", s.Const)
	}
	if s.Default != nil {
		add("default", s.Default)
	}
	if s.AdditionalSchema != nil {
		add("additionalProperties", s.AdditionalSchema)
	} else if s.AdditionalAllowed != nil {
		add("additionalProperties", *s.AdditionalAllowed)
	}
	for _, f := range []struct {
		key   string
		value *float64
	}{
		{"minimum", s.Minimum}, {"maximum", s.Maximum},
		{"exclusiveMinimum", s.ExclusiveMinimum}, {"exclusiveMaximum", s.ExclusiveMaximum},
		{"multipleOf", s.MultipleOf},
	} {
		if f.value != nil {
			add(f.key, *f.value)
		}
	}
	if s.MinLength != nil {
		add("minLength", *s.MinLength)
	}
	if s.MaxLength != nil {
		add("maxLength", *s.MaxLength)
	}
	if s.Pattern != "" {
		add("pattern", s.Pattern)
	}
	if s.MinItems != nil {
		add("minItems", *s.MinItems)
	}
	if s.MaxItems != nil {
		add("maxItems", *s.MaxItems)
	}
	if s.Format != "" {
		add("format", s.Format)
	}
	if len(s.AnyOf) > 0 {
		add("anyOf", s.AnyOf)
	}
	if len(s.OneOf) > 0 {
		add("oneOf", s.OneOf)
	}
	if len(s.AllOf) > 0 {
		add("allOf", s.AllOf)
	}
	for _, k := range sortedKeys(s.Extra) {
		add(k, s.Extra[k])
	}
	return out
}

// orderedProperties marshals an object schema's properties in
// OrderedProperties order.
type orderedProperties struct{ s *Schema }

func (p orderedProperties) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range p.s.OrderedProperties() {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(name)
		b.Write(k)
		b.WriteByte(':')
		raw, err := json.Marshal(p.s.Properties[name])
		if err != nil {
			return nil, err
		}
		b.Write(raw)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// MarshalJSON renders the schema as standard JSON Schema, keywords in
// Keywords order.
func (s *Schema) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for key, val := range s.Keywords() {
		raw, err := json.Marshal(val)
		if err != nil {
			return nil, err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(raw)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// UnmarshalJSON decodes a JSON Schema object into a Schema, preserving property
// order from the source bytes.
// OrderedProperties returns an object schema's property names in the order
// they serialize, the way pi reads them with Object.keys(properties): JS
// insertion order is PropertyOrder, with a sorted fallback when no order was
// recorded. MarshalJSON and the strict tool schema conversion both derive
// property order from this one rule, so the `required` array that strict
// conversion rebuilds from the property names always matches the order
// `properties` appears in on the wire. The result is a fresh, non-nil slice
// the caller may keep.
func (s *Schema) OrderedProperties() []string {
	if len(s.PropertyOrder) > 0 {
		return append([]string(nil), s.PropertyOrder...)
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Schema) UnmarshalJSON(data []byte) error {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(data, &generic); err != nil {
		return err
	}
	s.Extra = map[string]any{}
	s.KeywordOrder = jsonObjectKeyOrder(data)
	// keep stores a modeled keyword whose value the typed field cannot hold
	// in Extra, so it is neither dropped nor read as a zero value.
	keep := func(key string, raw json.RawMessage) {
		var v any
		_ = json.Unmarshal(raw, &v) // an overflowing number stays nil: JSON.stringify(Infinity) is null
		s.Extra[key] = v
	}
	for key, raw := range generic {
		isNull := string(raw) == "null"
		switch key {
		case "type":
			var single string
			if err := json.Unmarshal(raw, &single); err == nil {
				s.Type = single
				break
			}
			var multi []string
			if err := json.Unmarshal(raw, &multi); err == nil {
				for _, t := range multi {
					if t == "null" {
						s.Nullable = true
					} else {
						s.Type = t
					}
				}
			}
		case "description":
			_ = json.Unmarshal(raw, &s.Description)
		case "properties":
			var props map[string]*Schema
			if err := json.Unmarshal(raw, &props); err != nil {
				return err
			}
			s.Properties = props
			s.PropertyOrder = jsonObjectKeyOrder(raw)
		case "required":
			_ = json.Unmarshal(raw, &s.Required)
		case "items":
			var it Schema
			if err := json.Unmarshal(raw, &it); err == nil {
				s.Items = &it
			} else {
				keep(key, raw) // not one schema: the tuple form [schema, ...], or a boolean
			}
		case "enum":
			if json.Unmarshal(raw, &s.Enum) != nil || len(s.Enum) == 0 {
				s.Enum = nil
				keep(key, raw) // [], null or not an array: still present, as in pi
			}
		case "default":
			_ = json.Unmarshal(raw, &s.Default)
		case "additionalProperties":
			var b bool
			if err := json.Unmarshal(raw, &b); err == nil {
				s.AdditionalAllowed = &b
				break
			}
			var sub Schema
			if err := json.Unmarshal(raw, &sub); err == nil {
				s.AdditionalSchema = &sub
			}
		case "minimum":
			if s.Minimum = unmarshalFloatPtr(raw); s.Minimum == nil || isNull {
				s.Minimum = nil
				keep(key, raw)
			}
		case "maximum":
			if s.Maximum = unmarshalFloatPtr(raw); s.Maximum == nil || isNull {
				s.Maximum = nil
				keep(key, raw)
			}
		case "exclusiveMinimum":
			if s.ExclusiveMinimum = unmarshalFloatPtr(raw); s.ExclusiveMinimum == nil || isNull {
				s.ExclusiveMinimum = nil
				keep(key, raw)
			}
		case "exclusiveMaximum":
			if s.ExclusiveMaximum = unmarshalFloatPtr(raw); s.ExclusiveMaximum == nil || isNull {
				s.ExclusiveMaximum = nil
				keep(key, raw)
			}
		case "multipleOf":
			if s.MultipleOf = unmarshalFloatPtr(raw); s.MultipleOf == nil || isNull {
				s.MultipleOf = nil
				keep(key, raw)
			}
		case "minLength":
			if s.MinLength = unmarshalIntPtr(raw); s.MinLength == nil || isNull {
				s.MinLength = nil
				keep(key, raw)
			}
		case "maxLength":
			if s.MaxLength = unmarshalIntPtr(raw); s.MaxLength == nil || isNull {
				s.MaxLength = nil
				keep(key, raw)
			}
		case "pattern":
			if json.Unmarshal(raw, &s.Pattern) != nil || s.Pattern == "" {
				s.Pattern = ""
				keep(key, raw)
			}
		case "minItems":
			if s.MinItems = unmarshalIntPtr(raw); s.MinItems == nil || isNull {
				s.MinItems = nil
				keep(key, raw)
			}
		case "maxItems":
			if s.MaxItems = unmarshalIntPtr(raw); s.MaxItems == nil || isNull {
				s.MaxItems = nil
				keep(key, raw)
			}
		case "const":
			_ = json.Unmarshal(raw, &s.Const)
			s.HasConst = true
		case "format":
			if json.Unmarshal(raw, &s.Format) != nil || s.Format == "" {
				s.Format = ""
				keep(key, raw)
			}
		case "anyOf":
			_ = json.Unmarshal(raw, &s.AnyOf)
		case "oneOf":
			_ = json.Unmarshal(raw, &s.OneOf)
		case "allOf":
			_ = json.Unmarshal(raw, &s.AllOf)
		default:
			var v any
			_ = json.Unmarshal(raw, &v)
			s.Extra[key] = v
		}
	}
	if len(s.Extra) == 0 {
		s.Extra = nil
	}
	return nil
}

// Clone returns a deep copy of the schema (nil-safe). It is the analogue of
// pi's structuredClone(schema) for callers that mutate a copy (strict tool
// schema conversion): nothing reachable from the returned schema aliases the
// receiver. Empty-but-non-nil slices and maps stay non-nil, so "present but
// empty" keywords (e.g. anyOf: []) survive the copy the way structuredClone
// preserves them.
func (s *Schema) Clone() *Schema {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Properties != nil {
		cp.Properties = make(map[string]*Schema, len(s.Properties))
		for name, prop := range s.Properties {
			cp.Properties[name] = prop.Clone()
		}
	}
	cp.PropertyOrder = slices.Clone(s.PropertyOrder)
	cp.KeywordOrder = slices.Clone(s.KeywordOrder)
	cp.Required = slices.Clone(s.Required)
	cp.Items = s.Items.Clone()
	if s.Enum != nil {
		cp.Enum = make([]any, len(s.Enum))
		for i, v := range s.Enum {
			cp.Enum[i] = deepCopy(v)
		}
	}
	cp.Default = deepCopy(s.Default)
	cp.AdditionalAllowed = clonePtr(s.AdditionalAllowed)
	cp.AdditionalSchema = s.AdditionalSchema.Clone()
	cp.Minimum = clonePtr(s.Minimum)
	cp.Maximum = clonePtr(s.Maximum)
	cp.ExclusiveMinimum = clonePtr(s.ExclusiveMinimum)
	cp.ExclusiveMaximum = clonePtr(s.ExclusiveMaximum)
	cp.MultipleOf = clonePtr(s.MultipleOf)
	cp.MinLength = clonePtr(s.MinLength)
	cp.MaxLength = clonePtr(s.MaxLength)
	cp.MinItems = clonePtr(s.MinItems)
	cp.MaxItems = clonePtr(s.MaxItems)
	cp.Const = deepCopy(s.Const)
	cp.AnyOf = cloneSchemas(s.AnyOf)
	cp.OneOf = cloneSchemas(s.OneOf)
	cp.AllOf = cloneSchemas(s.AllOf)
	if s.Extra != nil {
		cp.Extra = make(map[string]any, len(s.Extra))
		for key, v := range s.Extra {
			cp.Extra[key] = deepCopy(v)
		}
	}
	return &cp
}

func cloneSchemas(list []*Schema) []*Schema {
	if list == nil {
		return nil
	}
	out := make([]*Schema, len(list))
	for i, s := range list {
		out[i] = s.Clone()
	}
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func unmarshalFloatPtr(raw json.RawMessage) *float64 {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return &f
}

func unmarshalIntPtr(raw json.RawMessage) *int {
	var i int
	if err := json.Unmarshal(raw, &i); err != nil {
		return nil
	}
	return &i
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonObjectKeyOrder returns the top-level object keys of raw in source order.
func jsonObjectKeyOrder(raw json.RawMessage) []string {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	var keys []string
	depth := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return keys
		}
		if depth == 0 {
			if k, ok := keyTok.(string); ok {
				keys = append(keys, k)
			}
		}
		// Skip the value (which may be a nested object/array).
		if err := skipValue(dec); err != nil {
			return keys
		}
		_ = depth
	}
	return keys
}

func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		for dec.More() {
			if d == '{' {
				if _, err := dec.Token(); err != nil { // key
					return err
				}
			}
			if err := skipValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing delim
			return err
		}
	}
	return nil
}

// deepCopy copies a JSON value through a marshal round trip (Clone's copy of
// Default, Const, Enum and Extra values).
func deepCopy(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}
