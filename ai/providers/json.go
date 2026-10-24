package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// jsEscape renders s the way `JSON.stringify(s).slice(1, -1)` does in
// JavaScript: only `"`, `\` and control characters are escaped, and unpaired
// UTF-16 surrogates become \udXXX (ES2019 well-formed stringify). Go's
// encoding/json cannot be used here — it additionally escapes <, >, & and
// U+2028/U+2029.
//
// The lone-surrogate handling only reaches strings that did NOT arrive through
// encoding/json: Unmarshal replaces an unpaired surrogate escape with U+FFFD
// before this function ever sees it, and that loss is not recoverable here. It
// applies to values a caller built in Go (where a WTF-8 triple can survive), so
// the compensation is partial, not total.
func jsEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if hi, isSurrogate := jstext.WTF8Surrogate(s[i:]); isSurrogate {
			// A well-formed pair is a single astral character in JS and is
			// emitted literally; a lone surrogate is escaped.
			if hi <= 0xDBFF {
				if lo, ok := jstext.WTF8Surrogate(s[i+3:]); ok && lo >= 0xDC00 {
					b.WriteRune(utf16.DecodeRune(hi, lo))
					i += 6
					continue
				}
			}
			fmt.Fprintf(&b, "\\u%04x", hi)
			i += 3
			continue
		}
		switch ch := s[i]; {
		case ch == '"':
			b.WriteString(`\"`)
		case ch == '\\':
			b.WriteString(`\\`)
		case ch <= 0x1f:
			b.WriteString(jstext.EscapeControlChar(ch))
		default:
			b.WriteByte(ch)
		}
		i++
	}
	return b.String()
}

// jsQuote renders s the way `JSON.stringify(s)` does, quotes included.
func jsQuote(s string) string { return `"` + jsEscape(s) + `"` }

// jsStringify renders a JSON document the way JavaScript's
// `JSON.stringify(JSON.parse(raw))` does. Go's encoding/json cannot be used:
// marshalling a decoded map sorts its keys, whereas JS reproduces the parsed
// object's own-property order, and Go escapes strings differently (see
// jsEscape). Returns ok=false when raw is not a single well-formed JSON value.
//
// One JS behavior survives only partially: an unpaired UTF-16 surrogate escape
// is replaced with U+FFFD by encoding/json before jsEscape can see it, where JS
// keeps the code unit and re-emits `\udXXX`. Not recoverable at this layer —
// see jsEscape.
func jsStringify(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := jsStringifyValue(dec, &b); err != nil {
		return "", false
	}
	// Reject trailing content, which JSON.parse would also reject.
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	return b.String(), true
}

func jsStringifyValue(dec *json.Decoder, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return jsStringifyToken(dec, b, tok)
}

func jsStringifyToken(dec *json.Decoder, b *strings.Builder, tok json.Token) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			if err := jsStringifyObjectBody(dec, b); err != nil {
				return err
			}
		case '[':
			b.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := jsStringifyValue(dec, b); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		default:
			return fmt.Errorf("unexpected delimiter %q", t)
		}
		// Consume the matching closing delimiter.
		if _, err := dec.Token(); err != nil {
			return err
		}
	case string:
		b.WriteString(jsQuote(t))
	case json.Number:
		b.WriteString(jsNumber(t.String()))
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("unexpected token %T", tok)
	}
	return nil
}

// jsStringifyObjectBody renders the members of an object whose `{` has already
// been read, reproducing the two things JSON.parse does to an object literal
// that a streaming re-emit does not.
//
//   - A repeated key is ONE property: JSON.parse creates it at its first
//     occurrence and later occurrences only overwrite the value, so the last
//     value is kept in the first position.
//   - Own-property order is not insertion order. OrdinaryOwnPropertyKeys lists
//     array-index keys first, ascending numerically, and only then the rest in
//     creation order — so `{"type":"x","0":"y"}` re-serializes as
//     `{"0":"y","type":"x"}`.
//
// Members are rendered into a buffer before being emitted because both rules
// can move a member that has already been read.
func jsStringifyObjectBody(dec *json.Decoder, b *strings.Builder) error {
	type member struct {
		key     string
		index   uint32 // array-index value of key; meaningful when isIndex
		value   string
		isIndex bool
	}
	var members []member
	positions := map[string]int{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		var value strings.Builder
		if err := jsStringifyValue(dec, &value); err != nil {
			return err
		}
		if at, seen := positions[name]; seen {
			members[at].value = value.String()
			continue
		}
		positions[name] = len(members)
		index, isIndex := jstext.ArrayIndexKey(name)
		members = append(members, member{key: name, index: index, value: value.String(), isIndex: isIndex})
	}

	order := make([]int, 0, len(members))
	for i, m := range members {
		if m.isIndex {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(x, y int) bool { return members[order[x]].index < members[order[y]].index })
	for i, m := range members {
		if !m.isIndex {
			order = append(order, i)
		}
	}

	b.WriteByte('{')
	for i, at := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsQuote(members[at].key))
		b.WriteByte(':')
		b.WriteString(members[at].value)
	}
	b.WriteByte('}')
	return nil
}

// jsNumber renders a JSON number the way JavaScript's Number-to-String
// conversion does, so `1.0` becomes "1" and `1e2` becomes "100". JS switches to
// exponential notation only outside [1e-7, 1e21); Go's %g switches on a
// different threshold, so the ranges are selected explicitly. Values that do not
// parse as a float at all are passed through unchanged.
func jsNumber(src string) string {
	f, err := strconv.ParseFloat(src, 64)
	if err != nil {
		// Out of float64 range is not a syntax error: JSON.parse yields
		// ±Infinity (or a signed zero), and JSON.stringify writes Infinity as
		// `null`. ParseFloat reports the same saturated value alongside
		// ErrRange, so only a genuinely malformed literal passes through.
		if !errors.Is(err, strconv.ErrRange) {
			return src
		}
		if math.IsInf(f, 0) {
			return "null"
		}
	}
	// JS String(-0) is "0", where Go's FormatFloat keeps the sign.
	if f == 0 {
		return "0"
	}
	// JS uses fixed notation on [1e-6, 1e21) and exponential outside it:
	// String(1e-6) is "0.000001" but String(9.9e-7) is "9.9e-7".
	if abs := math.Abs(f); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	// Go pads the exponent to two digits ("1e-08"); JS does not ("1e-8").
	out := strconv.FormatFloat(f, 'e', -1, 64)
	if i := strings.IndexByte(out, 'e'); i >= 0 {
		mant, exp := out[:i], out[i+1:]
		sign := ""
		if len(exp) > 0 && (exp[0] == '+' || exp[0] == '-') {
			sign, exp = string(exp[0]), exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		out = mant + "e" + sign + exp
	}
	return out
}

// orEmptyArguments returns a tool call's arguments for a request body, in the
// order pi's JS object lists their keys (ai.ToolCall.ArgumentsOrder), standing in an empty object for absent arguments
// (pi's `toolCall.arguments ?? {}`).
func orEmptyArguments(tc ai.ToolCall) any {
	if args := tc.OrderedArguments(); args != nil {
		return args
	}
	return map[string]any{}
}

// parseJSONWithRepair parses JSON, retrying once with repairs (ai.RepairJSON)
// on failure.
func parseJSONWithRepair(s string, out any) error {
	if err := json.Unmarshal([]byte(s), out); err == nil {
		return nil
	}
	repaired := ai.RepairJSON(s)
	if repaired != s {
		return json.Unmarshal([]byte(repaired), out)
	}
	return json.Unmarshal([]byte(s), out) // return original error
}

// jsonValueKind reports what a valid JSON text holds, by its first
// non-whitespace byte: '{' object, '[' array, '"' string, 'n' null, 't'/'f'
// booleans, anything else a number.
func jsonValueKind[T ~string | ~[]byte](text T) byte {
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case ' ', '\t', '\n', '\r':
		default:
			return c
		}
	}
	return 0
}

// appendToolCallArguments is pi's `args.append(delta)` for a delta as an event
// holds it, a JSON value or undefined, and returns the call's partial
// arguments. A string is the delta. pi reads the length of any other value,
// which for undefined and null throws V8's TypeError. Any other value appends
// its String() to the text the call finishes with, as pi's `+=` does; pi's
// parser reads nothing of it (a number, boolean or object has no length),
// where the port's reads that text too, so only such a delta's partial
// arguments can differ from pi's, never the finished call's.
func appendToolCallArguments(args *ai.ToolCallArgumentsStream, delta json.RawMessage) (map[string]any, ai.OrderedObject, error) {
	if text, ok := rawString(delta); ok {
		partial, order := args.Append(text)
		return partial, order, nil
	}
	switch {
	case delta == nil:
		return nil, nil, errors.New("Cannot read properties of undefined (reading 'length')")
	case jsonValueKind(delta) == 'n':
		return nil, nil, errors.New("Cannot read properties of null (reading 'length')")
	}
	text, err := rawToString(delta)
	if err != nil {
		return nil, nil, err
	}
	partial, order := args.Append(text)
	return partial, order, nil
}

// sanitizeSurrogates removes unpaired UTF-16 surrogate code units (port of
// sanitizeSurrogates): pi replaces them with "" — they are DELETED, not
// substituted with U+FFFD. In Go strings, surrogate code units can only occur
// as WTF-8 byte triples (0xED 0xA0..0xBF 0x80..0xBF); a high+low pair is the
// JS-valid case and decodes to the astral character, while a lone surrogate
// is dropped. Valid UTF-8 (including real U+FFFD characters) passes through.
func sanitizeSurrogates(s string) string {
	if !containsWTF8Surrogate(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		hi, ok := jstext.WTF8Surrogate(s[i:])
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		if hi <= 0xDBFF { // high surrogate: pair it with a following low surrogate
			if lo, ok2 := jstext.WTF8Surrogate(s[i+3:]); ok2 && lo >= 0xDC00 {
				// Properly paired surrogates form a valid astral character in
				// JS and are preserved.
				b.WriteRune(utf16.DecodeRune(hi, lo))
				i += 6
				continue
			}
		}
		// Unpaired surrogate: deleted (pi replaces with "").
		i += 3
	}
	return b.String()
}

func containsWTF8Surrogate(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
			return true
		}
	}
	return false
}
