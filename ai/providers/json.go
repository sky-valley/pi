package providers

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

var validJSONEscapes = map[byte]bool{
	'"': true, '\\': true, '/': true, 'b': true, 'f': true,
	'n': true, 'r': true, 't': true, 'u': true,
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// repairJSON escapes raw control characters inside strings and doubles
// backslashes before invalid escape characters (port of repairJson).
func repairJSON(s string) string {
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
			b.WriteString(escapeControlChar(ch))
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

func escapeControlChar(ch byte) string {
	switch ch {
	case '\b':
		return "\\b"
	case '\f':
		return "\\f"
	case '\n':
		return "\\n"
	case '\r':
		return "\\r"
	case '\t':
		return "\\t"
	default:
		return fmt.Sprintf("\\u%04x", ch)
	}
}

// parseJSONWithRepair parses JSON, retrying once with repairs on failure.
func parseJSONWithRepair(s string, out any) error {
	if err := json.Unmarshal([]byte(s), out); err == nil {
		return nil
	}
	repaired := repairJSON(s)
	if repaired != s {
		return json.Unmarshal([]byte(repaired), out)
	}
	return json.Unmarshal([]byte(s), out) // return original error
}

// parseStreamingJSON parses potentially-incomplete JSON from streaming tool-call
// deltas, always returning a map (empty on total failure). Port of parseStreamingJson.
func parseStreamingJSON(partial string) map[string]any {
	if strings.TrimSpace(partial) == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := parseJSONWithRepair(partial, &out); err == nil && out != nil {
		return out
	}
	if completed, ok := completePartialJSON(partial); ok {
		var o map[string]any
		if err := json.Unmarshal([]byte(completed), &o); err == nil && o != nil {
			return o
		}
	}
	if completed, ok := completePartialJSON(repairJSON(partial)); ok {
		var o map[string]any
		if err := json.Unmarshal([]byte(completed), &o); err == nil && o != nil {
			return o
		}
	}
	return map[string]any{}
}

// completePartialJSON closes open strings, arrays, and objects in a truncated
// JSON document so it can parse. It approximates the partial-json library for
// the streaming tool-argument case.
func completePartialJSON(s string) (string, bool) {
	var stack []byte
	inString := false
	escaped := false
	// trailing tracks whether we're mid-token (after a colon/comma) where a value
	// is expected but missing.
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	b := strings.Builder{}
	b.WriteString(strings.TrimRight(s, " \t\r\n"))
	completed := b.String()

	// Drop a dangling token that can't be completed (trailing comma, colon, or a
	// key with no value). Strip trailing comma.
	completed = strings.TrimRight(completed, ",")

	if inString {
		completed += "\""
	}
	// Trim a dangling "key": with no value or trailing colon.
	completed = trimDanglingColon(completed)

	for i := len(stack) - 1; i >= 0; i-- {
		completed += string(stack[i])
	}
	if completed == "" {
		return "", false
	}
	return completed, true
}

func trimDanglingColon(s string) string {
	t := strings.TrimRight(s, " \t\r\n")
	if strings.HasSuffix(t, ":") {
		// remove "key": with the key, back to the previous { , or [
		idx := strings.LastIndexAny(t, "{[,")
		if idx >= 0 {
			return t[:idx+1]
		}
	}
	return s
}

// sanitizeSurrogates removes unpaired UTF-16 surrogate code units that would
// otherwise corrupt JSON encoding (port of sanitizeSurrogates).
func sanitizeSurrogates(s string) string {
	if !strings.ContainsRune(s, '�') && isValidUTF16(s) {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	for _, r := range runes {
		if utf16.IsSurrogate(r) {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isValidUTF16(s string) bool {
	for _, r := range s {
		if utf16.IsSurrogate(r) {
			return false
		}
	}
	return true
}
