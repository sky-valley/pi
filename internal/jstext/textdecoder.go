package jstext

import "strings"

// TextDecoder is a TextDecoder("utf-8") decoding a stream one call at a time,
// as the WHATWG Encoding Standard's UTF-8 decoder does in replacement mode: one
// byte-order mark before the stream's first character is dropped, however the
// calls split it; invalid UTF-8 becomes one U+FFFD per maximal subpart
// (DecodeUTF8); and a sequence a call leaves incomplete is held for the next.
// The zero value is ready to use.
type TextDecoder struct {
	// IgnoreBOM is the constructor's {ignoreBOM: true}: a leading byte-order
	// mark is kept as text, as Buffer.toString("utf-8") keeps it.
	IgnoreBOM bool

	held []byte
	// started is whether the stream's first character has been decoded,
	// after which no byte-order mark is dropped.
	started bool
}

// Decode is decode(b, {stream: true}): the text b decodes to, given the calls
// before it.
func (d *TextDecoder) Decode(b []byte) string {
	if len(d.held) > 0 {
		b = append(d.held, b...)
		d.held = nil
	}
	cut := len(b) - incompleteUTF8Suffix(b)
	text := DecodeUTF8(b[:cut])
	if cut < len(b) {
		d.held = append([]byte(nil), b[cut:]...)
	}
	return d.emit(text)
}

// Flush is decode() with no input, which ends the stream: the bytes still
// held become U+FFFD, and the decoder starts over. A decoder never flushed
// drops them.
func (d *TextDecoder) Flush() string {
	text := d.emit(DecodeUTF8(d.held))
	*d = TextDecoder{IgnoreBOM: d.IgnoreBOM}
	return text
}

// emit drops the byte-order mark the stream's first character may be.
func (d *TextDecoder) emit(text string) string {
	if !d.started && text != "" {
		d.started = true
		if !d.IgnoreBOM {
			text = strings.TrimPrefix(text, byteOrderMark)
		}
	}
	return text
}

// DecodeText is what one decode(b) of a fresh TextDecoder("utf-8") returns:
// the stream's text, a leading byte-order mark dropped, and an incomplete
// final sequence as U+FFFD. A single decode(b, {stream: true}) is a fresh
// TextDecoder's Decode(b).
func DecodeText(b []byte) string {
	var d TextDecoder
	return d.Decode(b) + d.Flush()
}

// byteOrderMark is U+FEFF in UTF-8, which a TextDecoder drops from the start
// of its text.
const byteOrderMark = "\xef\xbb\xbf"
