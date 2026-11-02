package schema

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// resolveURI is resolver.ts resolveUri: `ref` resolved against `base` as
// `new URL(ref, base).href` does; where the URL parser fails (a base with an
// opaque path such as a URN, and a reference that is not a fragment), a
// fragment-only reference is appended to the base's document and any other
// reference is taken as it is.
//
// The parser is the part of the WHATWG URL Standard that schema identifiers
// meet: scheme, authority (lowercased host of a special scheme, default port
// dropped), dot segments, query, fragment and the percent-encode sets Node
// applies. It does not do IDNA, IPv4/IPv6 host canonicalization or Windows
// drive letters; a host that would need them is kept as written.
func resolveURI(ref, base string) string {
	b, ok := parseURL(base, nil)
	if ok {
		if u, ok := parseURL(ref, b); ok {
			return u.String()
		}
	}
	if strings.HasPrefix(ref, "#") {
		return documentOf(base) + ref
	}
	return ref
}

// documentOf is `uri` without its fragment.
func documentOf(uri string) string {
	if i := strings.IndexByte(uri, '#'); i >= 0 {
		return uri[:i]
	}
	return uri
}

type urlRecord struct {
	scheme      string
	hasHost     bool
	userinfo    string // serialized, without the '@'
	host        string
	port        string
	path        []string
	opaque      bool // path is opaquePath, a URN's "uuid:..."
	opaquePath  string
	hasQuery    bool
	query       string
	hasFragment bool
	fragment    string
}

var specialPorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21", "file": ""}

func isSpecial(scheme string) bool {
	_, ok := specialPorts[scheme]
	return ok
}

func (u *urlRecord) String() string {
	var b strings.Builder
	b.WriteString(u.scheme)
	b.WriteByte(':')
	switch {
	case u.hasHost:
		b.WriteString("//")
		if u.userinfo != "" {
			b.WriteString(u.userinfo)
			b.WriteByte('@')
		}
		b.WriteString(u.host)
		if u.port != "" {
			b.WriteByte(':')
			b.WriteString(u.port)
		}
	case !u.opaque && len(u.path) > 1 && u.path[0] == "":
		b.WriteString("/.")
	}
	if u.opaque {
		b.WriteString(u.opaquePath)
	} else {
		for _, s := range u.path {
			b.WriteByte('/')
			b.WriteString(s)
		}
	}
	if u.hasQuery {
		b.WriteByte('?')
		b.WriteString(u.query)
	}
	if u.hasFragment {
		b.WriteByte('#')
		b.WriteString(u.fragment)
	}
	return b.String()
}

// parseURL is the basic URL parser over `input` with an optional base;
// ok is false where it fails.
func parseURL(input string, base *urlRecord) (*urlRecord, bool) {
	input = strings.TrimFunc(input, func(r rune) bool { return r <= 0x20 })
	input = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(input)
	if scheme, rest, ok := splitScheme(input); ok {
		special := isSpecial(scheme)
		if special && base != nil && base.scheme == scheme && !startsWithSlash(rest, true) {
			return parseRelative(rest, base)
		}
		u := &urlRecord{scheme: scheme}
		switch {
		case scheme == "file":
			if strings.HasPrefix(rest, "//") {
				return u.parseAuthority(rest[2:])
			}
			u.hasHost = true
			return u.parsePathQueryFragment("/"+strings.TrimLeft(rest, `/\`), nil)
		case special:
			return u.parseAuthority(strings.TrimLeft(rest, `/\`))
		case strings.HasPrefix(rest, "//"):
			return u.parseAuthority(rest[2:])
		case strings.HasPrefix(rest, "/"):
			return u.parsePathQueryFragment(rest, nil)
		}
		path, query, fragment := splitPathQueryFragment(rest)
		u.opaque = true
		u.opaquePath = percentEncode(path, c0ControlSet)
		u.setQueryFragment(query, fragment)
		return u, true
	}
	if base == nil {
		return nil, false
	}
	if base.opaque {
		if !strings.HasPrefix(input, "#") {
			return nil, false
		}
		u := *base
		u.hasFragment, u.fragment = true, percentEncode(input[1:], fragmentSet)
		return &u, true
	}
	return parseRelative(input, base)
}

func splitScheme(input string) (scheme, rest string, ok bool) {
	if input == "" || !isASCIIAlpha(input[0]) {
		return "", "", false
	}
	for i := 1; i < len(input); i++ {
		c := input[i]
		switch {
		case c == ':':
			return strings.ToLower(input[:i]), input[i+1:], true
		case isASCIIAlpha(c) || ('0' <= c && c <= '9') || c == '+' || c == '-' || c == '.':
		default:
			return "", "", false
		}
	}
	return "", "", false
}

func isASCIIAlpha(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

func startsWithSlash(s string, special bool) bool {
	return s != "" && (s[0] == '/' || (special && s[0] == '\\'))
}

// parseRelative resolves a reference without a scheme against a base with a
// hierarchical path.
func parseRelative(input string, base *urlRecord) (*urlRecord, bool) {
	special := isSpecial(base.scheme)
	u := &urlRecord{scheme: base.scheme}
	if len(input) >= 2 && startsWithSlash(input, special) && startsWithSlash(input[1:], special) {
		if special && u.scheme != "file" {
			return u.parseAuthority(strings.TrimLeft(input, `/\`))
		}
		return u.parseAuthority(input[2:])
	}
	u.hasHost, u.userinfo, u.host, u.port = base.hasHost, base.userinfo, base.host, base.port
	if startsWithSlash(input, special) {
		return u.parsePathQueryFragment(input, nil)
	}
	switch {
	case input == "" || input[0] == '#':
		u.path = base.path
		u.hasQuery, u.query = base.hasQuery, base.query
		if input != "" {
			u.hasFragment, u.fragment = true, percentEncode(input[1:], fragmentSet)
		}
		return u, true
	case input[0] == '?':
		u.path = base.path
		_, query, fragment := splitPathQueryFragment(input)
		u.setQueryFragment(query, fragment)
		return u, true
	}
	dir := base.path
	if len(dir) > 0 {
		dir = dir[:len(dir)-1]
	}
	return u.parsePathQueryFragment("/"+input, dir)
}

// parseAuthority reads `userinfo@host:port` and what follows it.
func (u *urlRecord) parseAuthority(s string) (*urlRecord, bool) {
	special := isSpecial(u.scheme)
	end := len(s)
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '/' || c == '?' || c == '#' || (special && c == '\\') {
			end = i
			break
		}
	}
	authority, rest := s[:end], s[end:]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		user, password, hasPassword := strings.Cut(authority[:at], ":")
		u.userinfo = percentEncode(user, userinfoSet)
		if hasPassword && password != "" {
			u.userinfo += ":" + percentEncode(password, userinfoSet)
		}
		authority = authority[at+1:]
	}
	host := authority
	if colon := strings.LastIndexByte(authority, ':'); colon >= 0 && !strings.Contains(authority[colon:], "]") {
		host = authority[:colon]
		port := authority[colon+1:]
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 0 || n > 65535 || strings.ContainsAny(port, "+-") {
				return nil, false
			}
			if p := strconv.Itoa(n); p != specialPorts[u.scheme] || !special {
				u.port = p
			}
		}
	}
	switch {
	case strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]"):
		host = strings.ToLower(host) // an IPv6 address, kept as written
	case special:
		decoded, err := percentDecode(host)
		if err != nil || (decoded == "" && u.scheme != "file") || strings.ContainsAny(decoded, forbiddenDomain) {
			return nil, false
		}
		host = strings.ToLower(decoded)
	default:
		if strings.ContainsAny(host, forbiddenHost) {
			return nil, false
		}
		host = percentEncode(host, c0ControlSet)
	}
	u.hasHost, u.host = true, host
	if rest == "" || !startsWithSlash(rest, special) {
		if special {
			u.path = []string{""}
		}
		_, query, fragment := splitPathQueryFragment(rest)
		u.setQueryFragment(query, fragment)
		return u, true
	}
	return u.parsePathQueryFragment(rest, nil)
}

const (
	forbiddenHost   = " #/:<>?@[\\]^|\x00\t\n\r"
	forbiddenDomain = forbiddenHost + "%\x7f"
)

// parsePathQueryFragment reads an absolute path (s starts with a slash) onto
// the segments `dir`, then the query and fragment.
func (u *urlRecord) parsePathQueryFragment(s string, dir []string) (*urlRecord, bool) {
	special := isSpecial(u.scheme)
	path, query, fragment := splitPathQueryFragment(s)
	segments := append([]string(nil), dir...)
	if special {
		path = strings.ReplaceAll(path, `\`, "/")
	}
	parts := strings.Split(path[1:], "/")
	for i, p := range parts {
		last := i == len(parts)-1
		switch strings.ToLower(p) {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(segments) > 0 {
				segments = segments[:len(segments)-1]
			}
			if last {
				segments = append(segments, "")
			}
		case ".", "%2e":
			if last {
				segments = append(segments, "")
			}
		default:
			segments = append(segments, percentEncode(p, pathSet))
		}
	}
	u.path = segments
	u.setQueryFragment(query, fragment)
	return u, true
}

// splitPathQueryFragment splits s at its first '?' or '#'; query and fragment
// keep their leading delimiter ("" when absent).
func splitPathQueryFragment(s string) (path, query, fragment string) {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s, fragment = s[:i], s[i:]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s, query = s[:i], s[i:]
	}
	return s, query, fragment
}

func (u *urlRecord) setQueryFragment(query, fragment string) {
	if query != "" {
		set := querySet
		if isSpecial(u.scheme) {
			set = specialQuerySet
		}
		u.hasQuery, u.query = true, percentEncode(query[1:], set)
	}
	if fragment != "" {
		u.hasFragment, u.fragment = true, percentEncode(fragment[1:], fragmentSet)
	}
}

// The percent-encode sets: each also encodes every byte outside printable
// ASCII (the C0 control percent-encode set).
const (
	c0ControlSet    = ""
	fragmentSet     = " \"<>`"
	querySet        = " \"#<>"
	specialQuerySet = querySet + "'"
	pathSet         = querySet + "?`{}"
	userinfoSet     = pathSet + "/:;=@[\\]^|"
)

func percentEncode(s, set string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || strings.IndexByte(set, c) >= 0 {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func percentDecode(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b = append(b, byte(n))
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return string(b), nil
}

// errURIMalformed is the URIError decodeURIComponent throws.
var errURIMalformed = errors.New("URI malformed")

// decodeURIComponent decodes every %XX escape; an escape that is not hex, or
// escaped bytes that are not UTF-8 (a surrogate included), fail as in
// JavaScript.
func decodeURIComponent(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	escaped := func(i int) (byte, bool) {
		if i+3 > len(s) || s[i] != '%' {
			return 0, false
		}
		n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		return byte(n), err == nil
	}
	var b []byte
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b = append(b, s[i])
			i++
			continue
		}
		lead, ok := escaped(i)
		if !ok {
			return "", errURIMalformed
		}
		i += 3
		n := 1
		switch {
		case lead < 0x80:
		case lead&0xE0 == 0xC0:
			n = 2
		case lead&0xF0 == 0xE0:
			n = 3
		case lead&0xF8 == 0xF0:
			n = 4
		default:
			return "", errURIMalformed
		}
		seq := []byte{lead}
		for range n - 1 {
			c, ok := escaped(i)
			if !ok {
				return "", errURIMalformed
			}
			seq = append(seq, c)
			i += 3
		}
		if !utf8.Valid(seq) {
			return "", errURIMalformed
		}
		b = append(b, seq...)
	}
	return string(b), nil
}
