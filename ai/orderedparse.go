package ai

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// maxOrderedDepth is how deeply parseOrdered lets arrays and objects nest,
// encoding/json's own limit. The parser takes a stack frame per level.
const maxOrderedDepth = 10000

// parseOrdered reads data, one complete JSON text, into what JSON.parse
// returns, in DecodeOrderedValue's shapes. It accepts what JSON.parse accepts
// (RFC 8259, with JSON's four whitespace characters around tokens), nested up
// to maxOrderedDepth levels, and decodes it in a single pass.
//
// Two things a Go string cannot hold are read as encoding/json reads them: a
// byte that is not UTF-8, and a \u escape that is half a surrogate pair, are
// each U+FFFD.
func parseOrdered(data []byte) (any, error) {
	return parseOrderedWith(orderedParser{data: data})
}

// parseOrderedKeepingSurrogates is parseOrdered, except that half a
// surrogate pair — a \uD83D escape whose other half is not the next escape,
// or a WTF-8 surrogate triple in the text — is kept as its WTF-8 triple, as
// JSON.parse keeps the code unit, and a high half followed by a low one, in
// any mix of escapes and triples, is the character they make. It is what a
// tool call's finished arguments are read with, so they hold what the
// streamed partial arguments held (StreamingJSONParser keeps the code unit
// too).
func parseOrderedKeepingSurrogates(data []byte) (any, error) {
	return parseOrderedWith(orderedParser{data: data, keepSurrogates: true})
}

func parseOrderedWith(p orderedParser) (any, error) {
	p.skipSpace()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos < len(p.data) {
		return nil, p.fail("unexpected content after the JSON value")
	}
	return v, nil
}

type orderedParser struct {
	data []byte
	pos  int
	// keepSurrogates keeps half a surrogate pair as WTF-8
	// (parseOrderedKeepingSurrogates).
	keepSurrogates bool
}

// stringOf is the string of a literal's decoded bytes: as is when they are
// UTF-8; otherwise each invalid byte is U+FFFD, as encoding/json reads it,
// except that keepSurrogates keeps WTF-8 surrogate triples, joining a pair.
func (p *orderedParser) stringOf(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if p.keepSurrogates {
		return jstext.ToWellFormedWTF8(b)
	}
	return string([]rune(string(b)))
}

// errOrderedEnd is the error for a text that ends before its value does.
var errOrderedEnd = errors.New("unexpected end of JSON input")

func (p *orderedParser) fail(what string) error {
	if p.pos >= len(p.data) {
		return errOrderedEnd
	}
	return fmt.Errorf("invalid JSON at byte %d (%q): %s", p.pos, p.data[p.pos], what)
}

func (p *orderedParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// value reads the value starting at p.pos, inside depth containers.
func (p *orderedParser) value(depth int) (any, error) {
	if p.pos >= len(p.data) {
		return nil, errOrderedEnd
	}
	switch c := p.data[p.pos]; {
	case c == '{' || c == '[':
		if depth >= maxOrderedDepth {
			return nil, p.fail(fmt.Sprintf("nested deeper than %d levels", maxOrderedDepth))
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

// literal reads word, which stands for value.
func (p *orderedParser) literal(word string, value any) (any, error) {
	if len(p.data)-p.pos < len(word) || string(p.data[p.pos:p.pos+len(word)]) != word {
		return nil, p.fail("expected " + word)
	}
	p.pos += len(word)
	return value, nil
}

// object reads an object, listing its keys in the order JSON.parse's object
// does (OrdinaryOwnPropertyKeys): keys that are array indices first,
// ascending, then the rest in the order they were first written. A repeated
// key keeps its first position and takes the last value.
func (p *orderedParser) object(depth int) (any, error) {
	p.pos++ // '{'
	obj := OrderedObject{}
	var at map[string]int // obj's positions by key, once linear search would cost
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
			i = obj.indexOf(key)
		}
		if i >= 0 {
			obj[i].Value = value
		} else {
			obj = append(obj, OrderedField{Key: key, Value: value})
			if _, isIndex := jstext.ArrayIndexKey(key); isIndex {
				hasIndexKey = true
			}
			switch {
			case at != nil:
				at[key] = len(obj) - 1
			case len(obj) >= 16:
				at = make(map[string]int, 2*len(obj))
				for j, f := range obj {
					at[f.Key] = j
				}
			}
		}
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, errOrderedEnd
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			if hasIndexKey {
				sort.SliceStable(obj, func(i, j int) bool {
					a, aIndex := jstext.ArrayIndexKey(obj[i].Key)
					b, bIndex := jstext.ArrayIndexKey(obj[j].Key)
					if aIndex && bIndex {
						return a < b
					}
					return aIndex && !bIndex
				})
			}
			return obj, nil
		default:
			return nil, p.fail("expected ',' or '}' after a property value")
		}
	}
}

func (o OrderedObject) indexOf(key string) int {
	for i, f := range o {
		if f.Key == key {
			return i
		}
	}
	return -1
}

func (p *orderedParser) array(depth int) (any, error) {
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
			return nil, errOrderedEnd
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

// string reads the string literal starting at p.pos.
func (p *orderedParser) string() (string, error) {
	p.pos++ // '"'
	start := p.pos
	ascii := true
	for p.pos < len(p.data) {
		switch c := p.data[p.pos]; {
		case c == '"':
			s := p.data[start:p.pos]
			p.pos++
			if ascii {
				return string(s), nil
			}
			return p.stringOf(s), nil
		case c == '\\':
			return p.escapedString(start)
		case c < 0x20:
			return "", p.fail("bad control character in string literal")
		case c >= utf8.RuneSelf:
			ascii = false
		}
		p.pos++
	}
	return "", errOrderedEnd
}

// escapedString finishes a string literal that began at start and holds an
// escape at p.pos, decoding its escapes as encoding/json does.
func (p *orderedParser) escapedString(start int) (string, error) {
	out := make([]byte, 0, p.pos-start+16)
	out = append(out, p.data[start:p.pos]...)
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return p.stringOf(out), nil
		case c < 0x20:
			return "", p.fail("bad control character in string literal")
		case c != '\\':
			out = append(out, c)
			p.pos++
			continue
		}
		if p.pos+1 >= len(p.data) {
			return "", errOrderedEnd
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
				if next, ok := p.hex4(p.pos + 2); ok && p.pos+1 < len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
					if pair := utf16.DecodeRune(r, next); pair != unicode.ReplacementChar {
						out = utf8.AppendRune(out, pair)
						p.pos += 6
						continue
					}
				}
				if p.keepSurrogates {
					// The triple joins a triple or escape of the other half
					// when stringOf reads the literal.
					out = append(out, jstext.CodeUnit(r)...)
					continue
				}
				r = unicode.ReplacementChar
			}
			out = utf8.AppendRune(out, r)
			continue
		default:
			p.pos++
			return "", p.fail("bad escaped character")
		}
		p.pos += 2
	}
	return "", errOrderedEnd
}

// hex4 reads the four hex digits at i as a code unit.
func (p *orderedParser) hex4(i int) (rune, bool) {
	if i+4 > len(p.data) {
		return 0, false
	}
	var r rune
	for _, c := range p.data[i : i+4] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return 0, false
		}
		r = r*16 + rune(c)
	}
	return r, true
}

// number reads the number starting at p.pos as JSON.parse does: the nearest
// float64, ±Inf past float64's range and zero below it.
func (p *orderedParser) number() (any, error) {
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
	integer := true
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		integer = false
		if digits() == 0 {
			return nil, p.fail("expected a digit after the decimal point")
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		integer = false
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			return nil, p.fail("expected a digit in the exponent")
		}
	}
	text := p.data[start:p.pos]
	if integer && len(text) <= 16 {
		// Up to 16 digits fit an int64 exactly, and converting that to a
		// float64 rounds it as ParseFloat would round the digits.
		negative := text[0] == '-'
		var n int64
		for _, c := range text[boolIndex(negative):] {
			n = n*10 + int64(c-'0')
		}
		f := float64(n)
		if negative {
			f = -f // -0 stays negative zero, as JSON.parse's does
		}
		return f, nil
	}
	f, err := strconv.ParseFloat(string(text), 64)
	var numErr *strconv.NumError
	if err != nil && !(errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange)) {
		return nil, fmt.Errorf("invalid JSON number %s: %w", text, err)
	}
	return f, nil // ±Inf on overflow, as JSON.parse reads it
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}
