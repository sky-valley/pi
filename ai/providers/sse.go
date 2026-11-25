package providers

import (
	"bytes"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// serverSentEvent is pi's ServerSentEvent (utils/sse.ts): its `event` field
// ("" for pi's null, which an empty `event:` is too), its `data` lines joined
// with "\n", and its raw lines for diagnostics.
type serverSentEvent struct {
	event string
	data  string
	raw   []string
}

// sseDecoder is pi's SseDecoder (utils/sse.ts, upstream da7e26cc8), shared by
// the anthropic-messages and pi-messages readers. Lines end with LF, CRLF or
// CR — a CRLF split across two pushes included — and a blank line ends an
// event. Only new bytes are scanned and an incomplete line is copied once per
// push, so an event arriving in many chunks costs time linear in its size.
//
// It reads the body as pi's TextDecoder makes text of it: a byte-order mark
// at the very start is dropped, and invalid UTF-8 becomes U+FFFD per maximal
// subpart (jstext.DecodeUTF8). A line break never belongs to a multi-byte
// sequence, so decoding each line is decoding the body.
//
// maxLine, when positive, is the port's own bound on a line (pi has none):
// push fails with tooLong once a line, ended or not, is longer.
type sseDecoder struct {
	maxLine int
	tooLong error

	// pending is the incomplete last line.
	pending []byte
	// skipLF: the last push ended with CR, so a leading LF of the next one
	// completes that CRLF.
	skipLF  bool
	started bool // a line was decoded, so no byte-order mark can follow
	event   string
	data    []string
	raw     []string
}

// push takes the next bytes and hands emit each event they complete, in
// order; emit's error stops the push and is returned.
func (d *sseDecoder) push(chunk []byte, emit func(serverSentEvent) error) error {
	start := 0
	if d.skipLF && len(chunk) > 0 && chunk[0] == '\n' {
		start = 1
	}
	if len(chunk) > 0 {
		d.skipLF = false
	}
	for {
		i := bytes.IndexAny(chunk[start:], "\r\n")
		if i < 0 {
			break
		}
		end := start + i
		line := chunk[start:end]
		if len(d.pending) > 0 {
			line = append(d.pending, line...)
		}
		if d.maxLine > 0 && len(line) > d.maxLine {
			return d.tooLong
		}
		d.pending = d.pending[:0]
		if err := d.decodeLine(line, emit); err != nil {
			return err
		}
		start = end + 1
		if chunk[end] == '\r' {
			if start == len(chunk) {
				d.skipLF = true
			} else if chunk[start] == '\n' {
				start++
			}
		}
	}
	d.pending = append(d.pending, chunk[start:]...)
	if d.maxLine > 0 && len(d.pending) > d.maxLine {
		return d.tooLong
	}
	return nil
}

// end ends the body and hands emit the remaining events, including a last
// one the body ended without a blank line.
func (d *sseDecoder) end(emit func(serverSentEvent) error) error {
	if len(d.pending) > 0 {
		line := d.pending
		d.pending = nil
		if err := d.decodeLine(line, emit); err != nil {
			return err
		}
	}
	d.skipLF = false
	return d.flush(emit)
}

func (d *sseDecoder) decodeLine(b []byte, emit func(serverSentEvent) error) error {
	if !d.started {
		b, d.started = stripBOM(b), true
	}
	if len(b) == 0 {
		return d.flush(emit)
	}
	line := jstext.DecodeUTF8(b)
	d.raw = append(d.raw, line)
	if strings.HasPrefix(line, ":") {
		return nil
	}
	field, value, found := strings.Cut(line, ":")
	if found {
		value = strings.TrimPrefix(value, " ")
	}
	switch field {
	case "event":
		d.event = value
	case "data":
		d.data = append(d.data, value)
	}
	return nil
}

// flush ends the event so far: one with neither an event name nor data is
// dropped, its raw lines with it.
func (d *sseDecoder) flush(emit func(serverSentEvent) error) error {
	ev := serverSentEvent{event: d.event, raw: d.raw}
	hasData := len(d.data) > 0
	if hasData {
		ev.data = strings.Join(d.data, "\n")
	}
	d.event, d.data, d.raw = "", nil, nil
	if ev.event == "" && !hasData {
		return nil
	}
	return emit(ev)
}
