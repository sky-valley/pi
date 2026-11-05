package jstext

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestTextDecoderMatchesNode replays each decode node's TextDecoder made
// of a byte stream split into reads (testdata/text-decoder/capture.mts): the
// text of every read, a byte-order mark dropped only before the stream's first
// character, a sequence a read leaves incomplete carried into the next, and
// the bytes still held at the end dropped, as a decoder never flushed drops
// them. Where node's UTF-8 fast path drops a second byte-order mark (the
// capture's nodeQuirk rows), the reads must join to the Encoding Standard's
// text instead.
func TestTextDecoderMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/text-decoder/text-decoder-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Input        []byte   `json:"input"`
			Cuts         []int    `json:"cuts"`
			Reads        []string `json:"reads"`
			NodeQuirk    bool     `json:"nodeQuirk"`
			StandardJoin string   `json:"standardJoin"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) < 100 {
		t.Fatalf("only %d rows in the capture", len(capture.Rows))
	}
	quirks := 0
	for _, row := range capture.Rows {
		var d TextDecoder
		var reads []string
		at := 0
		for _, cut := range append(slices.Clone(row.Cuts), len(row.Input)) {
			reads = append(reads, d.Decode(row.Input[at:cut]))
			at = cut
		}
		if row.NodeQuirk {
			quirks++
			if joined := strings.Join(reads, ""); joined != row.StandardJoin {
				t.Errorf("% x cut at %v: %q, the Encoding Standard %q", row.Input, row.Cuts, joined, row.StandardJoin)
			}
			continue
		}
		if !slices.Equal(reads, row.Reads) {
			t.Errorf("% x cut at %v: %q, node %q", row.Input, row.Cuts, reads, row.Reads)
		}
	}
	if quirks == 0 {
		t.Fatal("the capture tags no nodeQuirk rows; the split doubled byte-order mark should be one")
	}
}

// TestDecodeTextMatchesNode replays what one decode call of a fresh node
// TextDecoder made of each input (testdata/capture-textdecode.mjs): DecodeText
// for decode(b), and a fresh TextDecoder's Decode for decode(b, {stream: true}).
func TestDecodeTextMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/textdecode-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Input  string `json:"input"`
			Stream bool   `json:"stream"`
			Text   string `json:"text"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) < 60 {
		t.Fatalf("only %d rows in the capture", len(capture.Rows))
	}
	for _, row := range capture.Rows {
		input, err := hex.DecodeString(row.Input)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if row.Stream {
			var d TextDecoder
			got = d.Decode(input)
		} else {
			got = DecodeText(input)
		}
		if got != row.Text {
			t.Errorf("% x (stream %t) = %+q, node %+q", input, row.Stream, got, row.Text)
		}
	}
}

// Flush ends the stream and the decoder starts over: the next stream's leading
// byte-order mark is dropped again. node v26.4.0 decodes these calls, in order,
// to "a", "", "\ufffd" and "b".
func TestTextDecoderStartsOverAfterFlush(t *testing.T) {
	var d TextDecoder
	got := []string{
		d.Decode([]byte("\xef\xbb\xbfa")),
		d.Decode([]byte("\xe2\x82")),
		d.Flush(),
		d.Decode([]byte("\xef\xbb\xbfb")),
	}
	if want := []string{"a", "", "\ufffd", "b"}; !slices.Equal(got, want) {
		t.Fatalf("got %+q, node %+q", got, want)
	}
}

// TextDecoder("utf-8", {ignoreBOM: true}) keeps a leading byte-order mark,
// however the calls split it, and still decodes like Buffer.toString.
func TestTextDecoderIgnoreBOMKeepsTheMark(t *testing.T) {
	input := []byte("\xef\xbb\xbfa\xc3")
	for split := 0; split <= len(input); split++ {
		d := TextDecoder{IgnoreBOM: true}
		got := d.Decode(input[:split]) + d.Decode(input[split:]) + d.Flush()
		if want := "\ufeffa\ufffd"; got != want {
			t.Errorf("split at %d: %q, want %q", split, got, want)
		}
		if !d.IgnoreBOM {
			t.Errorf("Flush reset IgnoreBOM")
		}
	}
}
