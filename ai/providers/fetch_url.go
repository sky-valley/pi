package providers

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// errInvalidURL is the TypeError `new URL(...)` throws for a URL the WHATWG
// URL parser refuses.
var errInvalidURL = errors.New("Invalid URL")

// specialSchemePorts are the WHATWG URL Standard's special schemes but file,
// each with its default port.
var specialSchemePorts = map[string]int{"http": 80, "https": 443, "ws": 80, "wss": 443, "ftp": 21}

// sdkJoinURL is the string an SDK adapter's request URL is parsed from: its
// base URL and an endpoint path that starts with a slash, joined as
// @anthropic-ai/sdk's and openai's buildURL join them — the path's slash
// dropped when the base ends with one — and as @google/genai's constructUrl
// does, which drops one trailing slash from the base and joins with a slash:
// the same string. One slash goes, so a base that ends in two sends a path
// with an empty segment, "/v1//chat/completions". (pi-messages strips every
// trailing slash itself.)
func sdkJoinURL(base, path string) string {
	if strings.HasSuffix(base, "/") {
		return base + strings.TrimPrefix(path, "/")
	}
	return base + path
}

// requestURL is the URL a request an adapter built from rawURL is sent to:
// what pi's `new URL(rawURL)` — the WHATWG URL parser, which every adapter's
// request URL goes through — makes of it, as net/url can hold it; or
// errInvalidURL where that parser refuses it, as far as net/url tells: a URL
// with no scheme, one net/url cannot parse, a special one with no host, or a
// port past 65535.
//
// net/url parses rawURL after the WHATWG parser's own handling of its input
// (whatwgURLInput). A special URL (http, https, ws, wss, ftp) is then held as
// that parser holds it: its host lowercased (ASCII), its port normalized — a
// default port dropped, leading zeros too — and an empty path read as "/".
//
// The parsers still differ elsewhere, and there the port reads as net/url
// (testdata/base-url tags each measured case): dot segments stay in the path,
// an IPv4 shorthand or a part past 255 is a host name, a pipe in the path is
// escaped, a malformed percent escape or a space in the userinfo is refused,
// and a query or fragment in the base URL stays where the string puts it.
func requestURL(rawURL string) (string, error) {
	u, err := url.Parse(whatwgURLInput(rawURL))
	if err != nil || u.Scheme == "" {
		return "", errInvalidURL
	}
	defaultPort, special := specialSchemePorts[u.Scheme]
	if !special {
		return u.String(), nil
	}
	host := u.Hostname()
	if host == "" {
		return "", errInvalidURL
	}
	host = asciiLower(host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > 65535 {
			return "", errInvalidURL
		}
		if n != defaultPort {
			host += ":" + strconv.Itoa(n)
		}
	}
	u.Host = host
	if u.Path == "" {
		u.Path = "/" // a special URL's path is never empty
	}
	// An empty username and password are no credentials, and serialize as
	// nothing (net/http would send them as Basic auth).
	if u.User != nil && u.User.Username() == "" {
		if password, _ := u.User.Password(); password == "" {
			u.User = nil
		}
	}
	return u.String(), nil
}

// fetchCredentialsError is the TypeError undici's fetch throws, sending
// nothing, for a request URL that includes credentials (a username or a
// password): "Request cannot be constructed from a URL that includes
// credentials: " and the URL's href.
type fetchCredentialsError struct{ href string }

func (e *fetchCredentialsError) Error() string {
	return "Request cannot be constructed from a URL that includes credentials: " + e.href
}

// fetchRefusal is the error undici's fetch refuses req with before it sends
// anything, or nil: a URL with credentials (fetchCredentialsError), which it
// throws as it is, and a port the fetch standard blocks (fetchBadPorts), for
// which it rejects with its "fetch failed" — here the *url.Error client.Do
// would fail with, which undiciFetchError reads as one, its cause undici's
// Error "bad port". It applies only to a request the port's own client sends;
// a custom HTTPClient, pi's custom fetch, takes whatever it is handed.
func fetchRefusal(req *http.Request) error {
	if req.URL.User != nil {
		return &fetchCredentialsError{href: req.URL.String()}
	}
	if port, err := strconv.Atoi(req.URL.Port()); err == nil && fetchBadPorts[port] {
		op := req.Method[:1] + strings.ToLower(req.Method[1:])
		return &url.Error{Op: op, URL: req.URL.String(), Err: errBadPort}
	}
	return nil
}

// errBadPort is the network error undici's fetch fails a request to a blocked
// port with.
var errBadPort = errors.New("bad port")

// fetchBadPorts are the ports the fetch standard's port blocking refuses,
// undici 6.29.1's list (lib/web/fetch/constants.js badPorts).
var fetchBadPorts = map[int]bool{
	1: true, 7: true, 9: true, 11: true, 13: true, 15: true, 17: true, 19: true, 20: true, 21: true,
	22: true, 23: true, 25: true, 37: true, 42: true, 43: true, 53: true, 69: true, 77: true, 79: true,
	87: true, 95: true, 101: true, 102: true, 103: true, 104: true, 109: true, 110: true, 111: true,
	113: true, 115: true, 117: true, 119: true, 123: true, 135: true, 137: true, 139: true, 143: true,
	161: true, 179: true, 389: true, 427: true, 465: true, 512: true, 513: true, 514: true, 515: true,
	526: true, 530: true, 531: true, 532: true, 540: true, 548: true, 554: true, 556: true, 563: true,
	587: true, 601: true, 636: true, 989: true, 990: true, 993: true, 995: true, 1719: true, 1720: true,
	1723: true, 2049: true, 3659: true, 4045: true, 4190: true, 5060: true, 5061: true, 6000: true,
	6566: true, 6665: true, 6666: true, 6667: true, 6668: true, 6669: true, 6679: true, 6697: true,
	10080: true,
}

// whatwgURLInput is rawURL as the WHATWG basic URL parser reads it before
// net/url can: leading and trailing C0 controls and spaces stripped, and
// every tab, LF and CR removed; for a special scheme but file, the run of
// slashes and backslashes after "scheme:" read as the authority's "//" and a
// backslash before the query or fragment as a path "/"; then any other C0
// control, or DEL, percent-encoded, as the parser encodes one wherever it may
// stand (in a scheme or host, where it may not, net/url refuses the escape).
func whatwgURLInput(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] <= ' ' {
		start++
	}
	for end > start && s[end-1] <= ' ' {
		end--
	}
	s = s[start:end]
	if strings.ContainsAny(s, "\t\n\r") {
		s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	}
	if scheme, rest, ok := urlScheme(s); ok {
		if _, special := specialSchemePorts[scheme]; special {
			rest = strings.TrimLeft(rest, `/\`)
			end := strings.IndexAny(rest, "?#")
			if end < 0 {
				end = len(rest)
			}
			rest = strings.ReplaceAll(rest[:end], `\`, "/") + rest[end:]
			s = scheme + "://" + rest
		}
	}
	return percentEncodeControls(s)
}

// urlScheme splits s at the end of a leading URL scheme — an ASCII letter,
// then letters, digits, "+", "-" and "." up to a ":" — lowercased, and
// reports whether it has one.
func urlScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return asciiLower(s[:i]), s[i+1:], true
		default:
			return "", s, false
		}
	}
	return "", s, false
}

// percentEncodeControls percent-encodes each C0 control and DEL in s, with
// uppercase hex digits as the WHATWG parser writes them.
func percentEncodeControls(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			b.WriteString(s[last:i])
			fmt.Fprintf(&b, "%%%02X", c)
			last = i + 1
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// asciiLower lowercases the ASCII letters of s, as the WHATWG host parser
// does; anything else it leaves as it is.
func asciiLower(s string) string {
	i := strings.IndexFunc(s, func(r rune) bool { return 'A' <= r && r <= 'Z' })
	if i < 0 {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if 'A' <= b[i] && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
