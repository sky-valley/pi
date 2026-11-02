// Adapted from TypeBox 1.3.27 (MIT, Copyright (c) 2017-2026 Haydn Paterson);
// see LICENSES/typebox-LICENSE.

package formats

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// idnEmailLocal is the local part of typebox/idn_email.ts IdnEmail (/iu): a
// dot-atom that admits every non-ASCII code point, or a quoted string whose
// escapes take any code point but a line terminator (/u `.`).
var idnEmailLocal = regexp.MustCompile("^(?:[A-Za-z0-9!#$%&'*+/=?^_`{|}~\\x{80}-\\x{10FFFF}-]+(?:\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~\\x{80}-\\x{10FFFF}-]+)*" +
	`|"(?:[^"\\]|\\[^\n\r\x{2028}\x{2029}])*")$`)

// IdnEmail reports whether value, in NFC, is an internationalized email
// address (RFC 6531): a local part as above, "@", and dot-separated labels
// of letters, digits and hyphens of at most 63 code points that start with a
// letter or digit and do not end with a hyphen. Ports typebox/idn_email.ts.
func IdnEmail(value string) bool {
	value = norm.NFC.String(value)
	// The domain admits no "@", so the address splits at the last one.
	at := strings.LastIndexByte(value, '@')
	if at < 0 || !idnEmailLocal.MatchString(value[:at]) {
		return false
	}
	for label := range strings.SplitSeq(value[at+1:], ".") {
		if !idnEmailLabel(label) {
			return false
		}
	}
	return true
}

// idnEmailLabel is [\p{L}\p{N}](?:[\p{L}\p{N}-]{0,62})(?<!-) under /iu.
func idnEmailLabel(label string) bool {
	n := 0
	var last rune
	for _, r := range label {
		if n == 0 && !unicode.Is(jsIdnEmailDomain, r) || r != '-' && !unicode.Is(jsIdnEmailDomain, r) {
			return false
		}
		n++
		last = r
	}
	return n >= 1 && n <= 63 && last != '-'
}
