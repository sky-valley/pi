// Adapted from TypeBox 1.3.27 (MIT, Copyright (c) 2017-2026 Haydn Paterson);
// see LICENSES/typebox-LICENSE.

package formats

import (
	"regexp"
	"strings"
)

var (
	// iri.ts IpvFutureMatch.
	ipvFuture = regexp.MustCompile(`\[[vV][0-9a-fA-F]+\.[^\]]+\]`)
	// iri_reference.ts MalformedScheme: a scheme-like run straight into "//", without its ":".
	malformedScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+\-.]*//`)
)

// ipvFutureMaxLength is iri.ts IpvFutureMatchMaxLength: longer values skip
// the IPvFuture substitution.
const ipvFutureMaxLength = 2048

// IRI reports whether value is an absolute RFC 3987 IRI: no whitespace,
// control characters or <>^`{|}\, every "%" followed by two hex digits, and
// accepted by the WHATWG URL parser once an IPvFuture literal ([v1.x]) is
// replaced by [::1]. Ports typebox/iri.ts.
func IRI(value string) bool {
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c <= 0x20, c == '<', c == '>', c == '^', c == '`', c == '{', c == '|', c == '}', c == '\\':
			return false
		}
	}
	if !wellFormedPercents(value) {
		return false
	}
	if utf16Len(value) < ipvFutureMaxLength {
		// String.prototype.replace without the g flag: the first match only.
		if loc := ipvFuture.FindStringIndex(value); loc != nil {
			value = value[:loc[0]] + "[::1]" + value[loc[1]:]
		}
	}
	return canParseURL(value, false)
}

// IRIReference reports whether value is an RFC 3987 IRI reference: no
// spaces, control characters or backslashes, every "%" followed by two hex
// digits, no scheme-like prefix running into "//" without a colon, and
// accepted by the WHATWG URL parser against the base http://example.com.
// Ports typebox/iri_reference.ts.
func IRIReference(value string) bool {
	if strings.ContainsFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '\\' }) {
		return false
	}
	return wellFormedPercents(value) && !malformedScheme.MatchString(value) && canParseURL(value, true)
}

// URL reports whether the WHATWG URL parser accepts value with no base,
// URL.canParse(value). Ports typebox/url.ts.
func URL(value string) bool { return canParseURL(value, false) }

// wellFormedPercents is the negation of /%(?![0-9a-fA-F]{2})/.
func wellFormedPercents(s string) bool {
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			return true
		}
		if i+2 >= len(s) || hexValue(rune(s[i+1])) < 0 || hexValue(rune(s[i+2])) < 0 {
			return false
		}
		s = s[i+1:]
	}
}
