package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// This file ports how pi's two openai adapters read a streamed response: the
// openai SDK's Stream.fromSSEResponse over _iterSSEMessages (openai 7.19.0,
// core/streaming.js and internal/decoders/line.js — the version pi-ai's
// package-lock.json entry locks, nested under packages/ai since ab30693d6).
// Both adapters iterate the SDK's Stream, so both Go loops read their body
// through iterateOpenAIStream.

// openaiSSEEvent is one server-sent event as the SDK's SSEDecoder dispatches
// it. An absent event name and an empty one read alike everywhere the SDK looks
// at it.
type openaiSSEEvent struct {
	event string
	data  string
}

// utf8BOM is the byte-order mark the SDK's per-line UTF-8 decode drops.
const utf8BOM = "\xef\xbb\xbf"

// readOpenAISSE is the SDK's _iterSSEMessages. The body reaches its line
// splitting in iterSSEChunks' pieces (openaiSSEChunkReader), each read raced
// against ctx (openaiAbortableBody). Lines end at "\n", "\r\n" or a lone "\r"
// (openaiSSELines), and each line is decoded on its own by a TextDecoder
// (jstext.DecodeUTF8), which writes one U+FFFD per maximal subpart of an
// invalid sequence and drops one leading byte-order mark. A blank line
// dispatches the event collected so far, unless it has neither a name nor any
// data; `data` lines join with "\n"; a line starting with ":" is a comment;
// every other field (id, retry, …) is ignored. When the body ends, an event it
// ended inside is dispatched too (SSEDecoder.flush).
//
// A field's value is everything after the line's first colon less exactly one
// leading space. Nothing else is trimmed, and JSON.parse accepts only JSON's
// own whitespace around a value, so data padded with anything else (U+00A0,
// U+FEFF, U+0085, VT, FF) does not parse.
//
// A line longer than maxOpenAISSELine fails the reading, a port limit.
//
// The SDK reads with the request's signal in hand, and ctx is that signal.
// Once it is done, nothing more is dispatched — not even what has already been
// read, since the SDK checks the signal before every line and before its final
// flush — and the reading ends without an error, a read still waiting on the
// body included unless it fails within openaiAbandonedReadGrace
// (openaiAbortableBody, docs/UPSTREAM.md D88); pi's adapter then runs its
// post-loop checks. A read that fails with an abort ends the reading the same way, since
// the SDK's Stream swallows a transport abort: undici's AbortError, which the
// port's own client's body fails with once ctx is done (fetchBody), and
// context.Canceled from a custom client's body, the Go stand-in for the
// AbortError a custom fetch's body throws. Any other failed read ends the
// reading with its error, ctx done or not; the LineDecoder is flushed only at
// the body's end, so what iterSSEChunks held back past the last event
// separator is lost.
func readOpenAISSE(body io.Reader, ctx context.Context, dispatch func(openaiSSEEvent) error) error {
	chunks := &openaiSSEChunkReader{body: &progressReader{r: newOpenAIAbortableBody(ctx, body), provider: "openai"}}
	var lines openaiSSELines
	scanner := bufio.NewScanner(chunks)
	scanner.Buffer(make([]byte, 0, 64*1024), maxOpenAISSELine)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		return lines.split(data, atEOF && chunks.err == io.EOF)
	})
	aborted := func() bool { return ctx != nil && ctx.Err() != nil }
	var event string
	var data []string
	for scanner.Scan() {
		if aborted() {
			return nil
		}
		line := strings.TrimPrefix(jstext.DecodeUTF8(scanner.Bytes()), utf8BOM)
		if line == "" {
			if event == "" && len(data) == 0 {
				continue
			}
			sse := openaiSSEEvent{event: event, data: strings.Join(data, "\n")}
			event, data = "", nil
			if err := dispatch(sse); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, errOperationAborted) || errors.Is(err, context.Canceled) {
			return nil
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("an openai stream line is longer than the port's %d MiB limit (pi reads a line of any length); this is a port limit, report it with the provider and model: %w", maxOpenAISSELine>>20, err)
		}
		return err
	}
	if aborted() || (event == "" && len(data) == 0) {
		return nil
	}
	return dispatch(openaiSSEEvent{event: event, data: strings.Join(data, "\n")})
}

// maxOpenAISSELine is the longest line readOpenAISSE reads. The SDK's
// LineDecoder has no limit; a longer line fails the stream with the port's
// own error.
const maxOpenAISSELine = 16 << 20

// openaiAbortableBody is a body as the SDK's createAbortableSSESource reads
// it: each read raced against ctx. Once ctx is done, every read fails with
// undici's AbortError at once, and a read still waiting on the body is given
// openaiAbandonedReadGrace to finish. What it returns by then is the read's,
// so a body that fails in that window fails the stream with its own error
// (context.Canceled, an abort, aside); past it the read is abandoned and
// fails with the AbortError, so a body that ignores the cancellation still
// ends the stream. An abandoned read finishes in the background into a buffer
// of its own and what it returns is dropped; one that never returns keeps its
// goroutine for good. The adapter closes the body once its stream has ended,
// which can be while an abandoned read is still in the body's Read; the SDK
// cancels its reader at the abort.
//
// The window is the port's (docs/UPSTREAM.md D88). The SDK settles its race
// by JavaScript's event order: a custom fetch's body keeps its own error only
// if it errors its stream before the microtask the SDK's abort listener
// queues — at once from an abort listener, or a microtask later from one
// that fires ahead of the SDK's — and one that fails any later, even after
// setTimeout(0), ends pi's stream as an abort. Go has no such order: a body
// answers ctx on a goroutine of its own.
type openaiAbortableBody struct {
	ctx  context.Context
	body io.Reader
	// buf is what each read fills before it is copied out; a read that ctx
	// abandoned keeps it, and no read follows that one.
	buf []byte
}

// openaiAbandonedReadGrace is how long a read still waiting on the body when
// ctx ends is given to finish (see openaiAbortableBody): long enough for a
// body that fails in reaction to the cancellation to keep its error, as
// stream-abort's `a custom fetch's body fails once the signal aborts` row
// needs, and short enough that one that ignores the cancellation ends the
// stream promptly. pi keeps such an error only when it comes ahead of the
// SDK's abort microtask, and ends the stream at once for a body that ignores
// the abort (docs/UPSTREAM.md D88).
const openaiAbandonedReadGrace = 100 * time.Millisecond

// maxAbortableRead bounds one raced read, and with it the buffer an
// openaiAbortableBody keeps.
const maxAbortableRead = 32 << 10

// newOpenAIAbortableBody races body's reads against ctx, unless ctx can never
// be done or body is the port's own client's (fetchBody), whose read already
// fails at once when ctx ends.
func newOpenAIAbortableBody(ctx context.Context, body io.Reader) io.Reader {
	if ctx == nil || ctx.Done() == nil {
		return body
	}
	if _, own := body.(fetchBody); own {
		return body
	}
	return &openaiAbortableBody{ctx: ctx, body: body}
}

func (b *openaiAbortableBody) Read(p []byte) (int, error) {
	if b.ctx.Err() != nil {
		return 0, errOperationAborted
	}
	p = p[:min(len(p), maxAbortableRead)]
	if cap(b.buf) < len(p) {
		b.buf = make([]byte, len(p))
	}
	buf := b.buf[:len(p)]
	type result struct {
		n   int
		err error
	}
	read := make(chan result, 1)
	go func() {
		n, err := b.body.Read(buf)
		read <- result{n, err}
	}()
	select {
	case r := <-read:
		return copy(p, buf[:r.n]), r.err
	case <-b.ctx.Done():
	}
	grace := time.NewTimer(openaiAbandonedReadGrace)
	defer grace.Stop()
	select {
	case r := <-read:
		return copy(p, buf[:r.n]), r.err
	case <-grace.C:
		return 0, errOperationAborted
	}
}

// openaiSSEChunkReader is the SDK's iterSSEChunks: it passes the body on only
// up to the end of its last event separator (openaiSSEChunkEnd), holding the
// rest back until more arrives or the body ends. What is held back when a read
// fails is lost, as it is in the SDK.
type openaiSSEChunkReader struct {
	// body is the response body, guarded against reads that never progress
	// (progressReader).
	body io.Reader
	// buf is the read-ahead: buf[:ready] is passed on, buf[ready:] is held.
	buf   []byte
	ready int
	// err is the read error that ended the body; io.EOF at its end.
	err error
}

func (c *openaiSSEChunkReader) Read(p []byte) (int, error) {
	for c.ready == 0 {
		if c.err != nil {
			return 0, c.err
		}
		if cap(c.buf)-len(c.buf) < 4096 {
			c.buf = append(c.buf, make([]byte, 32*1024)...)[:len(c.buf)]
		}
		scanned := max(0, len(c.buf)-3) // a separator ends past what was scanned
		n, err := c.body.Read(c.buf[len(c.buf):cap(c.buf)])
		c.buf = c.buf[:len(c.buf)+n]
		for at := scanned; ; {
			end := openaiSSEChunkEnd(c.buf[at:])
			if end < 0 {
				break
			}
			at += end
			c.ready = at
		}
		if err != nil {
			c.err = err
			if err == io.EOF {
				c.ready = len(c.buf)
			} else {
				c.buf = c.buf[:c.ready]
			}
		}
	}
	n := copy(p, c.buf[:c.ready])
	c.buf = c.buf[:copy(c.buf, c.buf[n:])]
	c.ready -= n
	return n, nil
}

// openaiSSEChunkEnd is the SDK's findDoubleNewlineIndex: the index just past
// the first two line endings in a row in b, each "\n", "\r\n" or "\r", or -1.
// A "\r" that ends b counts as a whole line ending: the "\n" that may follow
// it in the next read then begins the next piece, where openaiSSELines skips
// it.
func openaiSSEChunkEnd(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if first := sseLineEnding(b, i); first > 0 {
			if second := sseLineEnding(b, i+first); second > 0 {
				return i + first + second
			}
		}
	}
	return -1
}

// sseLineEnding is the length of the line ending at b[i]: 2 for "\r\n", 1 for
// another "\r" or a "\n", 0 for anything else and past b's end.
func sseLineEnding(b []byte, i int) int {
	switch {
	case i >= len(b):
		return 0
	case b[i] == '\n':
		return 1
	case b[i] != '\r':
		return 0
	case i+1 < len(b) && b[i+1] == '\n':
		return 2
	}
	return 1
}

// openaiSSELines splits lines the way the SDK's LineDecoder does: "\n", "\r\n"
// or a lone "\r" ends a line. A "\r" ends its line at once, even at the end of
// what has been read so far; a "\n" that then begins the next read is the
// second byte of that ending, and is skipped. So no line keeps a trailing
// "\r", which is why SSEDecoder's own strip of one never applies here. The
// body's last line needs no ending (LineDecoder.flush).
type openaiSSELines struct {
	// skipLF is set when a "\r" ended the data read so far.
	skipLF bool
}

// split is a bufio.SplitFunc; atEOF is the body's end.
func (s *openaiSSELines) split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if s.skipLF && len(data) > 0 {
		s.skipLF = false
		if data[0] == '\n' {
			return 1, nil, nil
		}
	}
	i := bytes.IndexAny(data, "\r\n")
	switch {
	case i < 0 && atEOF && len(data) > 0:
		return len(data), data, nil
	case i < 0:
		return 0, nil, nil
	case data[i] == '\n':
		return i + 1, data[:i], nil
	case i+1 == len(data):
		s.skipLF = true
		return i + 1, data[:i], nil
	case data[i+1] == '\n':
		return i + 2, data[:i], nil
	}
	return i + 1, data[:i], nil
}

// openaiStreamItem is one item the SDK's Stream yields.
type openaiStreamItem struct {
	// value is what JSON.parse made of the item, in ai.DecodeOrderedValue's
	// shapes: the value pi's loops read, with jsvalue.go's JavaScript
	// semantics, and the value OnProviderStreamEvent receives.
	value any
	// text is the item's JSON text.
	text []byte
}

// keepingSurrogates is the item as JSON.parse makes it with half a surrogate
// pair kept (ai.DecodeOrderedValueKeepingSurrogates) — what an adapter reads
// a tool call's argument deltas from, so a pair cut between two deltas joins
// again in the arguments, as in pi. It is value itself when the text escapes
// no surrogate, the common case, which costs no second decode.
func (item openaiStreamItem) keepingSurrogates() any {
	if !bytes.Contains(item.text, []byte(`\ud`)) && !bytes.Contains(item.text, []byte(`\uD`)) {
		return item.value
	}
	if value, err := ai.DecodeOrderedValueKeepingSurrogates(item.text); err == nil {
		return value
	}
	return item.value
}

// iterateOpenAIStream is the SDK's Stream.fromSSEResponse iterator over
// readOpenAISSE: it hands yield each item the SDK yields, decoded once.
//
//   - An event whose data is exactly "[DONE]" ends the stream, and nothing
//     after it is read: the SDK breaks out of its loop and cancels the body, so
//     neither a later event nor a failure of the body is seen.
//   - Data is JSON.parse'd (openaiStreamJSON). Data it rejects fails the stream
//     with the SDK's own SyntaxError, errOpenAIStreamMalformedJSON, whatever
//     JSON.parse's reason; nothing is repaired, and the event is not yielded.
//   - An event named "thread.*" is yielded as {"event": name, "data": value}.
//   - An event named "error" throws the SDK's APIError, as
//     *openaiStreamChunkError, for its data's `error` member, or for the data
//     itself when that member is null or absent (`data?.error ?? data`).
//   - Any other event whose value has a truthy `error` member throws the
//     APIError for that member.
//
// Every APIError fails the stream, whatever its message says: the Stream's
// catch spares only a transport abort (see readOpenAISSE).
func iterateOpenAIStream(body io.Reader, ctx context.Context, yield func(openaiStreamItem) error) error {
	err := readOpenAISSE(body, ctx, func(sse openaiSSEEvent) error {
		if sse.data == "[DONE]" {
			return errOpenAIStreamDone
		}
		text := []byte(sse.data)
		value, err := openaiStreamJSON(text)
		if err != nil {
			return err
		}
		if strings.HasPrefix(sse.event, "thread.") {
			name, _ := json.Marshal(sse.event)
			return yield(openaiStreamItem{
				value: ai.OrderedObject{{Key: "event", Value: sse.event}, {Key: "data", Value: value}},
				text:  bytes.Join([][]byte{[]byte(`{"event":`), name, []byte(`,"data":`), text, []byte(`}`)}, nil),
			})
		}
		if sse.event == "error" {
			return newOpenAIStreamEventError(text, value)
		}
		if errorValue := jsGet(value, "error"); jsTruthy(errorValue) {
			return newOpenAIStreamChunkError(rawOptional(text)["error"], errorValue)
		}
		return yield(openaiStreamItem{value: value, text: text})
	})
	if errors.Is(err, errOpenAIStreamDone) {
		return nil
	}
	return err
}

// errOpenAIStreamDone ends readOpenAISSE's reading at "[DONE]".
var errOpenAIStreamDone = errors.New("the openai stream's [DONE] ended the reading")

// errOpenAIStreamNoBody is the OpenAIError the SDK's _iterSSEMessages throws on
// the Stream's first iteration when the response's body is null: after pi has
// awaited onResponse and pushed start, before any item reaches the observer.
var errOpenAIStreamNoBody = errors.New("Attempted to iterate over a response with no body")

// errOpenAIStreamMalformedJSON is the SyntaxError the SDK's Stream throws for
// an event whose data JSON.parse rejects, whatever the reason.
var errOpenAIStreamMalformedJSON = errors.New("Error reading response: malformed server-sent event JSON.")

// openaiStreamJSON is the SDK's `JSON.parse(sse.data)`: the value JSON.parse
// returns (ai.DecodeOrderedValue), else errOpenAIStreamMalformedJSON.
// jstext.JSONSyntaxError, V8's parser, tells a document JSON.parse rejects
// from one only encoding/json does, which is a port bug.
//
// Past encoding/json's nesting limit the port cannot read an event whichever
// way JSON.parse goes, so such data fails with the port's own error: the
// decoder and jstext.JSONSyntaxError recurse once per level, and a line of the
// 16 MiB the reader allows would overflow the goroutine stack.
func openaiStreamJSON(data []byte) (any, error) {
	if jsonNestsDeeperThan(data, maxJSONNesting) {
		return nil, fmt.Errorf("an openai stream event nests deeper than %d levels, which the port's JSON decoder cannot read; this is a port limit, report it with the event", maxJSONNesting)
	}
	if value, err := ai.DecodeOrderedValue(data); err == nil {
		return value, nil
	}
	if jstext.JSONSyntaxError(string(data)) != nil {
		return nil, errOpenAIStreamMalformedJSON
	}
	return nil, errors.New("encoding/json rejects an openai stream event that JSON.parse accepts; this is a port bug, report it with the event")
}

// maxJSONNesting is encoding/json's nesting limit (its scanner's
// maxNestingDepth).
const maxJSONNesting = 10000

// jsonNestsDeeperThan reports whether b opens more than limit arrays and
// objects inside one another, counting only brackets outside strings.
func jsonNestsDeeperThan(b []byte, limit int) bool {
	depth, inString, escaped := 0, false, false
	for _, c := range b {
		switch {
		case escaped:
			escaped = false
		case inString:
			escaped = c == '\\'
			inString = c != '"'
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			if depth++; depth > limit {
				return true
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return false
}

// openaiStreamChunkError is the openai SDK's APIError for a stream item
// carrying an error: `new APIError(undefined, error, undefined, headers)`.
// Its message is makeMessage's with no status, which is also what pi's
// adapters surface, since formatProviderError adds nothing without one.
type openaiStreamChunkError struct {
	message string
	// value is the APIError's `error`: data.error, or for an event named
	// "error" `data?.error ?? data`.
	value any
}

// newOpenAIStreamChunkError is the APIError for value, the error an item
// carries, whose JSON text is errorText. makeMessage reads that text through
// openaiSDKErrorDetail, which the error-body path shares.
func newOpenAIStreamChunkError(errorText []byte, value any) *openaiStreamChunkError {
	message := openaiSDKErrorDetail(errorText)
	if message == "" {
		message = "(no status code or body)"
	}
	return &openaiStreamChunkError{message: message, value: value}
}

// newOpenAIStreamEventError is the APIError an event named "error" throws,
// whose data's text and value are text and value: for the data's `error`
// member, or for the data itself when that member is null or absent — every
// value that is not an object included (`data?.error ?? data`).
func newOpenAIStreamEventError(text []byte, value any) *openaiStreamChunkError {
	if member := jsGet(value, "error"); member != nil && member != jsUndefined {
		return newOpenAIStreamChunkError(rawOptional(text)["error"], member)
	}
	return newOpenAIStreamChunkError(text, value)
}

func (e *openaiStreamChunkError) Error() string { return e.message }

// metadataRaw is pi's `(error as any)?.error?.metadata?.raw` for this error,
// as the String() the completions catch block appends: ok is false unless raw
// is truthy. A raw String() would throw on (an object with its own toString
// member) is not appended; pi's catch block itself throws there and never
// ends the stream, which the port does not reproduce.
func (e *openaiStreamChunkError) metadataRaw() (string, bool) {
	raw := jsGet(jsGet(e.value, "metadata"), "raw")
	if !jsTruthy(raw) {
		return "", false
	}
	text, err := jsToString(raw)
	return text, err == nil
}

// withOpenAIErrorMetadataRaw is the openai-completions catch block's append:
// some providers behind OpenRouter put detail in error.metadata.raw, and pi
// adds it on its own line unless the message already contains it. Only the
// SDK's APIError carries an `error`; any other failure passes through.
func withOpenAIErrorMetadataRaw(err error) error {
	var chunkErr *openaiStreamChunkError
	if !errors.As(err, &chunkErr) {
		return err
	}
	raw, ok := chunkErr.metadataRaw()
	if !ok || strings.Contains(err.Error(), raw) {
		return err
	}
	return errors.New(err.Error() + "\n" + raw)
}
