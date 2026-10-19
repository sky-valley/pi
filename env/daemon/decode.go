package daemon

import (
	"strings"
	"unicode/utf8"
)

// WHATWG UTF-8 decoding that matches Node's `new TextDecoder().decode(bytes)`
// of a whole input when the input arrives in pieces (Durable env/decode.ts):
// invalid sequences become U+FFFD by the maximal-subpart rule, and only a
// byte-order mark at the very start is dropped.
//
// The root module's internal/jstext implements the same decoder, but it
// imports golang.org/x/text, which the daemon's dependency budget (stdlib,
// x/sys, fsnotify) does not admit; the few functions needed live here.

const byteOrderMark = "\xef\xbb\xbf"

// decoder decodes one stream chunk by chunk, holding back an incomplete
// character for the next chunk. With dropBOM, a byte-order mark that starts
// the stream is dropped (a stream decoder); without it, nothing is (a range
// decoder, for a byte range that does not start its input).
type decoder struct {
	held    []byte
	dropBOM bool
	started bool
}

// newStreamDecoder decodes a stream, dropping a leading byte-order mark. It
// removes a UTF-8 mark only and never switches to UTF-16 on one.
func newStreamDecoder() *decoder { return &decoder{dropBOM: true} }

// newRangeDecoder decodes a byte range from inside its input.
func newRangeDecoder() *decoder { return &decoder{} }

// decode is the text of b given the bytes before it; with last, it ends the
// stream and a held incomplete sequence becomes U+FFFD.
func (d *decoder) decode(b []byte, last bool) string {
	if len(d.held) > 0 {
		b = append(d.held, b...)
		d.held = nil
	}
	cut := len(b)
	if !last {
		cut -= incompleteSuffix(b)
	}
	text := decodeUTF8(b[:cut])
	if cut < len(b) {
		d.held = append([]byte(nil), b[cut:]...)
	}
	if !d.started && text != "" {
		d.started = true
		if d.dropBOM {
			text = strings.TrimPrefix(text, byteOrderMark)
		}
	}
	return text
}

// startsWithBOM reports whether decoding the whole input drops its first
// three bytes as a byte-order mark.
func startsWithBOM(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf
}

// decodeUTF8 is the Encoding Standard's UTF-8 decoder in replacement mode:
// valid UTF-8 comes through as is, and an invalid sequence becomes one U+FFFD
// per maximal subpart. It is also Rust's String::from_utf8_lossy, which the
// Rust daemon uses for names. Go's own decoders replace each invalid byte
// (utf8.DecodeRune) or each run of them (strings.ToValidUTF8) instead.
func decodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	out.Grow(len(b) + 8)
	for i := 0; i < len(b); {
		lead := b[i]
		if lead < 0x80 {
			out.WriteByte(lead)
			i++
			continue
		}
		needed, lower, upper := utf8Lead(lead)
		if needed == 0 {
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		end := i + 1
		for end <= i+needed && end < len(b) && b[end] >= lower && b[end] <= upper {
			lower, upper = 0x80, 0xBF
			end++
		}
		if end == i+needed+1 {
			out.Write(b[i:end])
		} else {
			out.WriteRune(utf8.RuneError)
		}
		i = end
	}
	return out.String()
}

// utf8Lead is how the decoder reads a lead byte of 0x80 or more: how many
// continuation bytes follow, and the bounds of the first of them (which rule
// out overlong forms, surrogates and code points past U+10FFFF). needed is 0
// for a byte no sequence starts with.
func utf8Lead(lead byte) (needed int, lower, upper byte) {
	lower, upper = 0x80, 0xBF
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		needed = 1
	case lead >= 0xE0 && lead <= 0xEF:
		needed = 2
		if lead == 0xE0 {
			lower = 0xA0
		} else if lead == 0xED {
			upper = 0x9F
		}
	case lead >= 0xF0 && lead <= 0xF4:
		needed = 3
		if lead == 0xF0 {
			lower = 0x90
		} else if lead == 0xF4 {
			upper = 0x8F
		}
	}
	return needed, lower, upper
}

// incompleteSuffix is the length of the incomplete sequence b ends with, which
// later bytes could still complete; 0 when b ends where a character ends or
// on bytes no later byte could make valid.
func incompleteSuffix(b []byte) int {
	for k := 1; k <= 3 && k <= len(b); k++ {
		lead := b[len(b)-k]
		if lead < 0x80 {
			return 0
		}
		if lead < 0xC0 {
			continue
		}
		needed, lower, upper := utf8Lead(lead)
		if needed == 0 || k-1 >= needed {
			return 0
		}
		for _, c := range b[len(b)-k+1:] {
			if c < lower || c > upper {
				return 0
			}
			lower, upper = 0x80, 0xBF
		}
		return k
	}
	return 0
}

// compareUTF16 orders strings by UTF-16 code units, JavaScript's default sort.
func compareUTF16(a, b string) int {
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			ua, ub := ra, rb
			if ua >= 0x10000 {
				ua = 0xD800 + (ua-0x10000)>>10
			}
			if ub >= 0x10000 {
				ub = 0xD800 + (ub-0x10000)>>10
			}
			if ua != ub {
				return int(ua) - int(ub)
			}
			return int(ra) - int(rb)
		}
		a, b = a[na:], b[nb:]
	}
	return len(a) - len(b)
}
