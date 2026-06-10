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
	stringStart := -1
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
			stringStart = i
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

	// partial-json trims the whole input first (jsonString.trim()), even when
	// it ends inside an open string.
	completed := strings.TrimRight(s, " \t\r\n")
	if inString && isDanglingObjectKey(s, stack, stringStart) {
		// An open string in object-KEY position can't be completed into a
		// member; partial-json drops the incomplete key entirely.
		completed = strings.TrimRight(s[:stringStart], " \t\r\n")
		completed = strings.TrimRight(completed, ",")
		completed = trimDanglingColon(completed)
	} else if inString {
		// A trailing comma inside an open string is string CONTENT, not a
		// dangling token — close the string without stripping it.
		completed += "\""
	} else {
		// Drop a dangling token that can't be completed (trailing comma, colon,
		// or a key with no value). Strip trailing comma.
		completed = strings.TrimRight(completed, ",")
		// Trim a dangling "key": with no value or trailing colon.
		completed = trimDanglingColon(completed)
	}

	for i := len(stack) - 1; i >= 0; i-- {
		completed += string(stack[i])
	}
	if completed == "" {
		return "", false
	}
	return completed, true
}

// isDanglingObjectKey reports whether the open string starting at stringStart
// sits in object-key position (directly after '{' or ',' inside an object).
func isDanglingObjectKey(s string, stack []byte, stringStart int) bool {
	if stringStart < 0 || len(stack) == 0 || stack[len(stack)-1] != '}' {
		return false
	}
	for j := stringStart - 1; j >= 0; j-- {
		switch s[j] {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', ',':
			return true
		default:
			return false
		}
	}
	return false
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
		hi, ok := decodeWTF8Surrogate(s[i:])
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		if hi <= 0xDBFF { // high surrogate: pair it with a following low surrogate
			if lo, ok2 := decodeWTF8Surrogate(s[i+3:]); ok2 && lo >= 0xDC00 {
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

// decodeWTF8Surrogate decodes a UTF-16 surrogate code unit encoded as a WTF-8
// byte triple at the start of s.
func decodeWTF8Surrogate(s string) (rune, bool) {
	if len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2] >= 0x80 && s[2] <= 0xBF {
		return 0xD000 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), true
	}
	return 0, false
}

func containsWTF8Surrogate(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
			return true
		}
	}
	return false
}
