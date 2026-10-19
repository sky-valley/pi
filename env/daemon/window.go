package daemon

import (
	"math"
	"strings"
	"time"
)

// Durable's output window (ShellExecOptions.window, ShellOutputInfo.skipped):
// undelivered command output is held and coalesced; text followed by more
// than the window (by a byte or a line) can never be in the caller's tail and
// is replaced by counts. Delivery follows the caller's commit pace, and waits
// while an earlier frame is still unsent.

type window struct {
	maxBytes       int
	maxLines       int
	minInterval    time.Duration
	bytesPerSecond float64
}

// windowFromJSON reads `{ maxBytes, maxLines, minIntervalMs, bytesPerSecond }`;
// any field missing or of the wrong type means no window.
func windowFromJSON(v any) (window, bool) {
	o, ok := asObject(v)
	if !ok {
		return window{}, false
	}
	maxBytes, ok1 := asU64(o["maxBytes"])
	maxLines, ok2 := asU64(o["maxLines"])
	interval, ok3 := asF64(o["minIntervalMs"])
	rate, ok4 := asF64(o["bytesPerSecond"])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return window{}, false
	}
	return window{
		maxBytes:       int(maxBytes),
		maxLines:       int(maxLines),
		minInterval:    secondsDuration(math.Max(interval, 0) / 1000),
		bytesPerSecond: math.Max(rate, 1),
	}, true
}

// secondsDuration is Rust's Duration::from_secs_f64 for a non-negative,
// finite number of seconds, saturating where Duration cannot represent it.
func secondsDuration(seconds float64) time.Duration {
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds * float64(time.Second))
}

type skipped struct {
	bytes           uint64
	newlines        uint64
	endsWithNewline bool
}

// outputEvent is one output event: its stream, text, and the output omitted
// right before it.
type outputEvent struct {
	stream  string
	text    string
	skipped object
}

type segment struct {
	stream string
	text   strings.Builder
}

type pending struct {
	window window
	// segments is undelivered text in arrival order, by stream.
	segments []*segment
	bytes    int
	newlines int
	skipped  *skipped
	nextSend time.Time
}

func newlineCount(text string) int { return strings.Count(text, "\n") }

func newPending(w window) *pending {
	return &pending{window: w, nextSend: time.Now()}
}

func (p *pending) isEmpty() bool { return len(p.segments) == 0 }

func (p *pending) push(stream, text string) {
	if text == "" {
		return
	}
	p.bytes += len(text)
	p.newlines += newlineCount(text)
	if n := len(p.segments); n > 0 && p.segments[n-1].stream == stream {
		p.segments[n-1].text.WriteString(text)
	} else {
		s := &segment{stream: stream}
		s.text.WriteString(text)
		p.segments = append(p.segments, s)
	}
	// Trimming costs a pass over the held text; do it once a few windows have
	// accumulated.
	if p.bytes > 4*(p.window.maxBytes+1) || p.newlines > 4*(p.window.maxLines+1) {
		p.trim()
	}
}

// proves reports whether the text after a cut still proves the cut text is
// outside the window.
func (p *pending) proves(bytes, newlines int) bool {
	return bytes > p.window.maxBytes || newlines > p.window.maxLines
}

// trim replaces the longest prefix whose remainder still proves exclusion by
// counts.
func (p *pending) trim() {
	var removed strings.Builder
	// Whole segments first.
	for len(p.segments) > 1 {
		first := p.segments[0].text.String()
		bytes, newlines := len(first), newlineCount(first)
		if !p.proves(p.bytes-bytes, p.newlines-newlines) {
			break
		}
		p.segments = p.segments[1:]
		p.bytes -= bytes
		p.newlines -= newlines
		removed.WriteString(first)
	}
	// Then a prefix of the first remaining segment, at a character boundary.
	first := p.segments[0].text.String()
	restBytes := p.bytes - len(first)
	restNewlines := p.newlines - newlineCount(first)
	cut := 0
	// By bytes: keep more than maxBytes overall.
	if keepBytes := max(p.window.maxBytes+1-restBytes, 0); len(first) > keepBytes {
		index := len(first) - keepBytes
		for index > 0 && !isCharBoundary(first, index) {
			index--
		}
		cut = max(cut, index)
	}
	// By lines: keep more than maxLines newlines overall, starting at a newline
	// of this segment.
	if keepNewlines := max(p.window.maxLines+1-restNewlines, 0); keepNewlines > 0 {
		var positions []int
		for i := 0; i < len(first); i++ {
			if first[i] == '\n' {
				positions = append(positions, i)
			}
		}
		if len(positions) >= keepNewlines {
			cut = max(cut, positions[len(positions)-keepNewlines])
		}
	} else {
		cut = len(first)
	}
	if cut > 0 {
		prefix := first[:cut]
		rest := first[cut:]
		p.bytes -= len(prefix)
		p.newlines -= newlineCount(prefix)
		removed.WriteString(prefix)
		if rest == "" {
			p.segments = p.segments[1:]
		} else {
			p.segments[0].text.Reset()
			p.segments[0].text.WriteString(rest)
		}
	}
	if removed.Len() > 0 {
		text := removed.String()
		if p.skipped == nil {
			p.skipped = &skipped{}
		}
		p.skipped.bytes += uint64(len(text))
		p.skipped.newlines += uint64(newlineCount(text))
		p.skipped.endsWithNewline = strings.HasSuffix(text, "\n")
	}
}

// isCharBoundary is Rust's str::is_char_boundary.
func isCharBoundary(s string, i int) bool {
	return i == 0 || i >= len(s) || s[i] < 0x80 || s[i] >= 0xC0
}

// take delivers everything held: one event carrying the skip and all text
// after it, or one event per segment.
func (p *pending) take() []outputEvent {
	segments := p.segments
	delivered := p.bytes
	p.segments = nil
	p.bytes = 0
	p.newlines = 0
	// The caller writes at most the window per sample, and paces itself by
	// what it wrote.
	pace := secondsDuration(float64(min(delivered, p.window.maxBytes)) / p.window.bytesPerSecond)
	p.nextSend = time.Now().Add(max(p.window.minInterval, pace))
	if p.skipped != nil {
		s := p.skipped
		p.skipped = nil
		stream := "stdout"
		if len(segments) > 0 {
			stream = segments[len(segments)-1].stream
		}
		var text strings.Builder
		for _, segment := range segments {
			text.WriteString(segment.text.String())
		}
		return []outputEvent{{
			stream: stream,
			text:   text.String(),
			skipped: object{
				"bytes":           s.bytes,
				"newlines":        s.newlines,
				"endsWithNewline": s.endsWithNewline,
			},
		}}
	}
	events := make([]outputEvent, 0, len(segments))
	for _, segment := range segments {
		events = append(events, outputEvent{stream: segment.stream, text: segment.text.String()})
	}
	return events
}
