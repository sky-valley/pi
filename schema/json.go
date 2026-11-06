package schema

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// stringify is JSON.stringify for the value model, the text upstream's
// messages embed (`expected ${JSON.stringify(node.const)}`): members in
// own-key order, numbers as JavaScript writes them, a non-finite number as
// null, half a surrogate pair as its \u escape, bytes as the object of index
// keys a Uint8Array is. It writes what jstext.Stringify cannot: an Object's
// order and WTF-8 surrogates. A bigint fails (panics with) V8's TypeError,
// which JSON.stringify throws: the compile, errors() and parse() that embed
// one (a bigint `const`) throw it upstream.
func stringify(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

// errBigIntJSON is V8's TypeError of JSON.stringify of a bigint.
var errBigIntJSON = errors.New("Do not know how to serialize a BigInt")

func writeJSON(b *strings.Builder, v any) {
	if f, ok := numberOf(v); ok {
		if isFinite(f) {
			b.WriteString(jstext.NumberToString(f))
		} else {
			b.WriteString("null")
		}
		return
	}
	if _, ok := bigintOf(v); ok {
		fail(errBigIntJSON)
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeJSONString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if _, ok := e.(undefinedValue); ok {
				e = nil // JSON.stringify writes an undefined item as null
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case []byte:
		writeJSON(b, bytesObject(x))
	case Object, map[string]any, *schemaObject:
		b.WriteByte('{')
		first := true
		for _, k := range ownKeys(x) {
			e, _ := getOwn(x, k)
			if _, ok := e.(undefinedValue); ok {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			writeJSONString(b, k)
			b.WriteByte(':')
			writeJSON(b, e)
		}
		b.WriteByte('}')
	default:
		// Not a value of the model; json.Marshal is the closest reading.
		// Every value of the model is written above, so Marshal only meets
		// what a caller put outside it (a func, a channel, a cyclic
		// pointer); JSON.stringify writes such a value (a function, a
		// symbol) as nothing, and null keeps the text JSON.
		out, err := json.Marshal(v)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(out)
	}
}

// writeJSONString quotes s as JSON.stringify does (QuoteJSONString): the two
// short escapes of '"' and '\\', \b \f \n \r \t, \u00XX for the other control
// characters, \uDXXX for half a surrogate pair, every other character as
// itself. A byte that is not UTF-8 or WTF-8 is U+FFFD.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					fmt.Fprintf(b, `\u%04x`, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		if unit, ok := jstext.WTF8Surrogate(s[i:]); ok {
			fmt.Fprintf(b, `\u%04x`, unit)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
	}
	b.WriteByte('"')
}

// maxDecodeDepth is how deeply Decode lets arrays and objects nest.
const maxDecodeDepth = 10000

// Decode reads JSON text into the value model as JSON.parse reads it: every
// object an Object, listing its keys in JavaScript own-key order (array-index
// keys ascending, then the others in the order they first appear; a repeated
// key keeps its first position and takes its last value), arrays as []any,
// numbers as float64 (±Inf past float64's range, as JSON.parse reads 1e400),
// and half a surrogate pair, escaped or not, kept as its WTF-8 triple. A
// syntax error is reported with the text of V8's SyntaxError where
// jstext.JSONSyntaxError knows it.
//
// It is how a caller compiles a schema from JSON text with the key order
// upstream would see.
func Decode(data []byte) (any, error) {
	p := decoder{data: data}
	p.skipSpace()
	v, err := p.value(0)
	if err == nil {
		p.skipSpace()
		if p.pos < len(data) {
			err = p.fail("unexpected content after the JSON value")
		}
	}
	if err != nil {
		if syntax := jstext.JSONSyntaxError(string(data)); syntax != nil {
			return nil, syntax
		}
		return nil, err
	}
	return v, nil
}

type decoder struct {
	data []byte
	pos  int
}

var errDecodeEnd = errors.New("unexpected end of JSON input")

func (p *decoder) fail(what string) error {
	if p.pos >= len(p.data) {
		return errDecodeEnd
	}
	return fmt.Errorf("invalid JSON at byte %d (%q): %s", p.pos, p.data[p.pos], what)
}

func (p *decoder) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *decoder) value(depth int) (any, error) {
	if p.pos >= len(p.data) {
		return nil, errDecodeEnd
	}
	switch c := p.data[p.pos]; {
	case c == '{' || c == '[':
		if depth >= maxDecodeDepth {
			return nil, p.fail(fmt.Sprintf("nested deeper than %d levels", maxDecodeDepth))
		}
		if c == '{' {
			return p.object(depth + 1)
		}
		return p.array(depth + 1)
	case c == '"':
		return p.string()
	case c == '-' || ('0' <= c && c <= '9'):
		return p.number()
	case c == 't':
		return p.literal("true", true)
	case c == 'f':
		return p.literal("false", false)
	case c == 'n':
		return p.literal("null", nil)
	}
	return nil, p.fail("expected a JSON value")
}

func (p *decoder) literal(word string, value any) (any, error) {
	if !bytes.HasPrefix(p.data[p.pos:], []byte(word)) {
		return nil, p.fail("expected " + word)
	}
	p.pos += len(word)
	return value, nil
}

func (p *decoder) object(depth int) (any, error) {
	p.pos++ // '{'
	obj := Object{}
	var at map[string]int // positions by key, once a linear search would cost
	hasIndexKey := false
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.fail("expected a double-quoted property name")
		}
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.fail("expected ':' after a property name")
		}
		p.pos++
		p.skipSpace()
		value, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		i := -1
		if at != nil {
			if j, ok := at[key]; ok {
				i = j
			}
		} else {
			i = slices.IndexFunc(obj, func(m Member) bool { return m.Key == key })
		}
		if i >= 0 {
			obj[i].Value = value
		} else {
			obj = append(obj, Member{key, value})
			if _, isIndex := jstext.ArrayIndexKey(key); isIndex {
				hasIndexKey = true
			}
			switch {
			case at != nil:
				at[key] = len(obj) - 1
			case len(obj) >= 16:
				at = make(map[string]int, 2*len(obj))
				for j, m := range obj {
					at[m.Key] = j
				}
			}
		}
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, errDecodeEnd
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			if hasIndexKey {
				slices.SortStableFunc(obj, func(a, b Member) int {
					ai, aIndex := jstext.ArrayIndexKey(a.Key)
					bi, bIndex := jstext.ArrayIndexKey(b.Key)
					switch {
					case aIndex && bIndex:
						return cmp.Compare(ai, bi)
					case aIndex:
						return -1
					case bIndex:
						return 1
					}
					return 0
				})
			}
			return obj, nil
		default:
			return nil, p.fail("expected ',' or '}' after a property value")
		}
	}
}

func (p *decoder) array(depth int) (any, error) {
	p.pos++ // '['
	arr := []any{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		p.skipSpace()
		value, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		arr = append(arr, value)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, errDecodeEnd
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr, nil
		default:
			return nil, p.fail("expected ',' or ']' after an array element")
		}
	}
}

// string reads a string literal: escapes decoded, a surrogate pair (escaped
// or as WTF-8 triples) joined, half a pair kept as its WTF-8 triple.
func (p *decoder) string() (string, error) {
	p.pos++ // '"'
	var out []byte
	start := p.pos
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			out = append(out, p.data[start:p.pos]...)
			p.pos++
			if utf8.Valid(out) {
				return string(out), nil
			}
			return jstext.ToWellFormedWTF8(out), nil
		case c < 0x20:
			return "", p.fail("bad control character in string literal")
		case c != '\\':
			p.pos++
			continue
		}
		out = append(out, p.data[start:p.pos]...)
		if p.pos+1 >= len(p.data) {
			return "", errDecodeEnd
		}
		switch e := p.data[p.pos+1]; e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := p.hex4(p.pos + 2)
			if !ok {
				p.pos++
				return "", p.fail("bad Unicode escape")
			}
			p.pos += 6
			if utf16.IsSurrogate(r) {
				// Kept as WTF-8; ToWellFormedWTF8 joins it with the other
				// half of a pair, escaped or not.
				out = append(out, jstext.CodeUnit(r)...)
			} else {
				out = utf8.AppendRune(out, r)
			}
			start = p.pos
			continue
		default:
			p.pos++
			return "", p.fail("bad escaped character")
		}
		p.pos += 2
		start = p.pos
	}
	return "", errDecodeEnd
}

func (p *decoder) hex4(i int) (rune, bool) {
	if i+4 > len(p.data) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(p.data[i:i+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(n), true
}

// number reads a number as JSON.parse does: the nearest float64, ±Inf past
// float64's range.
func (p *decoder) number() (any, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	digits := func() int {
		n := 0
		for p.pos < len(p.data) && '0' <= p.data[p.pos] && p.data[p.pos] <= '9' {
			p.pos++
			n++
		}
		return n
	}
	switch {
	case p.pos < len(p.data) && p.data[p.pos] == '0':
		p.pos++
	case digits() == 0:
		return nil, p.fail("expected a digit")
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if digits() == 0 {
			return nil, p.fail("expected a digit after the decimal point")
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			return nil, p.fail("expected a digit in the exponent")
		}
	}
	f, err := strconv.ParseFloat(string(p.data[start:p.pos]), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, err
	}
	return f, nil
}
