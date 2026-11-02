// Ported from Ada 2.9.2 (MIT, Copyright 2023 Yagiz Nizipli and Daniel Lemire);
// see LICENSES/ada-LICENSE-MIT.

package formats

import "strings"

// canParseURL is Node v22's URL.canParse(input) — or, with exampleBase,
// URL.canParse(input, "http://example.com") — which is Ada 2.9.2's
// can_parse: parse_url_impl run for validity only. Every path below that
// ends in success is a state Ada reaches without error; only the scheme,
// the authority (userinfo, host, port) and file hosts can fail, so the
// path, query and fragment are never inspected.
//
// The input is the UTF-8 Node hands Ada (a lone surrogate arrives as
// U+FFFD, which is what Go's JSON decoding produces too).
func canParseURL(input string, exampleBase bool) bool {
	// Strip ASCII tab and newline everywhere, then leading and trailing C0
	// controls and spaces; the fragment never fails.
	input = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, input)
	input = strings.TrimFunc(input, func(r rune) bool { return r <= ' ' })
	if i := strings.IndexByte(input, '#'); i >= 0 {
		input = input[:i]
	}

	if end := schemeEnd(input); end > 0 {
		scheme := strings.ToLower(input[:end])
		rest := input[end+1:]
		special := isSpecialScheme(scheme)
		switch {
		case scheme == "file":
			return parseFileURL(rest)
		case exampleBase && scheme == "http":
			// special relative or authority state
			if strings.HasPrefix(rest, "//") {
				return parseAuthority(strings.TrimLeft(rest[2:], `/\`), true)
			}
			return parseRelative(rest)
		case special:
			// special authority slashes, then special authority ignore slashes
			return parseAuthority(strings.TrimLeft(rest, `/\`), true)
		case strings.HasPrefix(rest, "//"):
			return parseAuthority(rest[2:], false)
		default:
			return true // path or authority without "//", or an opaque path
		}
	}
	// no scheme state: only a base rescues it (http://example.com, not file
	// and without an opaque path)
	return exampleBase && parseRelative(input)
}

// schemeEnd returns the index of the ":" that ends a leading scheme
// (ALPHA *( ALPHA / DIGIT / "+" / "-" / "." )), or -1.
func schemeEnd(s string) int {
	if s == "" || !isASCIIAlpha(s[0]) {
		return -1
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			return i
		case isASCIIAlpha(c), c >= '0' && c <= '9', c == '+', c == '-', c == '.':
		default:
			return -1
		}
	}
	return -1
}

func isASCIIAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isSpecialScheme(scheme string) bool {
	switch scheme {
	case "http", "https", "ws", "wss", "ftp", "file":
		return true
	}
	return false
}

// parseRelative is the relative state against the special, non-file base
// http://example.com.
func parseRelative(rest string) bool {
	if rest == "" || rest[0] != '/' && rest[0] != '\\' {
		return true // base's authority, a relative path
	}
	// relative slash state
	rest = rest[1:]
	if rest != "" && (rest[0] == '/' || rest[0] == '\\') {
		return parseAuthority(strings.TrimLeft(rest, `/\`), true)
	}
	return true // an absolute path on the base's authority
}

// parseAuthority is the authority state: the userinfo runs to the last "@"
// before the host's end; then the host and port states.
func parseAuthority(rest string, special bool) bool {
	delims := "@/?"
	if special {
		delims = `@/?\`
	}
	for atSeen := false; ; atSeen = true {
		i := strings.IndexAny(rest, delims)
		if i < 0 || rest[i] != '@' {
			if atSeen && (i == 0 || rest == "") {
				return false // credentials without a host
			}
			return parseHostAndPort(rest, special)
		}
		rest = rest[i+1:]
	}
}

// parseHostAndPort is the host state (with its port state): the host runs
// to ":" outside brackets, "/", "?", or "\" for a special URL.
func parseHostAndPort(rest string, special bool) bool {
	end, colon := len(rest), false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c == '[' {
			j := strings.IndexByte(rest[i:], ']')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		if c == ':' || c == '/' || c == '?' || special && c == '\\' {
			end, colon = i, c == ':'
			break
		}
	}
	host := rest[:end]
	if colon {
		return parseHost(host, special) && validPort(rest[end+1:], special)
	}
	if host == "" {
		return !special
	}
	return parseHost(host, special)
}

// validPort is Ada's parse_port with trailing-content checking: digits that
// fit in 16 bits (or none), then the end, "/", "?" or, special, "\".
func validPort(rest string, special bool) bool {
	n, value := 0, 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		value = value*10 + int(rest[n]-'0')
		if value > 0xffff {
			return false
		}
		n++
	}
	if n == len(rest) {
		return true
	}
	switch rest[n] {
	case '/', '?':
		return true
	case '\\':
		return special
	}
	return false
}

// parseFileURL is the file, file slash and file host states for a URL
// without a base file URL.
func parseFileURL(rest string) bool {
	if rest == "" || rest[0] != '/' && rest[0] != '\\' {
		return true
	}
	rest = rest[1:]
	if rest == "" || rest[0] != '/' && rest[0] != '\\' {
		return true
	}
	rest = rest[1:]
	host := rest
	if i := strings.IndexAny(rest, `/\?`); i >= 0 {
		host = rest[:i]
	}
	if host == "" || len(host) == 2 && isASCIIAlpha(host[0]) && (host[1] == ':' || host[1] == '|') {
		return true // an empty host, or a Windows drive letter that starts the path
	}
	return parseHost(host, true)
}

// parseHost is the host parser: an IPv6 literal in brackets, an opaque host
// for a non-special URL, else a domain run through domain-to-ASCII that may
// turn out to be an IPv4 address.
func parseHost(input string, special bool) bool {
	if input == "" {
		return false
	}
	if input[0] == '[' {
		return input[len(input)-1] == ']' && validIPv6(input[1:len(input)-1])
	}
	if !special {
		return !strings.ContainsAny(input, "\x00\t\n\r #/:<>?@[\\]^|")
	}
	ascii, ok := domainToASCII(percentDecode(input))
	if !ok || ascii == "" || strings.IndexFunc(ascii, isForbiddenDomainCodePoint) >= 0 {
		return false
	}
	if endsInNumber(ascii) {
		return validIPv4Host(ascii)
	}
	return true
}

func isForbiddenDomainCodePoint(r rune) bool {
	if r <= 0x20 || r >= 0x7f {
		return true
	}
	return strings.ContainsRune("#%/:<>?@[\\]^|", r)
}

// percentDecode decodes each "%" followed by two hex digits into its byte.
func percentDecode(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := hexValue(rune(s[i+1])), hexValue(rune(s[i+2])); hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// endsInNumber is Ada's checkers::is_ipv4 on a lowercase ASCII domain: the
// last label (after dropping one trailing dot) is all digits, or "0x"
// followed by lowercase hex digits.
func endsInNumber(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" {
		return false
	}
	if strings.Trim(last, "0123456789") == "" {
		return true
	}
	return len(last) >= 2 && last[:2] == "0x" && strings.Trim(last[2:], "0123456789abcdef") == ""
}

// validIPv4Host is Ada's parse_ipv4: up to four dot-separated numbers in
// decimal, octal (leading 0) or hex (0x), each but the last below 256, the
// last filling the remaining bytes; one trailing dot is allowed.
func validIPv4Host(host string) bool {
	host = strings.TrimSuffix(host, ".")
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return false
	}
	for i, part := range parts {
		v, ok := ipv4Number(part)
		if !ok {
			return false
		}
		if i < len(parts)-1 {
			if v > 255 {
				return false
			}
		} else if v >= 1<<(32-8*i) {
			return false
		}
	}
	return true
}

// ipv4Number parses one IPv4 part the way Ada's std::from_chars calls do:
// "0x" alone is 0, a 32-bit overflow fails, and the whole part must be
// consumed.
func ipv4Number(part string) (uint64, bool) {
	base := uint64(10)
	switch {
	case len(part) >= 2 && part[0] == '0' && (part[1] == 'x' || part[1] == 'X'):
		if len(part) == 2 {
			return 0, true
		}
		part, base = part[2:], 16
	case len(part) >= 2 && part[0] == '0' && part[1] >= '0' && part[1] <= '9':
		part, base = part[1:], 8
	}
	if part == "" {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(part); i++ {
		d := uint64(hexValue(rune(part[i])))
		if hexValue(rune(part[i])) < 0 || d >= base {
			return 0, false
		}
		v = v*base + d
		if v > 0xffffffff {
			return 0, false
		}
	}
	return v, true
}

// validIPv6 is the WHATWG IPv6 parser as Ada implements it.
func validIPv6(s string) bool {
	if s == "" {
		return false
	}
	piece, compress, i := 0, -1, 0
	if s[0] == ':' {
		if len(s) == 1 || s[1] != ':' {
			return false
		}
		i = 2
		piece++
		compress = piece
	}
	for i < len(s) {
		if piece == 8 {
			return false
		}
		if s[i] == ':' {
			if compress >= 0 {
				return false
			}
			i++
			piece++
			compress = piece
			continue
		}
		length := 0
		for length < 4 && i < len(s) && hexValue(rune(s[i])) >= 0 {
			i++
			length++
		}
		if i < len(s) && s[i] == '.' {
			if length == 0 || piece > 6 {
				return false
			}
			i -= length
			if !validIPv6IPv4(s[i:]) {
				return false
			}
			piece += 2
			break
		}
		if i < len(s) && s[i] == ':' {
			i++
			if i == len(s) {
				return false
			}
		} else if i < len(s) {
			return false
		}
		piece++
	}
	return compress >= 0 || piece == 8
}

// validIPv6IPv4 is the dotted-quad tail of an IPv6 address: four decimal
// numbers up to 255 without leading zeros.
func validIPv6IPv4(s string) bool {
	numbers, i := 0, 0
	for i < len(s) {
		if numbers > 0 {
			if s[i] != '.' || numbers == 4 {
				return false
			}
			i++
		}
		if i == len(s) || s[i] < '0' || s[i] > '9' {
			return false
		}
		value := -1
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			d := int(s[i] - '0')
			switch {
			case value < 0:
				value = d
			case value == 0:
				return false
			default:
				value = value*10 + d
			}
			if value > 255 {
				return false
			}
			i++
		}
		numbers++
	}
	return numbers == 4
}
