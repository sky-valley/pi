// Adapted from TypeBox 1.3.27 (MIT, Copyright (c) 2017-2026 Haydn Paterson);
// see LICENSES/typebox-LICENSE.

package formats

import (
	"strings"
	"unicode"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// Hostname reports whether value is an RFC 1123 host name of LDH labels and
// valid "xn--" A-labels, at most 253 UTF-16 code units long with no trailing
// dot. Ports typebox/hostname.ts and typebox/idna/hostname.ts.
func Hostname(value string) bool {
	n := utf16Len(value)
	if n == 0 || n > 253 || value[len(value)-1] == '.' {
		return false
	}
	for label := range strings.SplitSeq(value, ".") {
		if !validLabelLength(label) || !(isPunyLabel(label) || isASCIILabel(label)) {
			return false
		}
	}
	return true
}

// IdnHostname reports whether value is an internationalized host name
// (RFC 5890-5893): after mapping fullwidth ASCII, NFC, dropping the code
// points IDNA ignores and mapping full-stop variants to ".", every label is
// a valid A-label or U-label, and in a name with right-to-left labels every
// label passes the bidi rule. Ports typebox/idn_hostname.ts and
// typebox/idna/idn-hostname.ts.
func IdnHostname(value string) bool {
	if value == "" || strings.Contains(value, " ") {
		return false
	}
	normalized := normalizeHostname(value)
	if utf16Len(normalized) > 253 {
		return false
	}
	labels := strings.Split(normalized, ".")
	bidi := false
	for _, label := range labels {
		if hasBidiChars(label) {
			bidi = true
			break
		}
	}
	for _, label := range labels {
		if !validLabelLength(label) || !(isPunyLabel(label) || isUnicodeLabel([]rune(label))) {
			return false
		}
		if bidi && !satisfiesBidiRule([]rune(label)) {
			return false
		}
	}
	return true
}

// utf16Len is a JavaScript string's length: UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func validLabelLength(label string) bool {
	n := utf16Len(label)
	return n > 0 && n <= 63
}

// normalizeHostname is idn-hostname.ts NormalizeHostname.
func normalizeHostname(value string) string {
	fullwidth := strings.Map(func(r rune) rune {
		if r >= 0xff01 && r <= 0xff5e {
			return r - 0xfee0
		}
		return r
	}, value)
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x00ad, r == 0x034f, r >= 0x180b && r <= 0x180d, r == 0x200b,
			r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef:
			return -1
		case r == 0x3002, r == 0xff0e, r == 0xff61:
			return '.'
		}
		return r
	}, norm.NFC.String(fullwidth))
}

// isASCIILabel is idna/label/ascii.ts IsAsciiLabel: letters, digits and
// hyphens, not starting or ending with a hyphen, without "--" in the third
// and fourth positions.
func isASCIILabel(label string) bool {
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	if label != "" && (label[0] == '-' || label[len(label)-1] == '-') {
		return false
	}
	return len(label) < 4 || label[2:4] != "--"
}

// isACEPrefixed is puny.ts IsAcePrefixed, value.toLowerCase().startsWith("xn--"):
// only ASCII code points lowercase to x, n or "-", so the first four bytes decide.
func isACEPrefixed(label string) bool {
	return len(label) >= 4 && label[0]|0x20 == 'x' && label[1]|0x20 == 'n' && label[2] == '-' && label[3] == '-'
}

// punyBody is value.slice(4).toLowerCase() for an ACE-prefixed label. The
// result only feeds the Punycode decoder, which rejects every non-ASCII code
// unit, so only the lowercasings that land in ASCII matter: A-Z and U+212A
// KELVIN SIGN, which JavaScript lowercases to "k". (U+0130's full lowercase
// keeps a combining dot, so it stays non-ASCII as well.)
func punyBody(label string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		case r == 0x212a:
			return 'k'
		}
		return r
	}, label[4:])
}

// isPunyLabel is idna/label/puny.ts IsPunyLabel.
func isPunyLabel(label string) bool {
	if !isACEPrefixed(label) {
		return false
	}
	body := punyBody(label)
	if strings.LastIndexByte(body, '-') == 0 {
		return false
	}
	decoded, ok := punyDecode(body)
	if !ok || !hasNonASCII(decoded) {
		return false
	}
	return isUnicodeLabel(decoded)
}

func hasNonASCII(cps []rune) bool {
	for _, r := range cps {
		if r >= 0x80 {
			return true
		}
	}
	return false
}

// typeboxBidiClass is idna/format/bidi.ts GetBidiClass, an approximation
// that only ever answers EN, AN, NSM, R, AL, L or ON.
func typeboxBidiClass(r rune) bidiClass {
	switch {
	case r >= '0' && r <= '9', r >= 0x06f0 && r <= 0x06f9:
		return bidiEN
	case r >= 0x0660 && r <= 0x0669:
		return bidiAN
	case unicode.Is(jsNonspacingMark, r):
		return bidiNSM
	case unicode.Is(jsHebrew, r):
		return bidiR
	case unicode.Is(jsArabicLetter, r):
		return bidiAL
	case unicode.Is(jsLetter, r):
		return bidiL
	}
	return bidiON
}

// hasBidiChars is bidi.ts HasBidiChars: whether the label (decoded, for an
// A-label) is right-to-left.
func hasBidiChars(label string) bool {
	if isACEPrefixed(label) {
		decoded, ok := punyDecode(punyBody(label))
		return ok && hasRightToLeft(decoded)
	}
	return hasRightToLeft([]rune(label))
}

// hasRightToLeft is bidi.ts HasRightToLeftCharacters (RFC 5893 §1.4).
func hasRightToLeft(cps []rune) bool {
	for _, r := range cps {
		switch typeboxBidiClass(r) {
		case bidiR, bidiAL, bidiAN:
			return true
		}
	}
	return false
}

// satisfiesBidiRule is bidi.ts SatisfiesBidiRule, which checks RFC 5893
// §2 rules 1, 2, 4 and 5 (not the end-of-label rules 3 and 6).
func satisfiesBidiRule(cps []rune) bool {
	if len(cps) == 0 {
		return true
	}
	first := typeboxBidiClass(cps[0])
	if first != bidiL && first != bidiR && first != bidiAL {
		return false
	}
	rtl := first != bidiL
	sawEN, sawAN := false, false
	for _, r := range cps {
		switch typeboxBidiClass(r) {
		case bidiEN:
			sawEN = true
		case bidiAN:
			if !rtl {
				return false
			}
			sawAN = true
		case bidiR, bidiAL:
			if !rtl {
				return false
			}
		case bidiL:
			if rtl {
				return false
			}
		}
	}
	return !(rtl && sawEN && sawAN)
}

// typeboxVirama is pattern.ts RE_VIRAMA.
var typeboxVirama = map[rune]bool{
	0x094d: true, 0x09cd: true, 0x0a4d: true, 0x0acd: true, 0x0b4d: true, 0x0bcd: true, 0x0c4d: true, 0x0ccd: true,
	0x0d3b: true, 0x0d3c: true, 0x0d4d: true, 0x0dca: true, 0x1b44: true, 0x1baa: true, 0x1bab: true, 0xa9c0: true,
	0x11046: true, 0x1107f: true, 0x110b9: true, 0x11133: true, 0x11134: true, 0x111c0: true, 0x11235: true,
	0x1134d: true, 0x11442: true, 0x114c2: true, 0x115bf: true, 0x1163f: true, 0x116b6: true, 0x11c3f: true,
	0x11d44: true, 0x11d45: true,
}

// isRFC5892Disallowed is pattern.ts RE_RFC5892_DISALLOWED.
func isRFC5892Disallowed(r rune) bool {
	switch r {
	case 0x0640, 0x07fa, 0x302e, 0x302f, 0x3031, 0x3032, 0x3033, 0x3034, 0x3035, 0x303b:
		return true
	}
	return false
}

// isPermitted is pattern.ts RE_PERMITTED_CATEGORY: letters, "-" "+" "." ","
// ":" "/", decimal digits, non-spacing and spacing marks, and the CONTEXTO
// and PVALID exceptions.
func isPermitted(r rune) bool {
	switch r {
	case '-', '+', '.', ',', ':', '/',
		0x00b7, 0x0375, 0x05f3, 0x05f4, 0x200c, 0x200d, 0x30fb, // CONTEXTO exceptions
		0x00df, 0x03c2, 0x06fd, 0x06fe, 0x0f0b, 0x3007: // PVALID exceptions
		return true
	}
	return unicode.Is(jsLetter, r) || unicode.Is(jsDecimalNumber, r) ||
		unicode.Is(jsNonspacingMark, r) || unicode.Is(jsSpacingMark, r)
}

func isCombiningMark(r rune) bool {
	return unicode.Is(jsNonspacingMark, r) || unicode.Is(jsSpacingMark, r) || unicode.Is(jsEnclosingMark, r)
}

// isUnicodeLabel is idna/label/unicode.ts IsUnicodeLabel over a label's code
// points as JavaScript iterates them (a lone surrogate is one code point).
func isUnicodeLabel(cps []rune) bool {
	if hasNonASCII(cps) && punyEncodedLen(cps)+4 > 63 {
		return false
	}
	if hasRightToLeft(cps) && !satisfiesBidiRule(cps) {
		return false
	}
	n := len(cps)
	if cps[0] == '-' || cps[n-1] == '-' || n >= 4 && cps[2] == '-' && cps[3] == '-' {
		return false
	}
	if isCombiningMark(cps[0]) {
		return false
	}
	japanese, katakanaMiddleDot := false, false
	for i, r := range cps {
		if isRFC5892Disallowed(r) || !isPermitted(r) {
			return false
		}
		if unicode.Is(jsJapanese, r) {
			japanese = true
		}
		// JavaScript reads past either end as undefined; a NUL neighbour is
		// just as falsy, so 0 stands for both.
		var prev, next rune
		if i > 0 {
			prev = cps[i-1]
		}
		if i+1 < n {
			next = cps[i+1]
		}
		switch r {
		case 0x00b7: // MIDDLE DOT, between two l (Catalan)
			if prev != 'l' || next != 'l' {
				return false
			}
		case 0x0375: // GREEK LOWER NUMERAL SIGN, before Greek
			if next == 0 || !unicode.Is(jsGreek, next) {
				return false
			}
		case 0x05f3, 0x05f4: // HEBREW GERESH and GERSHAYIM, after Hebrew
			if prev == 0 || !unicode.Is(jsHebrew, prev) {
				return false
			}
		case 0x200c: // ZWNJ: only ASCII before it is rejected, unless a virama
			if prev == 0 || prev < 0x80 && !typeboxVirama[prev] {
				return false
			}
		case 0x200d: // ZWJ, after a virama
			if prev == 0 || !typeboxVirama[prev] {
				return false
			}
		case 0x30fb:
			katakanaMiddleDot = true
		}
	}
	return !katakanaMiddleDot || japanese
}
