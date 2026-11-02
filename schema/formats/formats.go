// Package formats holds the JSON Schema `format` checks of upstream pi-schema
// (packages/schema/src/formats, the @earendil-works/pi-schema/formats entry
// point), which adapts TypeBox 1.3.27's checks (MIT, Copyright (c) 2017-2026
// Haydn Paterson; see LICENSES/typebox-LICENSE).
//
// Each check reports whether a string has the format, with upstream's
// semantics on Node v22: its JavaScript regular expressions (UTF-16 code
// units, V8's Unicode 17 property tables, `$` that never matches before a
// trailing newline), its IDNA label rules, and — for url, iri and
// iri-reference — URL.canParse, which this package answers with a port of
// Ada 2.9.2, the WHATWG URL parser inside Node v22 (see whatwg.go). The
// regex format answers whether `new RegExp(value, "u")` would compile, with
// a port of V8's /u pattern grammar (see regex.go).
//
// The Unicode-dependent tables are read from that Node (tables_v8.go, Unicode
// 17.0) and from Ada's source (tables_ada.go, IDNA 15.0), so classification
// matches upstream rather than Go's older unicode package. The known
// differences from upstream on Node v22.23.4:
//
//   - Input is a Go string, normally decoded from JSON. A JSON escape of a
//     lone UTF-16 surrogate decodes to U+FFFD, so where upstream would see
//     the lone surrogate the checks see U+FFFD (the golden test records both
//     answers; of its rows only regex "[\udc00-\ud800]" changes, false
//     upstream and true here).
//   - idn-email and idn-hostname normalize to NFC with golang.org/x/text,
//     whose data is Unicode 15.0; Node's ICU is Unicode 17.0. Decomposed text
//     that only Unicode 16 composes — U+105C9, U+105E4, U+11383, U+11385,
//     U+1138E, U+11391, U+113C5, U+113C7, U+113C8, U+16121…U+16128 and
//     U+16D68…U+16D6A (Todhri, Tulu-Tigalari, Gurung Khema, Kirat Rai) — and
//     marks added after Unicode 15.0 with a nonzero combining class stay as
//     they are here. (Ada's own NFC, behind url and iri, is also Unicode
//     15.0.)
//   - Node v22's optimized URL.canParse can hand Ada a Latin-1 string as
//     UTF-8 (V8's fast API path), so upstream's url/iri answers for hosts
//     such as "é.com" flip once the call is hot; these checks give the
//     unoptimized, standard answer every time.
package formats

import (
	"regexp"
	"strings"
)

// All returns every check by its format name: the formats JSON Schema
// 2020-12 defines plus "url" and "json-pointer-uri-fragment", which TypeBox
// and ajv-formats also check. It returns a new map on each call, the Go
// analogue of upstream's frozen `formats` object; its values have the shape
// of the schema package's FormatCheck.
func All() map[string]func(string) bool {
	return map[string]func(string) bool{
		"date":                      Date,
		"date-time":                 DateTime,
		"duration":                  Duration,
		"email":                     Email,
		"hostname":                  Hostname,
		"idn-email":                 IdnEmail,
		"idn-hostname":              IdnHostname,
		"ipv4":                      IPv4,
		"ipv6":                      IPv6,
		"iri":                       IRI,
		"iri-reference":             IRIReference,
		"json-pointer":              JSONPointer,
		"json-pointer-uri-fragment": JSONPointerURIFragment,
		"regex":                     Regex,
		"relative-json-pointer":     RelativeJSONPointer,
		"time":                      Time,
		"uri":                       URI,
		"uri-reference":             URIReference,
		"uri-template":              URITemplate,
		"url":                       URL,
		"uuid":                      UUID,
	}
}

// The upstream patterns that carry the i flag are spelled out with both
// letter cases here: without /u, JavaScript's case-insensitive matching never
// pairs a non-ASCII code point with an ASCII one, while Go's (?i) folds
// U+212A KELVIN SIGN into k and U+017F LONG S into s.

var (
	// typebox/date.ts DATE.
	datePattern = regexp.MustCompile(`^(\d\d\d\d)-(\d\d)-(\d\d)$`)
	// typebox/time.ts TIME.
	timePattern = regexp.MustCompile(`^(\d\d):(\d\d):(\d\d)(?:\.\d+)?(?:([Zz])|([+-])(\d\d):(\d\d))?$`)
	// typebox/duration.ts Duration.
	durationPattern = regexp.MustCompile(`^P((\d+Y(\d+M(\d+D)?)?|\d+M(\d+D)?|\d+D)(T(\d+H(\d+M(\d+S)?)?|\d+M(\d+S)?|\d+S))?|T(\d+H(\d+M(\d+S)?)?|\d+M(\d+S)?|\d+S)|\d+W)$`)
	// typebox/email.ts Email (/i).
	emailPattern = regexp.MustCompile("^(?:[a-zA-Z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[a-zA-Z0-9!#$%&'*+/=?^_`{|}~-]+)*|\"(?:[^\"\\\\]|\\\\[\\x20-\\x7e])*\")" +
		`@(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*` +
		`|\[(?:[Ii][Pp][Vv]6:[a-fA-F0-9:]+|` + decOctet + `(?:\.` + decOctet + `){3})\])$`)
	// typebox/ipv4.ts IPv4.
	ipv4Pattern = regexp.MustCompile(`^` + ipv4Address + `$`)
	// typebox/ipv6.ts IPv6 (/i).
	ipv6Pattern = regexp.MustCompile(`^(?:(?:(?:` + h16 + `:){6}|::(?:` + h16 + `:){5}|(?:` + h16 + `)?::(?:` + h16 + `:){4}` +
		`|(?:(?:` + h16 + `:)?` + h16 + `)?::(?:` + h16 + `:){3}|(?:(?:` + h16 + `:){0,2}` + h16 + `)?::(?:` + h16 + `:){2}` +
		`|(?:(?:` + h16 + `:){0,3}` + h16 + `)?::` + h16 + `:|(?:(?:` + h16 + `:){0,4}` + h16 + `)?::)` + ls32 +
		`|(?:(?:` + h16 + `:){0,5}` + h16 + `)?::` + h16 + `|(?:(?:` + h16 + `:){0,6}` + h16 + `)?::)$`)
	// typebox/uuid.ts Uuid (/i).
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)
	// typebox/json_pointer.ts JsonPointer.
	jsonPointerPattern = regexp.MustCompile(`^(?:/(?:[^~/]|~0|~1)*)*$`)
	// typebox/json_pointer_uri_fragment.ts JsonPointerUriFragment (/i).
	jsonPointerURIFragmentPattern = regexp.MustCompile(`^#(?:/(?:[a-zA-Z0-9_\-.!$&'()*+,;:=@]|%[0-9a-fA-F]{2}|~0|~1)*)*$`)
	// typebox/relative_json_pointer.ts RelativeJsonPointer.
	relativeJSONPointerPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:#|(?:/(?:[^~/]|~0|~1)*)*)$`)
	// typebox/uri_template.ts UriTemplate (/i).
	uriTemplatePattern = regexp.MustCompile("^(?:(?:[^\\x00-\\x20\"<>%\\\\^`{|}\\x7f]|" + pctEncoded + `)|\{[+#./;?&=,!@|]?` +
		templateVarspec + `(?:,` + templateVarspec + `)*\})*$`)
	// typebox/uri.ts Uri (/i).
	uriPattern = regexp.MustCompile(`^` + uriScheme + `:(?:` + uriHierPart + `)?` + uriQueryFragment + `$`)
	// typebox/uri_reference.ts UriReference (/i).
	uriReferencePattern = regexp.MustCompile(`^(?:` + uriScheme + `:(?:` + uriHierPart + `)?|(?:` + uriRelativePart + `)?)` +
		uriQueryFragment + `$`)
)

// Pieces of the upstream patterns, named after RFC 3986's grammar.
const (
	decOctet    = `(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`
	ipv4Address = `(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)`
	h16         = `[0-9a-fA-F]{1,4}`
	ls32        = `(?:` + h16 + `:` + h16 + `|` + ipv4Address + `)`
	pctEncoded  = `%[0-9a-fA-F]{2}`

	templateVarchar = `(?:[a-zA-Z0-9_]|` + pctEncoded + `)`
	templateVarspec = templateVarchar + `+(?:\.` + templateVarchar + `+)*(?::[1-9]\d{0,3}|\*)?`

	uriScheme      = `[a-zA-Z][a-zA-Z0-9+\-.]*`
	uriIPv6Address = `(?:(?:` + h16 + `:){6}` + ls32 + `|::(?:` + h16 + `:){5}` + ls32 + `|(?:` + h16 + `)?::(?:` + h16 + `:){4}` + ls32 +
		`|(?:(?:` + h16 + `:){0,1}` + h16 + `)?::(?:` + h16 + `:){3}` + ls32 + `|(?:(?:` + h16 + `:){0,2}` + h16 + `)?::(?:` + h16 + `:){2}` + ls32 +
		`|(?:(?:` + h16 + `:){0,3}` + h16 + `)?::` + h16 + `:` + ls32 + `|(?:(?:` + h16 + `:){0,4}` + h16 + `)?::` + ls32 +
		`|(?:(?:` + h16 + `:){0,5}` + h16 + `)?::` + h16 + `|(?:(?:` + h16 + `:){0,6}` + h16 + `)?::)`
	uriUnreserved = `\-a-zA-Z0-9._~`
	uriSubDelims  = `!$&'()*+,;=`
	uriPchar      = `(?:[` + uriUnreserved + uriSubDelims + `:@]|` + pctEncoded + `)`
	uriAuthority  = `(?:(?:[` + uriUnreserved + uriSubDelims + `:]|` + pctEncoded + `)*@)?` +
		`(?:\[(?:` + uriIPv6Address + `|[vV][0-9a-fA-F]+\.[` + uriUnreserved + uriSubDelims + `:]+)\]|` + ipv4Address +
		`|(?:[` + uriUnreserved + uriSubDelims + `]|` + pctEncoded + `)*)(?::\d*)?`
	uriPathAbempty  = `(?:/` + uriPchar + `*)*`
	uriPathAbsolute = `/(?:` + uriPchar + `+` + uriPathAbempty + `)?`
	uriHierPart     = `//` + uriAuthority + uriPathAbempty + `|` + uriPathAbsolute + `|` + uriPchar + `+` + uriPathAbempty
	// The relative part's first segment admits no colon (RFC 3986 path-noscheme).
	uriRelativePart  = `//` + uriAuthority + uriPathAbempty + `|` + uriPathAbsolute + `|(?:[` + uriUnreserved + uriSubDelims + `@]|` + pctEncoded + `)+` + uriPathAbempty
	uriQueryFragment = `(?:\?(?:[` + uriUnreserved + uriSubDelims + `:@/?]|` + pctEncoded + `)*)?(?:#(?:[` + uriUnreserved + uriSubDelims + `:@/?]|` + pctEncoded + `)*)?`
)

var daysInMonth = [...]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func isLeapYear(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// atoi reads the decimal digits a pattern has already matched.
func atoi(digits string) int {
	n := 0
	for i := 0; i < len(digits); i++ {
		n = n*10 + int(digits[i]-'0')
	}
	return n
}

// Date reports whether value is an RFC 3339 full-date such as 2020-12-12.
// Ports typebox/date.ts.
func Date(value string) bool {
	m := datePattern.FindStringSubmatch(value)
	if m == nil {
		return false
	}
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if month < 1 || month > 12 || day < 1 {
		return false
	}
	if month == 2 && isLeapYear(year) {
		return day <= 29
	}
	return day <= daysInMonth[month]
}

// Time reports whether value is an RFC 3339 full-time with a required
// offset ("Z", "z" or ±hh:mm); a leap second (ss = 60) must fall on 23:59
// UTC. Ports typebox/time.ts with its default strictTimeZone.
func Time(value string) bool {
	m := timePattern.FindStringSubmatch(value)
	if m == nil {
		return false
	}
	zulu, sign := m[4], m[5]
	if zulu == "" && sign == "" {
		return false
	}
	hour, minute, second := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if hour > 23 || minute > 59 || second > 60 {
		return false
	}
	offset := 0
	if sign != "" {
		tzHour, tzMinute := atoi(m[6]), atoi(m[7])
		if tzHour > 23 || tzMinute > 59 {
			return false
		}
		offset = tzHour*60 + tzMinute
		if sign == "-" {
			offset = -offset
		}
	}
	if second < 60 {
		return true
	}
	utcMinutes := ((hour*60+minute-offset)%1440 + 1440) % 1440
	return utcMinutes == 23*60+59
}

// DateTime reports whether value is an RFC 3339 date-time: a Date and a
// Time joined by exactly one "T" or "t". Ports typebox/date_time.ts.
func DateTime(value string) bool {
	i := strings.IndexAny(value, "Tt")
	if i < 0 || strings.ContainsAny(value[i+1:], "Tt") {
		return false
	}
	return Date(value[:i]) && Time(value[i+1:])
}

// Duration reports whether value is an ISO 8601 duration such as P1Y2M3DT4H
// or P2W. Ports typebox/duration.ts.
func Duration(value string) bool { return durationPattern.MatchString(value) }

// Email reports whether value is an RFC 5322 address with an ASCII local
// part and a host name or bracketed address literal. Ports typebox/email.ts.
func Email(value string) bool { return emailPattern.MatchString(value) }

// IPv4 reports whether value is a dotted-quad IPv4 address without leading
// zeros. Ports typebox/ipv4.ts.
func IPv4(value string) bool { return ipv4Pattern.MatchString(value) }

// IPv6 reports whether value is an RFC 4291 text IPv6 address (no zone).
// Ports typebox/ipv6.ts.
func IPv6(value string) bool { return ipv6Pattern.MatchString(value) }

// UUID reports whether value is a hyphenated UUID of any version. Ports
// typebox/uuid.ts.
func UUID(value string) bool { return uuidPattern.MatchString(value) }

// JSONPointer reports whether value is an RFC 6901 JSON Pointer. Ports
// typebox/json_pointer.ts.
func JSONPointer(value string) bool { return jsonPointerPattern.MatchString(value) }

// JSONPointerURIFragment reports whether value is a JSON Pointer in URI
// fragment form ("#/a/b"). Ports typebox/json_pointer_uri_fragment.ts.
func JSONPointerURIFragment(value string) bool {
	return jsonPointerURIFragmentPattern.MatchString(value)
}

// RelativeJSONPointer reports whether value is a relative JSON Pointer
// ("0/a", "1#"). Ports typebox/relative_json_pointer.ts.
func RelativeJSONPointer(value string) bool { return relativeJSONPointerPattern.MatchString(value) }

// URITemplate reports whether value is an RFC 6570 URI template. Ports
// typebox/uri_template.ts.
func URITemplate(value string) bool { return uriTemplatePattern.MatchString(value) }

// URI reports whether value is an absolute RFC 3986 URI (ASCII only). Ports
// typebox/uri.ts.
func URI(value string) bool { return uriPattern.MatchString(value) }

// URIReference reports whether value is an RFC 3986 URI or relative
// reference (ASCII only). Ports typebox/uri_reference.ts.
func URIReference(value string) bool { return uriReferencePattern.MatchString(value) }
