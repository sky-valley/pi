package daemon

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// The daemon's JSON follows serde_json, which the Rust daemon is built on, so
// that replies are byte-identical: objects are written with their keys sorted
// (serde_json's Value map is a BTreeMap), strings escape only '"', '\\' and
// control characters (\b \t \n \f \r by name, others as lowercase \u00xx),
// and requests parse as serde_json parses them. Numbers stay json.Number so
// asU64 can refuse what serde_json's as_u64 refuses.

// object is a JSON object under construction or parsed from a request.
type object = map[string]any

// byteArray is written as a JSON array of numbers, as serde_json writes a
// Vec<u8>.
type byteArray []byte

// marshal writes v as serde_json writes the equivalent Value.
func marshal(v any) []byte {
	var b bytes.Buffer
	writeJSON(&b, v)
	return b.Bytes()
}

func writeJSON(b *bytes.Buffer, v any) {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, v)
	case int:
		b.WriteString(strconv.FormatInt(int64(v), 10))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(v), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(v, 10))
	case json.Number:
		b.WriteString(string(v))
	case byteArray:
		b.WriteByte('[')
		for i, c := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(int(c)))
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, s := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, item)
		}
		b.WriteByte(']')
	case object:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, key)
			b.WriteByte(':')
			writeJSON(b, v[key])
		}
		b.WriteByte('}')
	default:
		panic("daemon: no JSON form for a value of this type")
	}
}

const hexDigits = "0123456789abcdef"

// writeString is serde_json's format_escaped_str. Strings reaching it are
// valid UTF-8: names and texts are decoded lossily before they get here.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var escape string
		switch {
		case c == '"':
			escape = `\"`
		case c == '\\':
			escape = `\\`
		case c == '\b':
			escape = `\b`
		case c == '\t':
			escape = `\t`
		case c == '\n':
			escape = `\n`
		case c == '\f':
			escape = `\f`
		case c == '\r':
			escape = `\r`
		case c < 0x20:
			escape = `\u00` + string(hexDigits[c>>4]) + string(hexDigits[c&0xf])
		default:
			continue
		}
		b.WriteString(s[start:i])
		b.WriteString(escape)
		start = i + 1
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

// maxDepth is serde_json's recursion limit: a value nested this deep fails.
const maxDepth = 128

// parseJSON parses a request's JSON as serde_json::from_slice into a Value
// does, returning nil for what serde_json rejects (and for null, which the
// daemon refuses the same way). Unlike encoding/json, serde_json refuses
// invalid UTF-8, unpaired surrogate escapes, numbers out of the f64 range and
// nesting beyond its recursion limit; those are checked first.
func parseJSON(data []byte) any {
	if !utf8.Valid(data) || !pairedSurrogates(data) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var v any
	if decoder.Decode(&v) != nil {
		return nil
	}
	// Only whitespace may follow the value.
	for _, c := range data[decoder.InputOffset():] {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return nil
		}
	}
	if !withinLimits(v, 1) {
		return nil
	}
	return v
}

// withinLimits reports whether v nests less than maxDepth deep and every
// number in it is finite as an f64.
func withinLimits(v any, depth int) bool {
	switch v := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		return err == nil || !math.IsInf(f, 0)
	case []any:
		if depth >= maxDepth {
			return false
		}
		for _, item := range v {
			if !withinLimits(item, depth+1) {
				return false
			}
		}
	case object:
		if depth >= maxDepth {
			return false
		}
		for _, item := range v {
			if !withinLimits(item, depth+1) {
				return false
			}
		}
	}
	return true
}

// pairedSurrogates reports whether every \u escape of a surrogate in the JSON
// text's strings is a leading one directly followed by a trailing one, which
// serde_json requires of strings (encoding/json replaces the others).
func pairedSurrogates(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(data) {
				return true // malformed; the decoder rejects it
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			unit, ok := hexEscape(data, i)
			if !ok {
				return true
			}
			i += 5
			switch {
			case unit >= 0xDC00 && unit <= 0xDFFF:
				return false
			case unit >= 0xD800 && unit <= 0xDBFF:
				next, ok := hexEscape(data, i+1)
				if !ok || next < 0xDC00 || next > 0xDFFF {
					return false
				}
				i += 6
			}
		}
	}
	return true
}

// hexEscape reads the \uXXXX escape starting at data[i].
func hexEscape(data []byte, i int) (uint16, bool) {
	if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
		return 0, false
	}
	n, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
	return uint16(n), err == nil
}

// get is serde_json's value[key]: the field of an object, nil otherwise.
func get(v any, key string) any {
	o, _ := v.(object)
	return o[key]
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// asU64 is serde_json's as_u64: a number written as a non-negative integer
// that fits in 64 bits; not 1.0, 1e3 or -0.
func asU64(v any) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	u, err := strconv.ParseUint(string(n), 10, 64)
	return u, err == nil
}

// asF64 is serde_json's as_f64: any number.
func asF64(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil || !math.IsInf(f, 0)
}

func asArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

func asObject(v any) (object, bool) {
	o, ok := v.(object)
	return o, ok
}

// strings is an array of strings, or false when v is not one.
func asStrings(v any) ([]string, bool) {
	items, ok := asArray(v)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := asString(item)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
