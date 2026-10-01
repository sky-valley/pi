package ai

import (
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/sky-valley/pi/internal/jstext"
)

// Schema is a JSON Schema node used for tool parameters. It is the Go analogue
// of pi's TypeBox schemas: it serializes to standard JSON Schema for providers
// and supports coercion + validation of decoded tool-call arguments.
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
			}
		case "enum":
			_ = json.Unmarshal(raw, &s.Enum)
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

// ---------------------------------------------------------------------------
// Coercion + validation (port of validation.ts)
// ---------------------------------------------------------------------------

func (s *Schema) schemaTypes() []string {
	if s.Type == "" {
		return nil
	}
	if s.Nullable {
		return []string{s.Type, "null"}
	}
	return []string{s.Type}
}

func matchesJSONType(value any, typ string) bool {
	switch typ {
	case "number":
		// TypeBox's number guard is Number.isFinite: NaN/±Inf are not numbers.
		f, ok := toFloat(value)
		return ok && !math.IsNaN(f) && !math.IsInf(f, 0)
	case "integer":
		// Number.isInteger: finite and integral.
		f, ok := toFloat(value)
		return ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "null":
		return value == nil
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// jsNumber is jstext.StringToNumber with NaN reported as ok=false. Note ok=true
// may still yield ±Inf ("Infinity", "1e1000"); callers gate with
// Number.isFinite/isInteger semantics.
func jsNumber(value string) (float64, bool) {
	f := jstext.StringToNumber(value)
	return f, !math.IsNaN(f)
}

func coercePrimitiveByType(value any, typ string) any {
	switch typ {
	case "number":
		if value == nil {
			return float64(0)
		}
		// pi (validation.ts:88-92): Number(value) gated by Number.isFinite, with
		// a value.trim() !== "" guard ("" / whitespace would coerce to 0).
		if str, ok := value.(string); ok && jstext.Trim(str) != "" {
			if parsed, ok := jsNumber(str); ok && !math.IsInf(parsed, 0) {
				return parsed
			}
		}
		if b, ok := value.(bool); ok {
			if b {
				return float64(1)
			}
			return float64(0)
		}
		return value
	case "integer":
		if value == nil {
			return float64(0)
		}
		// pi (validation.ts:97-101): Number(value) gated by Number.isInteger.
		if str, ok := value.(string); ok && jstext.Trim(str) != "" {
			if parsed, ok := jsNumber(str); ok && !math.IsInf(parsed, 0) && parsed == math.Trunc(parsed) {
				return parsed
			}
		}
		if b, ok := value.(bool); ok {
			if b {
				return float64(1)
			}
			return float64(0)
		}
		return value
	case "boolean":
		if value == nil {
			return false
		}
		if str, ok := value.(string); ok {
			if str == "true" {
				return true
			}
			if str == "false" {
				return false
			}
		}
		if f, ok := toFloat(value); ok {
			if f == 1 {
				return true
			}
			if f == 0 {
				return false
			}
		}
		return value
	case "string":
		if value == nil {
			return ""
		}
		// pi validation.ts:135-136 uses String(value).
		switch v := value.(type) {
		case float64:
			return jstext.NumberToString(v)
		case bool:
			return strconv.FormatBool(v)
		}
		return value
	case "null":
		if value == "" || value == float64(0) || value == false {
			return nil
		}
		return value
	default:
		return value
	}
}

// Coerce applies JSON-schema-directed coercion to a decoded value, returning a
// possibly-new value (mirrors coerceWithJsonSchema).
func (s *Schema) Coerce(value any) any {
	next := value

	for _, nested := range s.AllOf {
		next = nested.Coerce(next)
	}
	if len(s.AnyOf) > 0 {
		next = coerceWithUnion(next, s.AnyOf)
	}
	if len(s.OneOf) > 0 {
		next = coerceWithUnion(next, s.OneOf)
	}

	types := s.schemaTypes()
	matchesUnionMember := false
	if len(types) > 1 {
		for _, t := range types {
			if matchesJSONType(next, t) {
				matchesUnionMember = true
				break
			}
		}
	}
	if len(types) > 0 && !matchesUnionMember {
		for _, t := range types {
			candidate := coercePrimitiveByType(next, t)
			if !valueEqual(candidate, next) {
				next = candidate
				break
			}
		}
	}

	if containsStr(types, "object") {
		if obj, ok := next.(map[string]any); ok {
			s.coerceObject(obj)
		}
	}
	if containsStr(types, "array") {
		if arr, ok := next.([]any); ok {
			s.coerceArray(arr)
		}
	}
	return next
}

func (s *Schema) coerceObject(value map[string]any) {
	defined := map[string]bool{}
	for name, propSchema := range s.Properties {
		defined[name] = true
		if v, ok := value[name]; ok {
			value[name] = propSchema.Coerce(v)
		}
	}
	if s.AdditionalSchema != nil {
		for key, v := range value {
			if defined[key] {
				continue
			}
			value[key] = s.AdditionalSchema.Coerce(v)
		}
	}
}

func (s *Schema) coerceArray(value []any) {
	if s.Items == nil {
		return
	}
	for i := range value {
		value[i] = s.Items.Coerce(value[i])
	}
}

func coerceWithUnion(value any, schemas []*Schema) any {
	// A value that already satisfies one of the branches is returned untouched,
	// so coercion against a different branch cannot mangle it (e.g. null in a
	// nullable union being turned into a number).
	for _, sub := range schemas {
		if len(sub.validate(value, "")) == 0 {
			return value
		}
	}

	for _, sub := range schemas {
		candidate := deepCopy(value)
		coerced := sub.Coerce(candidate)
		if len(sub.validate(coerced, "")) == 0 {
			return coerced
		}
	}
	return value
}

func containsStr(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

func valueEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

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

// ValidationError is a single schema violation.
type ValidationError struct {
	Path    string
	Message string
}

// validate checks value against the schema, accumulating errors with paths.
func (s *Schema) validate(value any, path string) []ValidationError {
	var errs []ValidationError

	for _, sub := range s.AllOf {
		errs = append(errs, sub.validate(value, path)...)
	}
	if len(s.AnyOf) > 0 {
		matched := false
		for _, sub := range s.AnyOf {
			if len(sub.validate(value, path)) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: "must match a schema in anyOf"})
		}
	}
	if len(s.OneOf) > 0 {
		count := 0
		for _, sub := range s.OneOf {
			if len(sub.validate(value, path)) == 0 {
				count++
			}
		}
		if count != 1 {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: "must match exactly one schema in oneOf"})
		}
	}

	types := s.schemaTypes()
	if len(types) > 0 {
		matched := false
		for _, t := range types {
			if matchesJSONType(value, t) {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, ValidationError{
				Path:    pathOr(path),
				Message: typeErrorMessage(types),
			})
			return errs
		}
	}

	if len(s.Enum) > 0 {
		matched := false
		for _, e := range s.Enum {
			if valueEqual(e, value) {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be equal to one of the allowed values"})
		}
	}
	if s.HasConst && !valueEqual(s.Const, value) {
		errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be equal to constant"})
	}

	// Keyword enforcement dispatches on the value's JSON type (JSON Schema
	// semantics: keywords for other types are ignored), so keywords also apply
	// when "type" is absent. Error wording matches TypeBox's en_US locale,
	// which pi surfaces via error.message.
	switch v := value.(type) {
	case map[string]any:
		if len(s.Required) > 0 {
			var missing []string
			for _, req := range s.Required {
				if _, ok := v[req]; !ok {
					missing = append(missing, req)
				}
			}
			if len(missing) > 0 {
				// TypeBox emits one "required" error listing every missing
				// property; pi's formatValidationPath paths it at the first.
				errs = append(errs, ValidationError{
					Path:    joinPath(path, missing[0]),
					Message: "must have required properties " + strings.Join(missing, ", "),
				})
			}
		}
		for name, propSchema := range s.Properties {
			if pv, ok := v[name]; ok {
				errs = append(errs, propSchema.validate(pv, joinPath(path, name))...)
			}
		}
		// additionalProperties enforcement (TypeBox .Check rejects extra keys).
		// additionalProperties:false → unknown keys are errors; an additionalProperties
		// schema → unknown keys are validated against it.
		if s.AdditionalSchema != nil {
			for key, pv := range v {
				if _, defined := s.Properties[key]; defined {
					continue
				}
				errs = append(errs, s.AdditionalSchema.validate(pv, joinPath(path, key))...)
			}
		} else if s.AdditionalAllowed != nil && !*s.AdditionalAllowed {
			for key := range v {
				if _, defined := s.Properties[key]; !defined {
					// TypeBox emits a single error at the object path.
					errs = append(errs, ValidationError{Path: pathOr(path), Message: "must not have additional properties"})
					break
				}
			}
		}
	case []any:
		if s.Items != nil {
			for i, item := range v {
				errs = append(errs, s.Items.validate(item, joinPath(path, strconv.Itoa(i)))...)
			}
		}
		if s.MinItems != nil && len(v) < *s.MinItems {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: fmt.Sprintf("must not have fewer than %d items", *s.MinItems)})
		}
		if s.MaxItems != nil && len(v) > *s.MaxItems {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: fmt.Sprintf("must not have more than %d items", *s.MaxItems)})
		}
	case string:
		if s.MinLength != nil && len([]rune(v)) < *s.MinLength {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: fmt.Sprintf("must not have fewer than %d characters", *s.MinLength)})
		}
		if s.MaxLength != nil && len([]rune(v)) > *s.MaxLength {
			errs = append(errs, ValidationError{Path: pathOr(path), Message: fmt.Sprintf("must not have more than %d characters", *s.MaxLength)})
		}
		if s.Pattern != "" {
			// TypeBox tests new RegExp(pattern, "u") (unanchored). Go's RE2
			// cannot compile some valid JS patterns (lookaround/backrefs);
			// those are unenforceable here and skipped without error.
			if re := compiledPattern(s.Pattern); re != nil && !re.MatchString(v) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: `must match pattern "` + s.Pattern + `"`})
			}
		}
	default:
		if f, ok := toFloat(value); ok {
			if s.Minimum != nil && !(f >= *s.Minimum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be >= " + jstext.NumberToString(*s.Minimum)})
			}
			if s.Maximum != nil && !(f <= *s.Maximum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be <= " + jstext.NumberToString(*s.Maximum)})
			}
			if s.ExclusiveMinimum != nil && !(f > *s.ExclusiveMinimum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be > " + jstext.NumberToString(*s.ExclusiveMinimum)})
			}
			if s.ExclusiveMaximum != nil && !(f < *s.ExclusiveMaximum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be < " + jstext.NumberToString(*s.ExclusiveMaximum)})
			}
			if s.MultipleOf != nil && !isJSMultipleOf(f, *s.MultipleOf) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be multiple of " + jstext.NumberToString(*s.MultipleOf)})
			}
		}
	}

	return errs
}

func typeErrorMessage(types []string) string {
	if len(types) == 1 {
		return "must be " + types[0]
	}
	return "must be either " + strings.Join(types, " or ")
}

// isJSMultipleOf ports TypeBox's Guard.IsMultipleOf (guard.mjs), including its
// 1e-10 float tolerance and the integer-dividend/integral-reciprocal shortcut.
func isJSMultipleOf(dividend, divisor float64) bool {
	const tolerance = 1e-10
	if math.IsNaN(dividend) || math.IsInf(dividend, 0) {
		return true // !IsNumber(dividend) → true in TypeBox
	}
	if dividend == math.Trunc(dividend) && math.Mod(1/divisor, 1) == 0 {
		return true
	}
	mod := math.Mod(dividend, divisor)
	return math.Min(math.Abs(mod), math.Abs(mod-divisor)) < tolerance
}

// patternCache memoizes compiled "pattern" regexps; a nil entry marks a
// pattern Go's RE2 cannot compile (skipped during validation).
var patternCache sync.Map // string -> *regexp.Regexp

func compiledPattern(pattern string) *regexp.Regexp {
	if v, ok := patternCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	patternCache.Store(pattern, re)
	return re
}

func pathOr(path string) string {
	if path == "" {
		return "root"
	}
	return path
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// Check reports whether value satisfies the schema.
func (s *Schema) Check(value any) bool {
	return len(s.validate(value, "")) == 0
}
