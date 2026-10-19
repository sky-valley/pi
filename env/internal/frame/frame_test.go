package frame

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// rustStream is what the Rust daemon of pi-env 1.3.0 wrote for `serve
// --token ab` given a request {"op":"frobnicate"} (id 7) and a request
// {"op":"pread","handle":1} (id 8), then five seconds of silence: the sync
// line, two error frames, and a ping.
const rustStream = "50492d454e562061620a" +
	"0000004303000000070000003a7b22636f6465223a2245494e56414c222c226d657373616765223a22756e6b6e6f776e206f7065726174696f6e2066726f626e6963617465227d" +
	"0000003403000000080000002b7b22636f6465223a224542414446222c226d657373616765223a22756e6b6e6f776e2068616e646c65227d" +
	"0000000b0600000000000000027b7d"

func TestReadsAndWritesTheRustDaemonsFrames(t *testing.T) {
	stream, err := hex.DecodeString(rustStream)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := bytes.CutPrefix(stream, []byte("PI-ENV ab\n"))
	if !ok {
		t.Fatal("no sync line")
	}
	r := bytes.NewReader(body)
	want := []Frame{
		{Kind: Error, ID: 7, JSON: []byte(`{"code":"EINVAL","message":"unknown operation frobnicate"}`), Payload: []byte{}},
		{Kind: Error, ID: 8, JSON: []byte(`{"code":"EBADF","message":"unknown handle"}`), Payload: []byte{}},
		{Kind: Ping, ID: 0, JSON: []byte(`{}`), Payload: []byte{}},
	}
	var written bytes.Buffer
	for i, w := range want {
		f, err := Read(r)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Kind != w.Kind || f.ID != w.ID || !bytes.Equal(f.JSON, w.JSON) || !bytes.Equal(f.Payload, w.Payload) {
			t.Fatalf("frame %d: got %+v, want %+v", i, f, w)
		}
		if err := Write(&written, f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Read(r); err != io.EOF {
		t.Fatalf("after the last frame: %v, want io.EOF", err)
	}
	if !bytes.Equal(written.Bytes(), body) {
		t.Fatalf("written %x\nwant    %x", written.Bytes(), body)
	}
}

// Upstream's frame.rs test.
func TestRoundTripsJSONAndPayload(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, Frame{Kind: Result, ID: 7, JSON: []byte(`{"a":1}`), Payload: []byte{0, 255, 10}}); err != nil {
		t.Fatal(err)
	}
	f, err := Read(&b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != Result || f.ID != 7 || string(f.JSON) != `{"a":1}` || !bytes.Equal(f.Payload, []byte{0, 255, 10}) {
		t.Fatalf("got %+v", f)
	}
	if _, err := Read(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("empty input: %v, want io.EOF", err)
	}
}

func TestFramingErrors(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		want  error
	}{
		// An end inside the length field is a clean end, as read_exact of it fails.
		{"partial length", []byte{0, 0}, io.EOF},
		{"too short", []byte{0, 0, 0, 8, 1, 0, 0, 0, 0, 0, 0, 0}, errLength},
		{"too long", []byte{0x01, 0x00, 0x00, 0x01}, errLength},
		{"JSON longer than the frame", []byte{0, 0, 0, 9, 1, 0, 0, 0, 1, 0, 0, 0, 1}, errJSONLength},
		{"cut short", []byte{0, 0, 0, 12, 1, 0, 0, 0, 1, 0, 0, 0, 1, '{'}, io.ErrUnexpectedEOF},
		{"body missing", []byte{0, 0, 0, 9}, io.ErrUnexpectedEOF},
	}
	for _, c := range cases {
		if _, err := Read(bytes.NewReader(c.input)); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := Write(io.Discard, Frame{Payload: make([]byte, MaxFrame)}); err != ErrTooLarge {
		t.Errorf("oversized write: %v", err)
	}
	f, err := Read(bytes.NewReader([]byte{0, 0, 0, 9, 1, 0, 0, 0, 3, 0, 0, 0, 0}))
	if err != nil || f.JSON != nil || f.ID != 3 {
		t.Errorf("empty JSON: %+v %v", f, err)
	}
}
