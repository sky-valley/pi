package daemon

import "testing"

// Upstream's decode.rs tests, which pin encoding_rs's behavior.

func TestDecodeKeepsAFEFFThatDoesNotStartTheStream(t *testing.T) {
	d := newStreamDecoder()
	text := ""
	for _, b := range []byte{0xe2, 0x82, 0xef, 0xbb, 0xbf, 0x61} {
		text += d.decode([]byte{b}, false)
	}
	text += d.decode(nil, true)
	if want := "\xef\xbf\xbd\xef\xbb\xbfa"; text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
}

func TestDecodeDropsOnlyALeadingMarkAndNeverSniffsUTF16(t *testing.T) {
	d := newStreamDecoder()
	if got := d.decode([]byte{0xef, 0xbb}, false) + d.decode([]byte{0xbf, 0x61}, true); got != "a" {
		t.Fatalf("got %q, want %q", got, "a")
	}
	d = newStreamDecoder()
	if got, want := d.decode([]byte{0xff, 0xfe, 0x41, 0x00}, true), "\xef\xbf\xbd\xef\xbf\xbdA\x00"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDecodeReplacesAnIncompleteSequenceOnce(t *testing.T) {
	d := newStreamDecoder()
	if got, want := d.decode([]byte{0xe2, 0x82, 0x41}, true), "\xef\xbf\xbdA"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	d = newStreamDecoder()
	if got, want := d.decode([]byte{0xe2, 0x82}, true), "\xef\xbf\xbd"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
