package nodepath

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrUnsupportedHost is FileURLToPath's answer for a host it does not parse
// as the WHATWG URL parser would: IPv6 literals and hosts that need IDNA
// (non-ASCII or punycode labels). Node turns those into UNC paths on Windows;
// callers treat every error as "not a file URL".
var ErrUnsupportedHost = errors.New("file URL host needs IPv6 or IDNA processing, which this parser does not implement")

// FileURLToPath is url.fileURLToPath(input, {windows}) for a string: the
// WHATWG URL parser's reading of a file: URL, then Node's conversion of its
// host and path. An error is where Node throws.
func FileURLToPath(input string, windows bool) (string, error) {
	host, pathname, err := parseFileURL(input)
	if err != nil {
		return "", err
	}
	if windows {
		return pathFromURLWin32(input, host, pathname)
	}
	return pathFromURLPosix(input, host, pathname)
}

func pathFromURLPosix(input, hostname, pathname string) (string, error) {
	if hostname != "" {
		return "", errors.New(`File URL host must be "localhost" or empty`)
	}
	for n := 0; n < len(pathname); n++ {
		if pathname[n] == '%' && at(pathname, n+1) == '2' && at(pathname, n+2)|0x20 == 'f' {
			return "", fmt.Errorf("File URL path must not include encoded / characters: %s", input)
		}
	}
	if !strings.Contains(pathname, "%") {
		return pathname, nil
	}
	return decodeURIComponent(pathname)
}

func pathFromURLWin32(input, hostname, pathname string) (string, error) {
	for n := 0; n < len(pathname); n++ {
		if pathname[n] == '%' {
			third := at(pathname, n+2) | 0x20
			if (at(pathname, n+1) == '2' && third == 'f') || (at(pathname, n+1) == '5' && third == 'c') {
				return "", fmt.Errorf("File URL path must not include encoded \\ or / characters: %s", input)
			}
		}
	}
	pathname = strings.ReplaceAll(pathname, "/", "\\")
	if strings.Contains(pathname, "%") {
		decoded, err := decodeURIComponent(pathname)
		if err != nil {
			return "", err
		}
		pathname = decoded
	}
	if hostname != "" {
		// A UNC path. Hosts that domainToUnicode would change (punycode) were
		// refused by the host parser.
		return "\\\\" + hostname + pathname, nil
	}
	letter := at(pathname, 1) | 0x20
	if letter < 'a' || letter > 'z' || at(pathname, 2) != ':' {
		return "", fmt.Errorf("File URL path must be absolute: %s", input)
	}
	return pathname[1:], nil
}

// parseFileURL is the WHATWG URL parser for input that starts with "file:",
// returning the URL's hostname and pathname getters.
func parseFileURL(input string) (hostname, pathname string, err error) {
	// Leading and trailing C0 controls and spaces go, and tabs and newlines
	// anywhere.
	input = strings.TrimFunc(input, func(r rune) bool { return r <= 0x20 })
	input = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, input)
	rest, ok := cutPrefixFold(input, "file:")
	if !ok {
		return "", "", errors.New("Invalid URL scheme: the URL must be of scheme file")
	}
	isSlash := func(c byte) bool { return c == '/' || c == '\\' }
	// file state, then file slash state: "//" leads to a host.
	var path []string
	if len(rest) >= 2 && isSlash(rest[0]) && isSlash(rest[1]) {
		rest = rest[2:]
		end := strings.IndexAny(rest, "/\\?#")
		if end == -1 {
			end = len(rest)
		}
		buffer := rest[:end]
		switch {
		case isWindowsDriveLetter(buffer, false):
			// The drive letter quirk: no host, and the buffer is the path's
			// first segment.
		case buffer == "":
			rest = rest[end:]
		default:
			host, err := parseHost(buffer)
			if err != nil {
				return "", "", err
			}
			if host == "localhost" {
				host = ""
			}
			hostname = host
			rest = rest[end:]
		}
		// path start state: a leading separator is consumed.
		if rest != "" && isSlash(rest[0]) {
			rest = rest[1:]
		}
	} else {
		// "file:" not followed by two slashes; upstream only calls this for
		// "file://", so the remaining forms (file:/x, file:x) are read as a
		// path from the start.
		if rest != "" && isSlash(rest[0]) {
			rest = rest[1:]
		}
	}

	// path state.
	buffer := strings.Builder{}
	for i := 0; ; i++ {
		var c byte
		eof := i >= len(rest)
		if !eof {
			c = rest[i]
		}
		if eof || c == '/' || c == '\\' || c == '?' || c == '#' {
			segment := buffer.String()
			separator := !eof && (c == '/' || c == '\\')
			switch {
			case isDoubleDotSegment(segment):
				if n := len(path); n > 0 && !(n == 1 && isWindowsDriveLetter(path[0], true)) {
					path = path[:n-1]
				}
				if !separator {
					path = append(path, "")
				}
			case isSingleDotSegment(segment):
				if !separator {
					path = append(path, "")
				}
			default:
				if len(path) == 0 && isWindowsDriveLetter(segment, false) {
					segment = segment[:1] + ":"
				}
				path = append(path, segment)
			}
			buffer.Reset()
			if !separator {
				break
			}
			continue
		}
		// UTF-8 percent-encode with the path percent-encode set.
		r, size := utf8.DecodeRuneInString(rest[i:])
		if r < 0x20 || r > 0x7e || strings.ContainsRune(" \"#<>?`{}", r) {
			for _, b := range []byte(string(r)) {
				fmt.Fprintf(&buffer, "%%%02X", b)
			}
		} else {
			buffer.WriteRune(r)
		}
		i += size - 1
	}
	return hostname, "/" + strings.Join(path, "/"), nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// isWindowsDriveLetter: an ASCII letter and ":" (or "|" unless normalized).
func isWindowsDriveLetter(s string, normalized bool) bool {
	return len(s) == 2 && isWindowsDeviceRoot(s[0]) && (s[1] == ':' || (!normalized && s[1] == '|'))
}

func isSingleDotSegment(s string) bool { return s == "." || strings.EqualFold(s, "%2e") }

func isDoubleDotSegment(s string) bool {
	switch strings.ToLower(s) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// parseHost is the host parser for a special URL, for the hosts it handles
// itself: ASCII domains (lowercased, percent-decoded) and IPv4 addresses in
// any of the forms the parser accepts.
func parseHost(input string) (string, error) {
	if strings.HasPrefix(input, "[") {
		return "", ErrUnsupportedHost
	}
	domain, err := percentDecode(input)
	if err != nil {
		return "", err
	}
	for i := 0; i < len(domain); i++ {
		if domain[i] >= 0x80 {
			return "", ErrUnsupportedHost
		}
	}
	domain = strings.ToLower(domain)
	for _, label := range strings.Split(domain, ".") {
		if strings.HasPrefix(label, "xn--") {
			return "", ErrUnsupportedHost
		}
	}
	if domain == "" {
		return "", errors.New("Invalid URL: empty host")
	}
	for i := 0; i < len(domain); i++ {
		if c := domain[i]; c <= 0x20 || c == 0x7f || strings.IndexByte("#%/:<>?@[\\]^|", c) >= 0 {
			return "", errors.New("Invalid URL: forbidden host code point")
		}
	}
	if endsInANumber(domain) {
		return parseIPv4(domain)
	}
	return domain, nil
}

// percentDecode is the URL standard's percent-decode: %XX becomes the byte,
// anything else stays.
func percentDecode(s string) (string, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			out = append(out, byte(v))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return string(out), nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// endsInANumber is the URL standard's ends-in-a-number checker.
func endsInANumber(domain string) bool {
	parts := strings.Split(domain, ".")
	if parts[len(parts)-1] == "" {
		if len(parts) == 1 {
			return false
		}
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	_, ok := parseIPv4Number(last)
	return ok
}

// parseIPv4 is the URL standard's IPv4 parser, serialized.
func parseIPv4(input string) (string, error) {
	parts := strings.Split(input, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return "", errors.New("Invalid URL: IPv4 address with more than four parts")
	}
	numbers := make([]uint64, 0, len(parts))
	for _, part := range parts {
		n, ok := parseIPv4Number(part)
		if !ok {
			return "", errors.New("Invalid URL: IPv4 part is not a number")
		}
		numbers = append(numbers, n)
	}
	for _, n := range numbers[:len(numbers)-1] {
		if n > 255 {
			return "", errors.New("Invalid URL: IPv4 part out of range")
		}
	}
	last := numbers[len(numbers)-1]
	if last >= 1<<(8*(5-len(numbers))) {
		return "", errors.New("Invalid URL: IPv4 address out of range")
	}
	ipv4 := last
	for i, n := range numbers[:len(numbers)-1] {
		ipv4 += n << (8 * (3 - i))
	}
	return fmt.Sprintf("%d.%d.%d.%d", ipv4>>24, (ipv4>>16)&255, (ipv4>>8)&255, ipv4&255), nil
}

// parseIPv4Number reads decimal, 0x hexadecimal or 0 octal.
func parseIPv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X"):
		s, base = s[2:], 16
	case len(s) >= 2 && s[0] == '0':
		s, base = s[1:], 8
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && numErr.Err == strconv.ErrRange {
			return 1 << 40, true
		}
		return 0, false
	}
	return n, true
}

// decodeURIComponent is JavaScript's: every %XX sequence is decoded, and
// sequences must form UTF-8 characters, or it fails (URIError).
func decodeURIComponent(s string) (string, error) {
	malformed := errors.New("URI malformed")
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			out.WriteByte(s[i])
			continue
		}
		b, ok := hexByte(s, i)
		if !ok {
			return "", malformed
		}
		i += 2
		if b < 0x80 {
			out.WriteByte(b)
			continue
		}
		var n int
		switch {
		case b&0xe0 == 0xc0:
			n = 2
		case b&0xf0 == 0xe0:
			n = 3
		case b&0xf8 == 0xf0:
			n = 4
		default:
			return "", malformed
		}
		seq := []byte{b}
		for k := 1; k < n; k++ {
			if i+1 >= len(s) || s[i+1] != '%' {
				return "", malformed
			}
			c, ok := hexByte(s, i+1)
			if !ok || c&0xc0 != 0x80 {
				return "", malformed
			}
			seq = append(seq, c)
			i += 3
		}
		r, size := utf8.DecodeRune(seq)
		if r == utf8.RuneError || size != n {
			return "", malformed
		}
		out.Write(seq)
	}
	return out.String(), nil
}

// hexByte reads the %XX at s[i].
func hexByte(s string, i int) (byte, bool) {
	if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
		return 0, false
	}
	v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
	return byte(v), true
}
