package ai

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
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
	// Extra holds passthrough keywords not modeled above.
	Extra map[string]any
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
	return &Schema{Type: "array", Items: item, Description: first(desc)}
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

// MarshalJSON renders the schema as standard JSON Schema with deterministic
// key ordering.
func (s *Schema) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	write := func(key string, val any) error {
		raw, err := json.Marshal(val)
		if err != nil {
			return err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(raw)
		return nil
	}

	if s.Type != "" {
		if s.Nullable {
			if err := write("type", []string{s.Type, "null"}); err != nil {
				return nil, err
			}
		} else {
			if err := write("type", s.Type); err != nil {
				return nil, err
			}
		}
	}
	if s.Description != "" {
		if err := write("description", s.Description); err != nil {
			return nil, err
		}
	}
	if s.Type == "object" || len(s.Properties) > 0 {
		order := s.PropertyOrder
		if len(order) == 0 && len(s.Properties) > 0 {
			for k := range s.Properties {
				order = append(order, k)
			}
			sort.Strings(order)
		}
		props := json.RawMessage("{}")
		if len(order) > 0 {
			var pb strings.Builder
			pb.WriteByte('{')
			for i, name := range order {
				if i > 0 {
					pb.WriteByte(',')
				}
				k, _ := json.Marshal(name)
				pb.Write(k)
				pb.WriteByte(':')
				raw, err := json.Marshal(s.Properties[name])
				if err != nil {
					return nil, err
				}
				pb.Write(raw)
			}
			pb.WriteByte('}')
			props = json.RawMessage(pb.String())
		}
		if err := write("properties", props); err != nil {
			return nil, err
		}
		if len(s.Required) > 0 {
			if err := write("required", s.Required); err != nil {
				return nil, err
			}
		}
	}
	if s.Items != nil {
		if err := write("items", s.Items); err != nil {
			return nil, err
		}
	}
	if len(s.Enum) > 0 {
		if err := write("enum", s.Enum); err != nil {
			return nil, err
		}
	}
	if s.HasConst {
		if err := write("const", s.Const); err != nil {
			return nil, err
		}
	}
	if s.Default != nil {
		if err := write("default", s.Default); err != nil {
			return nil, err
		}
	}
	if s.AdditionalSchema != nil {
		if err := write("additionalProperties", s.AdditionalSchema); err != nil {
			return nil, err
		}
	} else if s.AdditionalAllowed != nil {
		if err := write("additionalProperties", *s.AdditionalAllowed); err != nil {
			return nil, err
		}
	}
	if s.Minimum != nil {
		if err := write("minimum", *s.Minimum); err != nil {
			return nil, err
		}
	}
	if s.Maximum != nil {
		if err := write("maximum", *s.Maximum); err != nil {
			return nil, err
		}
	}
	if s.ExclusiveMinimum != nil {
		if err := write("exclusiveMinimum", *s.ExclusiveMinimum); err != nil {
			return nil, err
		}
	}
	if s.ExclusiveMaximum != nil {
		if err := write("exclusiveMaximum", *s.ExclusiveMaximum); err != nil {
			return nil, err
		}
	}
	if s.MultipleOf != nil {
		if err := write("multipleOf", *s.MultipleOf); err != nil {
			return nil, err
		}
	}
	if s.MinLength != nil {
		if err := write("minLength", *s.MinLength); err != nil {
			return nil, err
		}
	}
	if s.MaxLength != nil {
		if err := write("maxLength", *s.MaxLength); err != nil {
			return nil, err
		}
	}
	if s.Pattern != "" {
		if err := write("pattern", s.Pattern); err != nil {
			return nil, err
		}
	}
	if s.MinItems != nil {
		if err := write("minItems", *s.MinItems); err != nil {
			return nil, err
		}
	}
	if s.MaxItems != nil {
		if err := write("maxItems", *s.MaxItems); err != nil {
			return nil, err
		}
	}
	if s.Format != "" {
		if err := write("format", s.Format); err != nil {
			return nil, err
		}
	}
	if len(s.AnyOf) > 0 {
		if err := write("anyOf", s.AnyOf); err != nil {
			return nil, err
		}
	}
	if len(s.OneOf) > 0 {
		if err := write("oneOf", s.OneOf); err != nil {
			return nil, err
		}
	}
	if len(s.AllOf) > 0 {
		if err := write("allOf", s.AllOf); err != nil {
			return nil, err
		}
	}
	for _, k := range sortedKeys(s.Extra) {
		if err := write(k, s.Extra[k]); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// UnmarshalJSON decodes a JSON Schema object into a Schema, preserving property
// order from the source bytes.
func (s *Schema) UnmarshalJSON(data []byte) error {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(data, &generic); err != nil {
		return err
	}
	s.Extra = map[string]any{}
	for key, raw := range generic {
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
			s.Minimum = unmarshalFloatPtr(raw)
		case "maximum":
			s.Maximum = unmarshalFloatPtr(raw)
		case "exclusiveMinimum":
			s.ExclusiveMinimum = unmarshalFloatPtr(raw)
		case "exclusiveMaximum":
			s.ExclusiveMaximum = unmarshalFloatPtr(raw)
		case "multipleOf":
			s.MultipleOf = unmarshalFloatPtr(raw)
		case "minLength":
			s.MinLength = unmarshalIntPtr(raw)
		case "maxLength":
			s.MaxLength = unmarshalIntPtr(raw)
		case "pattern":
			_ = json.Unmarshal(raw, &s.Pattern)
		case "minItems":
			s.MinItems = unmarshalIntPtr(raw)
		case "maxItems":
			s.MaxItems = unmarshalIntPtr(raw)
		case "const":
			_ = json.Unmarshal(raw, &s.Const)
			s.HasConst = true
		case "format":
			_ = json.Unmarshal(raw, &s.Format)
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

// isJSWhitespace reports whether r is whitespace for JS string trimming
// (ECMA-262 TrimString: WhiteSpace ∪ LineTerminator). Notably this includes
// FEFF (ZWNBSP) and all Zs, but NOT U+0085 NEL (which Go's unicode.IsSpace
// accepts).
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x2028, 0x2029, 0xFEFF:
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// trimJSWhitespace trims like String.prototype.trim / Number()'s implicit trim.
func trimJSWhitespace(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

// jsNumber mirrors ECMA-262 StringToNumber (the JS Number(string) coercion).
// ok=false means the result is NaN. Note ok=true may still yield ±Inf
// ("Infinity", "1e1000"); callers gate with Number.isFinite/isInteger
// semantics. Verified against node: " 12 "→12, "0x10"→16, "0b101"→5,
// "0o17"→15, "+5"→5, "1e3"→1000, ".5"→0.5, "5."→5, ""→0, "1_0"→NaN,
// "0x1p4"→NaN, "-0x10"→NaN, "12abc"→NaN, "NaN"→NaN.
func jsNumber(value string) (float64, bool) {
	s := trimJSWhitespace(value)
	if s == "" {
		return 0, true // Number("") === 0
	}

	// Non-decimal integer literals: 0x/0X, 0b/0B, 0o/0O. No sign allowed.
	if len(s) > 2 && s[0] == '0' {
		var base int
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			digits := s[2:]
			if !validDigits(digits, base) {
				return 0, false
			}
			// Arbitrary precision (JS allows >2^64), rounded to float64.
			n, ok := new(big.Int).SetString(digits, base)
			if !ok {
				return 0, false
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f, true
		}
	}

	sign := 1.0
	rest := s
	switch s[0] {
	case '+':
		rest = s[1:]
	case '-':
		sign, rest = -1, s[1:]
	}
	if rest == "Infinity" {
		return sign * math.Inf(1), true
	}
	// Validate StrUnsignedDecimalLiteral strictly before ParseFloat: Go's
	// ParseFloat accepts JS-invalid forms ("1_0", "0x1p4", "inf", "nan").
	if !isStrUnsignedDecimalLiteral(rest) {
		return 0, false
	}
	f, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		if ne, isNum := err.(*strconv.NumError); isNum && ne.Err == strconv.ErrRange {
			// Overflow → ±Inf (like JS "1e1000" → Infinity); underflow → ~0.
			return sign * f, true
		}
		return 0, false
	}
	return sign * f, true
}

func validDigits(s string, base int) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'z':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'Z':
			d = c - 'A' + 10
		default:
			return false
		}
		if int(d) >= base {
			return false
		}
	}
	return true
}

// isStrUnsignedDecimalLiteral validates ECMA-262 StrUnsignedDecimalLiteral:
// digits [. digits] [ExponentPart] | . digits [ExponentPart].
func isStrUnsignedDecimalLiteral(s string) bool {
	i := 0
	digits := func() int {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i - start
	}
	intLen := digits()
	fracLen := 0
	if i < len(s) && s[i] == '.' {
		i++
		fracLen = digits()
	}
	if intLen == 0 && fracLen == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(s)
}

// jsNumberToString formats a float64 the way JavaScript's String(number) /
// Number.prototype.toString does (pi validation.ts:135-136 uses String(value)).
//
// JS uses the shortest round-trippable decimal, switching to exponential
// notation only when the decimal exponent is >= 21 or <= -7. This differs from
// Go's FormatFloat('g'), which would render 1000000 as "1e+06" and 1e-7 as
// "1e-07"; JS renders them "1000000" and "1e-7".
func jsNumberToString(f float64) string {
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == 0 {
		// Negative zero stringifies to "0" in JS.
		return "0"
	}

	neg := math.Signbit(f)
	abs := math.Abs(f)

	// Shortest round-trippable significand and base-10 exponent.
	// 'e' format gives "d.dddde±dd"; parse mantissa digits + exponent k where
	// value = digits * 10^(k - (len(digits)-1)).
	mant := strconv.FormatFloat(abs, 'e', -1, 64)
	eIdx := strings.IndexByte(mant, 'e')
	digitsPart := mant[:eIdx]
	exp10, _ := strconv.Atoi(mant[eIdx+1:])

	// Strip the decimal point to get the bare significant digits.
	digits := strings.Replace(digitsPart, ".", "", 1)
	// n = exponent such that value = digits-as-integer-with-implied-point.
	// Per ECMA-262 Number::toString: let k = number of digits, and s = the
	// integer formed by the digits; n is the position of the decimal point.
	k := len(digits)
	n := exp10 + 1 // point sits after the first 'n' digits when n in (0,k]

	var out string
	switch {
	case k <= n && n <= 21:
		// Integer with trailing zeros: digits followed by (n-k) zeros.
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		// Decimal point inside the digit string.
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		// 0.00...digits
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		// Exponential notation. Mantissa is first digit, optional ".rest",
		// exponent is (n-1) with explicit sign and no leading zero padding.
		e := n - 1
		var sign string
		if e >= 0 {
			sign = "+"
		} else {
			sign = "-"
			e = -e
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(e)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(e)
		}
	}

	if neg {
		return "-" + out
	}
	return out
}

func coercePrimitiveByType(value any, typ string) any {
	switch typ {
	case "number":
		if value == nil {
			return float64(0)
		}
		// pi (validation.ts:88-92): Number(value) gated by Number.isFinite, with
		// a value.trim() !== "" guard ("" / whitespace would coerce to 0).
		if str, ok := value.(string); ok && trimJSWhitespace(str) != "" {
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
		if str, ok := value.(string); ok && trimJSWhitespace(str) != "" {
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
		switch v := value.(type) {
		case float64:
			return jsNumberToString(v)
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
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be >= " + jsNumberToString(*s.Minimum)})
			}
			if s.Maximum != nil && !(f <= *s.Maximum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be <= " + jsNumberToString(*s.Maximum)})
			}
			if s.ExclusiveMinimum != nil && !(f > *s.ExclusiveMinimum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be > " + jsNumberToString(*s.ExclusiveMinimum)})
			}
			if s.ExclusiveMaximum != nil && !(f < *s.ExclusiveMaximum) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be < " + jsNumberToString(*s.ExclusiveMaximum)})
			}
			if s.MultipleOf != nil && !isJSMultipleOf(f, *s.MultipleOf) {
				errs = append(errs, ValidationError{Path: pathOr(path), Message: "must be multiple of " + jsNumberToString(*s.MultipleOf)})
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
