package providers

import (
	"reflect"
	"strings"
	"testing"
)

// decodeSSE pushes each chunk through d and returns the events they
// complete.
func decodeSSE(t *testing.T, d *sseDecoder, chunks ...string) []serverSentEvent {
	t.Helper()
	var events []serverSentEvent
	emit := func(ev serverSentEvent) error {
		events = append(events, ev)
		return nil
	}
	for _, c := range chunks {
		if err := d.push([]byte(c), emit); err != nil {
			t.Fatalf("push(%q): %v", c, err)
		}
	}
	return events
}

// The tests below transliterate upstream's test/sse.test.ts (da7e26cc8).

func TestSSEDecoderSameEventsForEveryChunkingAndLineEnding(t *testing.T) {
	want := []serverSentEvent{
		{event: "delta", data: "{\"a\":\n \"é\"}", raw: []string{": keepalive", "event: delta", `data: {"a":`, `data:  "é"}`}},
		{data: "tail", raw: []string{"data: tail"}},
	}
	for _, sep := range []string{"\n", "\r\n", "\r"} {
		body := strings.Join([]string{": keepalive", "event: delta", `data: {"a":`, `data:  "é"}`, "", "data: tail"}, sep)
		for size := 1; size <= len(body); size++ {
			var chunks []string
			for i := 0; i < len(body); i += size {
				chunks = append(chunks, body[i:min(i+size, len(body))])
			}
			var d sseDecoder
			got := decodeSSE(t, &d, chunks...)
			if err := d.end(func(ev serverSentEvent) error { got = append(got, ev); return nil }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("separator %q, chunk size %d: events = %#v, want %#v", sep, size, got, want)
			}
		}
	}
}

func TestSSEDecoderCRLFSplitAcrossPushesIsOneLineBreak(t *testing.T) {
	var d sseDecoder
	if got := decodeSSE(t, &d, "data: a\r"); len(got) != 0 {
		t.Fatalf("events = %#v, want none", got)
	}
	got := decodeSSE(t, &d, "\ndata: b\r\n\r\n")
	want := []serverSentEvent{{data: "a\nb", raw: []string{"data: a", "data: b"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestSSEDecoderEmptyEventNameDoesNotCarry(t *testing.T) {
	var d sseDecoder
	got := decodeSSE(t, &d, "event:\n\ndata: x\n\n")
	want := []serverSentEvent{{data: "x", raw: []string{"data: x"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestSSEDecoderDropsCommentLinesOfEventsWithoutData(t *testing.T) {
	var d sseDecoder
	got := decodeSSE(t, &d, ": ping\n\ndata: x\n\n")
	want := []serverSentEvent{{data: "x", raw: []string{"data: x"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

// A line far longer than one push (and than bufio.Scanner's 64 KiB token)
// arrives whole; with no maxLine set there is no bound, as in pi.
func TestSSEDecoderReadsALineLongerThanManyPushes(t *testing.T) {
	payload := strings.Repeat("x", 1<<20)
	body := "data: " + payload + "\n\n"
	var chunks []string
	for i := 0; i < len(body); i += 4096 {
		chunks = append(chunks, body[i:min(i+4096, len(body))])
	}
	var d sseDecoder
	got := decodeSSE(t, &d, chunks...)
	if len(got) != 1 || got[0].data != payload {
		t.Fatalf("got %d events, want one carrying the %d-byte line", len(got), len(payload))
	}
}
