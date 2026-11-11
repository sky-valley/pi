package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/sky-valley/pi/agent"
)

// Upstream 3a7bb6f48 (pi test/read-streaming.test.ts): the read tool streams
// text files through ReadOperations.CreateReadStream, keeping only the lines
// it can show, and gives the same result as the whole-file path.

const readTestPath = "/virtual/file.txt"

// readOutcome is a read's observable result: the tool result as JSON (text
// and details), or its error.
type readOutcome struct {
	Result string
	Err    string
}

func readWith(t *testing.T, ops ReadOperations, params map[string]any) readOutcome {
	t.Helper()
	res, err := readToolOps("/", &ops, nil).Execute(context.Background(), "call", params, func(agent.AgentToolResult) {})
	if err != nil {
		return readOutcome{Err: err.Error()}
	}
	text, merr := json.Marshal(res)
	if merr != nil {
		t.Fatal(merr)
	}
	return readOutcome{Result: string(text)}
}

// bufferOps reads the whole file; streamOps streams it in chunks of size n
// and fails on ReadFile, which it must not use.
func bufferOps(data []byte) ReadOperations {
	return ReadOperations{
		ReadFile: func(context.Context, string) ([]byte, error) { return data, nil },
		Access:   func(context.Context, string) error { return nil },
	}
}

func streamOps(data []byte, n int) ReadOperations {
	return ReadOperations{
		ReadFile: func(context.Context, string) ([]byte, error) {
			return nil, errors.New("readFile must not be used when createReadStream is present")
		},
		Access: func(context.Context, string) error { return nil },
		CreateReadStream: func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(chunked(data, n)), nil
		},
	}
}

func chunked(data []byte, n int) io.Reader {
	if n == 1 {
		return iotest.OneByteReader(bytes.NewReader(data))
	}
	return &chunkReader{data: data, n: n}
}

type chunkReader struct {
	data []byte
	n    int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	k := copy(p[:min(len(p), c.n)], c.data)
	c.data = c.data[k:]
	return k, nil
}

func joinLines(count int, line func(i int) string) string {
	lines := make([]string, count)
	for i := range lines {
		lines[i] = line(i)
	}
	return strings.Join(lines, "\n")
}

func readParams(offset, limit any) map[string]any {
	params := map[string]any{"path": readTestPath}
	if offset != nil {
		params["offset"] = offset
	}
	if limit != nil {
		params["limit"] = limit
	}
	return params
}

func TestReadStreamGivesTheWholeFileResult(t *testing.T) {
	fiftyKB := joinLines(512, func(int) string { return strings.Repeat("y", 99) }) + "y"
	cases := map[string]string{
		"empty":               "",
		"no trailing newline": "alpha\nbeta\ngamma",
		"trailing newline":    "alpha\nbeta\n",
		"CRLF":                "alpha\r\nbeta\r\n\r\ngamma\r\n",
		"multi-byte characters and UTF-8 byte order mark": "\ufeffé€😀\nñ\U00010348字\n😀😀😀",
		"2001 lines": joinLines(2001, func(i int) string { return fmt.Sprintf("line %d", i) }),
		// TruncateHead drops a trailing empty line: the empty line at the
		// line cap must not end the stored lines.
		"2001 lines, an empty line and more":   joinLines(2001, func(i int) string { return fmt.Sprintf("line %d", i) }) + "\n\nmore",
		"first line over 50KB":                 strings.Repeat("é", DefaultMaxBytes/2+1) + "\nsecond",
		"line over 50KB after short lines":     "a\nb\n" + strings.Repeat("z", 60*1024) + "\nc",
		"exactly 50KB and a trailing newline":  fiftyKB + "\n",
		"exactly 50KB, an empty line and more": fiftyKB + "\n\nmore",
		"multi-byte characters over 50KB":      joinLines(700, func(i int) string { return fmt.Sprintf("%d %s", i, strings.Repeat("€😀", 13)) }),
	}
	for name, text := range cases {
		data := []byte(text)
		total := strings.Count(text, "\n") + 1
		for _, offset := range []any{nil, 2.0, 3.0, float64(total), float64(total + 1)} {
			for _, limit := range []any{nil, 0.0, 1.0, float64(total)} {
				want := readWith(t, bufferOps(data), readParams(offset, limit))
				for _, n := range []int{1, 4096} {
					if got := readWith(t, streamOps(data, n), readParams(offset, limit)); got != want {
						t.Errorf("%s, offset %v, limit %v, chunks of %d:\n got %.300v\nwant %.300v", name, offset, limit, n, got, want)
					}
				}
			}
		}
	}
}

// pi decodes like Buffer.toString("utf-8"): one U+FFFD per maximal subpart,
// a byte-order mark kept, whichever path reads the file and however the
// chunks split a character. The expected text is node's own decode.
func TestReadDecodesLikeBufferToString(t *testing.T) {
	data := append([]byte("aé€😀\n😀b\U00010348\n"), 0x61, 0xc3, 0x0a, 0xe2, 0x82, 0x0a, 0xff, 0xfe, 0x80, 0xf0, 0x9f, 0x98, 0x0a, 0xed, 0xa0, 0x80)
	const node = "aé€😀\n😀b\U00010348\na\ufffd\n\ufffd\n\ufffd\ufffd\ufffd\ufffd\n\ufffd\ufffd\ufffd"
	want := readWith(t, bufferOps([]byte(node)), readParams(nil, nil))
	for _, ops := range []ReadOperations{bufferOps(data), streamOps(data, 1), streamOps(data, 2), streamOps(data, 3)} {
		if got := readWith(t, ops, readParams(nil, nil)); got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// ReadFile serves operations without CreateReadStream, and fractional or
// negative ranges, whose slicing defines what they select.
func TestReadUsesReadFileForUnstreamableRanges(t *testing.T) {
	data := []byte("one\ntwo\nthree\nfour")
	withStream := bufferOps(data)
	withStream.CreateReadStream = func(context.Context, string) (io.ReadCloser, error) {
		return nil, errors.New("an unstreamable range must not open a stream")
	}
	for _, r := range [][2]any{{2.5, 1.0}, {nil, -1.0}, {1.0, 1.5}} {
		if got := readWith(t, withStream, readParams(r[0], r[1])); got.Err != "" {
			t.Errorf("offset %v, limit %v: %s", r[0], r[1], got.Err)
		}
	}
	for _, r := range [][2]any{{nil, nil}, {2.0, 1.0}, {9.0, nil}} {
		if got := readWith(t, withStream, readParams(r[0], r[1])); !strings.Contains(got.Err, "must not open a stream") {
			t.Errorf("offset %v, limit %v streamed nothing: %+v", r[0], r[1], got)
		}
	}
	// pi's own output under node at 3a7bb6f48: limit -1 selects all lines
	// but the last, and the footer keeps the raw arithmetic.
	if got := readWith(t, bufferOps(data), readParams(nil, -1.0)); !strings.Contains(got.Result, `"one\ntwo\nthree\n\n[5 more lines in file. Use offset=0 to continue.]"`) {
		t.Errorf("limit -1: %s", got.Result)
	}
}

func TestReadStreamsLocalFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local.txt")
	text := joinLines(3000, func(i int) string { return fmt.Sprintf("%d %s", i, strings.Repeat("w", i%40)) })
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	local := DefaultReadOperations()
	if local.CreateReadStream == nil {
		t.Fatal("DefaultReadOperations has no CreateReadStream")
	}
	whole := local
	whole.CreateReadStream = nil
	for _, r := range [][2]any{{nil, nil}, {1500.0, 10.0}} {
		params := map[string]any{"path": path}
		if r[0] != nil {
			params["offset"], params["limit"] = r[0], r[1]
		}
		want := readWith(t, whole, params)
		if got := readWith(t, local, params); got != want {
			t.Errorf("%v: streamed %.200v, whole file %.200v", r, got, want)
		}
	}
	if got := readWith(t, local, map[string]any{"path": dir}); !strings.Contains(got.Err, "EISDIR") {
		t.Errorf("directory read = %+v, want EISDIR", got)
	}
}

// pi's "reads a local file up to its size when opened, like fs.readFile": a
// line appended while the read is under way is not counted.
func TestReadLocalStreamStopsAtTheSizeWhenOpened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.txt")
	if err := os.WriteFile(path, []byte("head\n"+strings.Repeat("x", 200_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := DefaultReadOperations()
	open := ops.CreateReadStream
	ops.CreateReadStream = func(ctx context.Context, p string) (io.ReadCloser, error) {
		stream, err := open(ctx, p)
		if err == nil {
			f, ferr := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if ferr != nil {
				t.Fatal(ferr)
			}
			f.WriteString("\nadded")
			f.Close()
		}
		return stream, err
	}
	res, err := readToolOps("/", &ops, nil).Execute(context.Background(), "call", map[string]any{"path": path, "limit": 1.0}, func(agent.AgentToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resultText(res), "head\n\n[1 more lines in file. Use offset=2 to continue.]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A 200 MB first line is measured, never stored whole.
func TestReadStreamMeasuresAHugeFirstLine(t *testing.T) {
	ops := ReadOperations{
		ReadFile: func(context.Context, string) ([]byte, error) { return nil, nil },
		Access:   func(context.Context, string) error { return nil },
		CreateReadStream: func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(repeatByte('x'), 200*1024*1024)), nil
		},
	}
	res, err := readToolOps("/", &ops, nil).Execute(context.Background(), "call", map[string]any{"path": "big.log"}, func(agent.AgentToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("[Line 1 is 200.0MB, exceeds 50.0KB limit. Use bash: sed -n '1p' big.log | head -c %d]", DefaultMaxBytes)
	if got := resultText(res); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// pi's "stops consuming the stream when aborted": the read fails with
// "Operation aborted", reads no further and closes the stream.
func TestReadStreamStopsWhenAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &abortingStream{cancel: cancel}
	ops := ReadOperations{
		ReadFile: func(context.Context, string) ([]byte, error) { return nil, nil },
		Access:   func(context.Context, string) error { return nil },
		CreateReadStream: func(context.Context, string) (io.ReadCloser, error) {
			return stream, nil
		},
	}
	_, err := readToolOps("/", &ops, nil).Execute(ctx, "call", map[string]any{"path": readTestPath}, func(agent.AgentToolResult) {})
	if err == nil || err.Error() != "Operation aborted" {
		t.Fatalf("err = %v, want Operation aborted", err)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.reads != 3 || !stream.closed {
		t.Errorf("reads = %d, closed = %v; want 3 reads, then closed", stream.reads, stream.closed)
	}
}

// Characterization of upstream ef9361ab7 (#11956), which stopped handing the
// abort signal to the local file stream so that an abort while the file is
// opened no longer crashes Node with an unhandled stream 'error'. The port's
// local stream never took a context: the read loop checks ctx before every
// chunk. These pin the two outcomes upstream's tests assert, through the real
// local operations.

// An abort while the local file is opened fails the read with "Operation
// aborted", reads nothing and closes the file.
func TestReadLocalFileAbortedWhileOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abort-on-open.txt")
	if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops := DefaultReadOperations()
	var stream *countingReadCloser
	open := ops.CreateReadStream
	ops.CreateReadStream = func(ctx context.Context, p string) (io.ReadCloser, error) {
		cancel()
		rc, err := open(ctx, p)
		if err != nil {
			return nil, err
		}
		stream = &countingReadCloser{ReadCloser: rc}
		return stream, nil
	}
	_, err := readToolOps(filepath.Dir(path), &ops, nil).Execute(ctx, "call", map[string]any{"path": path}, func(agent.AgentToolResult) {})
	if err == nil || err.Error() != "Operation aborted" {
		t.Fatalf("err = %v, want Operation aborted", err)
	}
	if stream == nil || stream.reads != 0 || !stream.closed {
		t.Errorf("stream %+v: want opened, never read, closed", stream)
	}
}

// An abort mid-read of a local file stops after the chunk in hand: the file
// is 16 chunks (1 MB at today's 64 KB, upstream's size), and only the first
// is read.
func TestReadLocalFileStopsWhenAbortedMidRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abort-mid-read.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 16*streamChunkSize), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops := DefaultReadOperations()
	var stream *countingReadCloser
	open := ops.CreateReadStream
	ops.CreateReadStream = func(ctx context.Context, p string) (io.ReadCloser, error) {
		rc, err := open(ctx, p)
		if err != nil {
			return nil, err
		}
		stream = &countingReadCloser{ReadCloser: rc, onRead: cancel}
		return stream, nil
	}
	_, err := readToolOps(filepath.Dir(path), &ops, nil).Execute(ctx, "call", map[string]any{"path": path}, func(agent.AgentToolResult) {})
	if err == nil || err.Error() != "Operation aborted" {
		t.Fatalf("err = %v, want Operation aborted", err)
	}
	if stream == nil || stream.reads != 1 || !stream.closed {
		t.Errorf("stream %+v: want one read, then closed", stream)
	}
}

// countingReadCloser counts the reads of a stream, calls onRead after each,
// and records its close. It needs no lock although an abort closes the
// stream from context.AfterFunc's goroutine: no Read follows the cancel (the
// read loop checks ctx first), and Execute waits for that Close before it
// returns, so the test's reads of the fields happen after it.
type countingReadCloser struct {
	io.ReadCloser
	onRead func()
	reads  int
	closed bool
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.reads++
	if c.onRead != nil {
		c.onRead()
	}
	return n, err
}

func (c *countingReadCloser) Close() error {
	c.closed = true
	return c.ReadCloser.Close()
}

// abortingStream cancels its read on the third Read. Close may come from the
// abort's own goroutine, hence the lock.
type abortingStream struct {
	cancel func()
	mu     sync.Mutex
	reads  int
	closed bool
}

func (s *abortingStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	s.reads++
	reads := s.reads
	s.mu.Unlock()
	if reads == 3 {
		s.cancel()
	}
	return copy(p, "line\n"), nil
}

func (s *abortingStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// The details pi serializes into session files keep their key order on both
// paths.
func TestReadStreamKeepsDetails(t *testing.T) {
	data := []byte(joinLines(2001, func(i int) string { return fmt.Sprintf("line %d", i) }))
	streamed := readWith(t, streamOps(data, 4096), readParams(1.0, nil))
	whole := readWith(t, bufferOps(data), readParams(1.0, nil))
	if !reflect.DeepEqual(streamed, whole) || !strings.Contains(streamed.Result, `"truncation"`) {
		t.Errorf("streamed %.300v, whole %.300v", streamed, whole)
	}
}
