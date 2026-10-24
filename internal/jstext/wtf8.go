package jstext

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// A JavaScript string is a sequence of UTF-16 code units, and may hold half a
// surrogate pair, which UTF-8 cannot encode. Where the port must hold one — a
// string pi builds piece by piece, cut between the halves of a pair, or a
// JSON \uD83D escape with no other half — it is held as its WTF-8 byte
// triple (0xED 0xA0..0xBF 0x80..0xBF), the encoding UTF-8 would give the code
// unit were it a character. These helpers read and write that form.

// WTF8Surrogate decodes the surrogate code unit a WTF-8 byte triple at the
// start of s holds.
func WTF8Surrogate(s string) (rune, bool) {
	if len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2] >= 0x80 && s[2] <= 0xBF {
		return 0xD000 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), true
	}
	return 0, false
}

// CodeUnit is String.fromCharCode(unit): a surrogate as its WTF-8 triple, any
// other code unit as UTF-8.
func CodeUnit(unit rune) string {
	if utf16.IsSurrogate(unit) {
		return string([]byte{0xED, byte(0xA0 | (unit>>6)&0x1F), byte(0x80 | unit&0x3F)})
	}
	return string(unit)
}

// CodeUnitLength is the JS length of a string that may hold WTF-8 surrogate
// triples: UTF-16 code units, a triple counting one. Any other byte that is
// not UTF-8 counts one, as the U+FFFD a decoder makes of it.
//
// A string read from bytes (a response body, a header) never means a
// surrogate by those bytes: there UTF16Length, which counts each such byte
// as the U+FFFD TextDecoder makes of it, is JS's length.
func CodeUnitLength(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			n++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			if _, ok := WTF8Surrogate(s[i:]); ok {
				size = 3
			}
			n++
		case r > 0xFFFF:
			n += 2
		default:
			n++
		}
		i += size
	}
	return n
}

// UTF16Length is JS String.prototype.length of a Go string read as UTF-8: an
// astral character is two code units, anything else (an invalid byte as the
// U+FFFD it decodes to) one.
func UTF16Length(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// JoinsSurrogates reports whether a + b joins half a surrogate pair at the
// end of a (WTF-8) with the other half at the start of b, and returns the
// character the two make.
func JoinsSurrogates(a, b string) (rune, bool) {
	if len(a) < 3 {
		return 0, false
	}
	hi, ok := WTF8Surrogate(a[len(a)-3:])
	if !ok || hi > 0xDBFF {
		return 0, false
	}
	lo, ok := WTF8Surrogate(b)
	if !ok || lo < 0xDC00 {
		return 0, false
	}
	return utf16.DecodeRune(hi, lo), true
}

// ConcatCodeUnits is JS `a + b`: when a ends with half a surrogate pair and b
// starts with the other half, the two make one character.
func ConcatCodeUnits(a, b string) string {
	if r, ok := JoinsSurrogates(a, b); ok {
		return a[:len(a)-3] + string(r) + b[3:]
	}
	return a + b
}

// ToWellFormedWTF8 is the string JSON.parse makes of raw string bytes: UTF-8
// as is, a WTF-8 surrogate triple kept (a high one followed by a low one
// joined into the character they make), and any other byte that is not UTF-8
// as U+FFFD.
func ToWellFormedWTF8(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] < utf8.RuneSelf {
			out = append(out, b[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size != 1 {
			out = append(out, b[i:i+size]...)
			i += size
			continue
		}
		unit, ok := WTF8Surrogate(string(b[i:min(i+3, len(b))]))
		if !ok {
			out = utf8.AppendRune(out, utf8.RuneError)
			i++
			continue
		}
		if unit <= 0xDBFF {
			if lo, ok := WTF8Surrogate(string(b[i+3 : min(i+6, len(b))])); ok && lo >= 0xDC00 {
				out = utf8.AppendRune(out, utf16.DecodeRune(unit, lo))
				i += 6
				continue
			}
		}
		out = append(out, b[i:i+3]...)
		i += 3
	}
	return string(out)
}

// EscapeControlChar is how JSON.stringify writes a control character
// (U+0000..U+001F) inside a string.
func EscapeControlChar(ch byte) string {
	switch ch {
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	default:
		return fmt.Sprintf(`\u%04x`, ch)
	}
}
