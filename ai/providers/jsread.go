package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// Reading a parsed wire event the way pi's JavaScript reads it.
//
// pi's anthropic and pi-messages adapters read the properties of a JSON.parse
// value directly, so a field of an unexpected JSON type is just a value: it
// converts, compares unequal or reads as undefined, and never fails the event
// the way a typed encoding/json decode fails it. These helpers keep each
// member as its raw JSON text — nil when absent, which is JavaScript's
// undefined — and make the reads pi's code makes: property lookup by exact
// name (encoding/json matches struct fields case-insensitively), the TypeError
// V8 throws for a property of undefined or null, truthiness (rawTruthy),
// strict equality and String() conversion.
//
// A value pi stores as-is into a field Go types as string or int (an id, a
// stop reason, a token count) is kept when Go can hold it — a string, a finite
// number — and reads as absent otherwise.

// rawObject is a JSON object's members by exact name, each as its raw JSON
// text; a repeated name keeps its last value, as the object JSON.parse builds
// does.
type rawObject map[string]json.RawMessage

// decodeRawObject decodes data, a JSON object, as json.Unmarshal into a
// map[string]json.RawMessage does. Its error is the syntax error JSON.parse
// throws when data is not JSON at all.
//
// It sits on every event the anthropic and pi-messages streams read, so a
// valid object is split by splitRawObject rather than by encoding/json, whose
// decode allocates and copies each member: the members are subslices of data,
// which the caller must not modify.
func decodeRawObject(data []byte) (rawObject, error) {
	if json.Valid(data) && jsonValueKind(data) == '{' {
		return splitRawObject(data), nil
	}
	var o rawObject
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// splitRawObject splits valid, a valid JSON object, into its members: each
// key unquoted as encoding/json unquotes it, each value its exact text (a
// subslice of valid whose capacity ends with it, so an append cannot write
// into what follows), a repeated key keeping its last value.
func splitRawObject(valid []byte) rawObject {
	o := rawObject{}
	i := skipJSONSpace(valid, 0) + 1 // past '{'
	for {
		i = skipJSONSpace(valid, i)
		if valid[i] == '}' {
			return o
		}
		keyEnd := skipJSONValue(valid, i)
		key := valid[i:keyEnd]
		i = skipJSONSpace(valid, keyEnd) + 1 // past ':'
		i = skipJSONSpace(valid, i)
		end := skipJSONValue(valid, i)
		o[unquoteJSONKey(key)] = json.RawMessage(valid[i:end:end])
		i = skipJSONSpace(valid, end)
		if valid[i] == ',' {
			i++
		}
	}
}

// unquoteJSONKey is a valid JSON string's text: the bytes between its quotes
// when it has no escape and is valid UTF-8, else encoding/json's unquoting
// (which also makes each invalid UTF-8 byte U+FFFD).
func unquoteJSONKey(quoted []byte) string {
	inner := quoted[1 : len(quoted)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		return string(inner)
	}
	var s string
	_ = json.Unmarshal(quoted, &s) // quoted is a valid JSON string
	return s
}

// skipJSONSpace returns the index of the first byte at or after i that is not
// JSON whitespace.
func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONValue returns the index just past the valid JSON value that starts
// at i.
func skipJSONValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		for i++; ; i++ {
			switch b[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
	case '{', '[':
		depth := 0
		for ; ; i++ {
			switch b[i] {
			case '"':
				i = skipJSONValue(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
	default: // a number, true, false or null
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return i
	}
}

// rawRead returns the members of the value raw holds, for the property reads
// pi's code makes on it, the first of which reads key: like `raw.key`, it
// throws V8's TypeError when raw is undefined or null. Any other value that is
// not an object has none of the properties pi reads, so each read of the
// empty result is undefined.
func rawRead(raw json.RawMessage, key string) (rawObject, error) {
	if raw == nil {
		return nil, fmt.Errorf("Cannot read properties of undefined (reading '%s')", key)
	}
	if jsonValueKind(raw) == 'n' {
		return nil, fmt.Errorf("Cannot read properties of null (reading '%s')", key)
	}
	return rawOptional(raw), nil
}

// rawOptional is optional chaining, `raw?.key`: the members of an object, and
// none for any other value, undefined and null included.
func rawOptional(raw json.RawMessage) rawObject {
	if jsonValueKind(raw) != '{' {
		return nil
	}
	o, err := decodeRawObject(raw)
	if err != nil {
		return nil // unreachable: raw is a member of a decoded document
	}
	return o
}

// rawNullish reports whether raw is undefined or null: what `?? x` replaces
// and `!= null` rules out.
func rawNullish(raw json.RawMessage) bool {
	return raw == nil || jsonValueKind(raw) == 'n'
}

// rawStringBytes is rawString's text as bytes — for a string with no escape,
// the bytes between its quotes, with no copy — for a caller that only
// compares it: `switch string(b)` does not allocate.
func rawStringBytes(raw json.RawMessage) ([]byte, bool) {
	if jsonValueKind(raw) != '"' {
		return nil, false
	}
	if text := bytes.TrimSpace(raw); bytes.IndexByte(text, '\\') < 0 && utf8.Valid(text) {
		return text[1 : len(text)-1], true
	}
	s, ok := rawString(raw)
	return []byte(s), ok
}

// rawString is the string raw holds, when it holds one.
func rawString(raw json.RawMessage) (string, bool) {
	if jsonValueKind(raw) != '"' {
		return "", false
	}
	// raw is a decoded document's member, so a string with no escape is its
	// text between the quotes — as long as that is valid UTF-8, which the
	// decode below would otherwise repair.
	if text := bytes.TrimSpace(raw); bytes.IndexByte(text, '\\') < 0 && utf8.Valid(text) {
		return string(text[1 : len(text)-1]), true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// rawNumber is the number raw holds, when it holds one, as JSON.parse
// reads it: ±Inf past float64's range, 0 below its smallest subnormal.
func rawNumber(raw json.RawMessage) (float64, bool) {
	switch c := jsonValueKind(raw); {
	case c == '-', c >= '0' && c <= '9':
		return jstext.Number(json.Number(bytes.TrimSpace(raw))), true
	}
	return 0, false
}

// rawCount reads a count pi stores as-is into a field Go holds as an int: a
// number, truncated toward zero, so the 10.0 a non-JavaScript server writes
// reads as 10. ok is false for anything an int cannot hold — a value that is
// not a number, an infinity, or a number past the int range.
func rawCount(raw json.RawMessage) (int, bool) {
	f, ok := rawNumber(raw)
	if !ok || !(-float64(math.MaxInt) < f && f < float64(math.MaxInt)) {
		return 0, false
	}
	return int(f), true
}

// rawToString is String(value), which `+` and a template literal apply: the
// text of a string, "undefined" for undefined, and jstext.ToString of any
// other value ("null", "5", "1,2", "[object Object]"). Its error is the
// TypeError V8 throws for an object with its own "toString" member, which
// JSON cannot make callable, leaving the object no string form.
func rawToString(raw json.RawMessage) (string, error) {
	if raw == nil {
		return "undefined", nil
	}
	if s, ok := rawString(raw); ok {
		return s, nil
	}
	v, err := jstext.Parse(raw)
	if err != nil {
		return "", err // unreachable: raw is a member of a decoded document
	}
	s, ok := jstext.ToString(v)
	if !ok {
		return "", errors.New("Cannot convert object to primitive value")
	}
	return s, nil
}

// rawStrictEqual is `a === b` for values from two separately parsed events:
// undefined and null each equal only themselves, booleans, numbers (by value,
// so 0 === -0 and 1 === 1.0) and strings (by content) compare, and an object
// or array equals nothing, as two parses never share one.
func rawStrictEqual(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ka, kb := jsonValueKind(a), jsonValueKind(b)
	// The same text is the same primitive (the common case, a block's index
	// against an event's), compared without decoding either.
	if ka != '{' && ka != '[' && bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b)) {
		return true
	}
	if x, ok := rawNumber(a); ok {
		y, ok := rawNumber(b)
		return ok && x == y
	}
	switch ka {
	case '"':
		x, _ := rawString(a)
		y, ok := rawString(b)
		return ok && x == y
	case 'n', 't', 'f':
		return ka == kb
	}
	return false
}
