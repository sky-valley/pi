package env

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// How much stderr a startup error shows: short, because a tool error can
// reach the model.
const (
	stderrTailChars = 2000
	stderrTailLines = 20
)

// stderrTail is the end of a starting daemon's stderr: its last
// stderrTailChars UTF-16 code units, as upstream's
// (tail + text).slice(-STDERR_TAIL_CHARS) keeps them, so a cut through a
// surrogate pair keeps the low half of it.
type stderrTail []uint16

func (t *stderrTail) add(text string) {
	*t = append(*t, jstext.CodeUnits(text)...)
	if extra := len(*t) - stderrTailChars; extra > 0 {
		*t = append((*t)[:0], (*t)[extra:]...)
	}
}

// String is the tail, half a surrogate pair as its WTF-8 triple.
func (t stderrTail) String() string { return jstext.FromCodeUnits(t) }

// withStderr is message plus the last lines of stderr. The remote machine
// wrote that text, so terminal escapes and control characters are removed:
// line ends become "\n", VT control sequences go (stripVTControlCharacters),
// and so do C0 and C1 controls other than "\n" and "\t"; lines lose their
// trailing whitespace (JS trimEnd), and empty lines go.
func withStderr(message, stderr string) string {
	stderr = stripVTControlCharacters(lineEnds.ReplaceAllString(stderr, "\n"))
	var text strings.Builder
	for i := 0; i < len(stderr); {
		// Half a surrogate pair (a cut tail starts with one) is a code unit
		// above the controls: kept.
		if _, ok := jstext.WTF8Surrogate(stderr[i:]); ok {
			text.WriteString(stderr[i : i+3])
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(stderr[i:])
		if r == '\n' || r == '\t' || (r >= 0x20 && (r < 0x7f || r > 0x9f)) {
			text.WriteString(stderr[i : i+size])
		}
		i += size
	}
	var lines []string
	for line := range strings.SplitSeq(text.String(), "\n") {
		if line = jstext.TrimEnd(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return message
	}
	return message + "\nremote stderr:\n" + strings.Join(lines[max(0, len(lines)-stderrTailLines):], "\n")
}

var lineEnds = regexp.MustCompile(`\r\n?`)

// vtControlSequence is node's ansi pattern (lib/internal/util/inspect.js, at
// node v26.4.0), which util.stripVTControlCharacters removes:
//
//	'[\\u001B\\u009B][[\\]()#;?]*' +
//	'(?:(?:(?:(?:;[-a-zA-Z\\d\\/\\#&.:=?%@~_]+)*' +
//	'|[a-zA-Z\\d]+(?:;[-a-zA-Z\\d\\/\\#&.:=?%@~_]*)*)?' +
//	'(?:\\u0007|\\u001B\\u005C|\\u009C))' +
//	'|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?' +
//	'[\\dA-PR-TZcf-nq-uy=><~]))'
//
// Its classes are ASCII and three C1 characters, and it has neither
// backreferences nor lookaround, so Go's leftmost-first matching makes the
// matches JavaScript's backtracking does (testdata/stderr pins it against
// node).
var vtControlSequence = regexp.MustCompile(`[\x1b\x{9b}][\[\]()#;?]*` +
	`(?:(?:(?:(?:;[-a-zA-Z\d/#&.:=?%@~_]+)*` +
	`|[a-zA-Z\d]+(?:;[-a-zA-Z\d/#&.:=?%@~_]*)*)?` +
	`(?:\x07|\x1b\\|\x{9c}))` +
	`|(?:(?:\d{1,4}(?:;\d{0,4})*)?` +
	`[\dA-PR-TZcf-nq-uy=><~]))`)

// stripVTControlCharacters is node's util.stripVTControlCharacters.
func stripVTControlCharacters(s string) string {
	if !strings.Contains(s, "\x1b") && !strings.Contains(s, "\u009b") {
		return s
	}
	return vtControlSequence.ReplaceAllString(s, "")
}

// stderrDecoder decodes a stream of stderr chunks as node's
// readable.setEncoding("utf8") does: a character split between chunks comes
// whole with the later one, invalid UTF-8 is U+FFFD per maximal subpart
// (jstext.DecodeUTF8), and end makes a sequence the stream left incomplete
// U+FFFD. Unlike a TextDecoder it keeps a leading byte-order mark.
type stderrDecoder struct {
	held []byte
}

func (d *stderrDecoder) decode(b []byte) string {
	if len(d.held) > 0 {
		b = append(d.held, b...)
		d.held = nil
	}
	cut := len(b) - incompleteSuffix(b)
	if cut < len(b) {
		d.held = append([]byte(nil), b[cut:]...)
	}
	return jstext.DecodeUTF8(b[:cut])
}

func (d *stderrDecoder) end() string {
	text := jstext.DecodeUTF8(d.held)
	d.held = nil
	return text
}

// incompleteSuffix is how many bytes at the end of b node's StringDecoder
// (src/string_decoder.cc) holds for the next chunk: those of a character its
// lead byte says is longer, read from the lead's upper bits alone. It may
// hold bytes no later byte makes valid, which only moves where their U+FFFD
// comes out: decoded with the bytes that follow, or alone before them when a
// byte that is not a continuation follows, the text is the same.
func incompleteSuffix(b []byte) int {
	if len(b) == 0 || b[len(b)-1]&0x80 == 0 {
		return 0
	}
	buffered := 0
	for i := len(b) - 1; ; i-- {
		buffered++
		if b[i]&0xc0 == 0x80 {
			// A continuation byte: its lead is further back, unless it is
			// further than a character reaches or not in b.
			if buffered >= 4 || i == 0 {
				return 0
			}
			continue
		}
		var length int
		switch {
		case b[i]&0xe0 == 0xc0:
			length = 2
		case b[i]&0xf0 == 0xe0:
			length = 3
		case b[i]&0xf8 == 0xf0:
			length = 4
		default:
			return 0
		}
		if buffered >= length {
			return 0
		}
		return buffered
	}
}
