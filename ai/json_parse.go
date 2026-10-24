package ai

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// validJSONEscapes are the escape characters RepairJSON keeps as they are. A
// `\u` is kept only when four hex digits follow; otherwise its backslash is
// doubled like any other invalid escape's.
var validJSONEscapes = map[byte]bool{
	'"': true, '\\': true, '/': true, 'b': true, 'f': true,
	'n': true, 'r': true, 't': true,
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// RepairJSON escapes raw control characters inside strings and doubles
// backslashes before invalid escape characters (pi's repairJson).
func RepairJSON(s string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inString {
			b.WriteByte(ch)
			if ch == '"' {
				inString = true
			}
			continue
		}
		if ch == '"' {
			b.WriteByte(ch)
			inString = false
			continue
		}
		if ch == '\\' {
			if i+1 >= len(s) {
				b.WriteString("\\\\")
				continue
			}
			next := s[i+1]
			if next == 'u' && i+6 <= len(s) {
				hex := s[i+2 : i+6]
				if len(hex) == 4 && isHexDigit(hex[0]) && isHexDigit(hex[1]) && isHexDigit(hex[2]) && isHexDigit(hex[3]) {
					b.WriteString("\\u")
					b.WriteString(hex)
					i += 5
					continue
				}
			}
			if validJSONEscapes[next] {
				b.WriteByte('\\')
				b.WriteByte(next)
				i++
				continue
			}
			b.WriteString("\\\\")
			continue
		}
		if ch <= 0x1f {
			b.WriteString(jstext.EscapeControlChar(ch))
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// ParseToolCallArguments is pi's parseToolCallArguments: the arguments of a
// FINISHED tool call, read from the model's complete arguments text. It never
// keeps a partially parsed prefix: the text is JSON.parse'd, then its repair
// (RepairJSON) when that differs, and kept only when it is an object.
// Otherwise the arguments are {} and the third return — the call's
// ArgumentsError — is JSON.parse's SyntaxError message (of the repaired text
// when the repair changed it, as parseJsonWithRepair throws), or "Tool
// arguments must be a JSON object", followed by the text the model sent. A
// text that is blank by JS trim is {} with no error. The second return is the
// object in the order pi's JS object lists its keys (ToolCall.ArgumentsOrder);
// a number that is not finite (past float64's range) is held as nil, as
// JSON.stringify writes pi's value, and half a surrogate pair is kept as its
// WTF-8 triple, as JSON.parse keeps the code unit (see
// parseOrderedKeepingSurrogates; OrderedObject writes it back as JS does).
func ParseToolCallArguments(text string) (map[string]any, OrderedObject, string) {
	if jstext.Trim(text) == "" {
		return map[string]any{}, nil, ""
	}
	value, err := parseJSONValueWithRepair(text)
	if err == nil {
		if obj, isObject := value.(OrderedObject); isObject {
			nullNonFinite(obj)
			return obj.Plain(), obj, ""
		}
		err = errors.New("Tool arguments must be a JSON object")
	}
	return map[string]any{}, nil, err.Error() + "\n\nReceived arguments:\n" + text
}

// parseJSONValueWithRepair is pi's parseJsonWithRepair for any JSON value:
// JSON.parse of text, else of its repair when the repair changed it. The error
// is the SyntaxError JSON.parse throws for the last text it read.
func parseJSONValueWithRepair(text string) (any, error) {
	value, err := parseOrderedKeepingSurrogates([]byte(text))
	if err == nil {
		return value, nil
	}
	parsed := text
	if repaired := RepairJSON(text); repaired != text {
		if value, err = parseOrderedKeepingSurrogates([]byte(repaired)); err == nil {
			return value, nil
		}
		parsed = repaired
	}
	if syntaxErr := jstext.JSONSyntaxError(parsed); syntaxErr != nil {
		return nil, syntaxErr
	}
	// JSON.parse accepts what the decoder refused: a port bug, not the model's.
	return nil, fmt.Errorf("%v (JSON.parse accepts this text; report it as a port bug)", err)
}

// nullNonFinite replaces every number in v that is not finite with nil, in
// place, at any depth, and returns v. The trees it is given are freshly
// parsed and so shared with nothing.
func nullNonFinite(v any) any {
	switch t := v.(type) {
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return nil
		}
	case []any:
		for i := range t {
			t[i] = nullNonFinite(t[i])
		}
	case OrderedObject:
		for i := range t {
			t[i].Value = nullNonFinite(t[i].Value)
		}
	}
	return v
}
