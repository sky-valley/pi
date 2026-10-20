package jstext

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"strings"
)

// Stringify returns v's JSON as JSON.stringify writes it, for the places pi
// measures or embeds that text. Two encoding/json habits are undone:
//
//   - it writes <, >, &, U+2028 and U+2029 as \u003c, \u003e, \u0026, \u2028
//     and \u2029 where JSON.stringify writes the characters themselves.
//     SetEscapeHTML alone is not enough — a MarshalJSON method that calls
//     json.Marshal hands the encoder text that is already escaped — so the
//     escapes are written back as characters. JSON.stringify never emits any
//     of the five, so this cannot misfire;
//   - it writes negative zero as -0 where JSON.stringify writes 0;
//   - it refuses a non-finite number, which JSON.stringify writes as null, and
//     it writes a json.Number (Parse's numbers) as its literal text, where
//     JSON.stringify spells the number the literal denotes ("1.50" is 1.5,
//     "1e308" is 1e+308, "1e400" is Infinity). A float64 or json.Number
//     directly in v or inside its []any and map[string]any, and a float of
//     any type directly in v or inside a slice or array of any element type,
//     is written the JavaScript way: a float32 as the float64 it stands for
//     (a type's own MarshalJSON handles its own).
//
// What remains is encoding/json's: map keys come out sorted where JS keeps
// insertion order (the length is the same, the text is not), and a Go string
// cannot hold a lone UTF-16 surrogate, which JS would write as an escape.
func Stringify(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	v, _ = jsNumbers(v)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	if !strings.Contains(s, `\u`) && !strings.Contains(s, "-0") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inString && c == '\\' && i+1 < len(s):
			// Step over whole escapes, so an escaped backslash followed by
			// "u2028" stays two characters of text.
			if s[i+1] == 'u' && i+5 < len(s) {
				if r, ok := unescaped[s[i+2:i+6]]; ok {
					b.WriteRune(r)
					i += 5
					continue
				}
			}
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i++
		case c == '"':
			inString = !inString
			b.WriteByte(c)
		case !inString && c == '-' && isNegativeZero(s[i+1:]):
			// JSON.stringify(-0) is "0": drop the sign.
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// isNegativeZero reports whether rest, the text after a '-' outside a string,
// is the number 0 itself, not 0.5 or 0e-3.
func isNegativeZero(rest string) bool {
	if rest == "" || rest[0] != '0' {
		return false
	}
	return len(rest) == 1 || (rest[1] != '.' && rest[1] != 'e' && rest[1] != 'E')
}

// unescaped maps the \uXXXX escapes encoding/json writes and JSON.stringify
// does not to the characters JSON.stringify writes instead.
var unescaped = map[string]rune{"003c": '<', "003e": '>', "0026": '&', "2028": '\u2028', "2029": '\u2029'}

// jsNumbers returns v with every number in it — v itself, or an element of
// its []any and map[string]any at any depth — as JSON.stringify would see it:
// a json.Number as the float64 it denotes, and a non-finite number as nil.
// changed reports whether anything was replaced; only the containers on the
// way to a replacement are copied.
func jsNumbers(v any) (out any, changed bool) {
	switch t := v.(type) {
	case json.Number:
		if f := Number(t); !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f, true
		}
		return nil, true
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return nil, true
		}
	case []any:
		var copied []any
		for i, e := range t {
			f, ch := jsNumbers(e)
			if ch && copied == nil {
				copied = append([]any(nil), t...)
			}
			if copied != nil {
				copied[i] = f
			}
		}
		if copied != nil {
			return copied, true
		}
	case map[string]any:
		var copied map[string]any
		for k, e := range t {
			if f, ch := jsNumbers(e); ch {
				if copied == nil {
					copied = maps.Clone(t)
				}
				copied[k] = f
			}
		}
		if copied != nil {
			return copied, true
		}
	default:
		return reflectedNumbers(v)
	}
	return v, false
}

var jsonMarshaler = reflect.TypeFor[json.Marshaler]()

// reflectedNumbers is jsNumbers for the values its type switch does not
// name: a float of any other type (float32, a named float) and a slice or
// array of any element type. A non-finite float becomes nil and a float32
// the float64 it stands for, as JavaScript sees it; a slice or array is
// rebuilt as []any only when an element changed. A type with its own
// MarshalJSON is left to it.
func reflectedNumbers(v any) (out any, changed bool) {
	if v == nil {
		return v, false
	}
	rv := reflect.ValueOf(v)
	if rv.Type().Implements(jsonMarshaler) {
		return v, false
	}
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, true
		}
		if rv.Kind() == reflect.Float32 {
			return f, true
		}
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return v, false
		}
		var copied []any
		for i := range rv.Len() {
			f, ch := jsNumbers(rv.Index(i).Interface())
			if ch && copied == nil {
				copied = make([]any, rv.Len())
				for j := range i {
					copied[j] = rv.Index(j).Interface()
				}
			}
			if copied != nil {
				copied[i] = f
			}
		}
		if copied != nil {
			return copied, true
		}
	}
	return v, false
}
