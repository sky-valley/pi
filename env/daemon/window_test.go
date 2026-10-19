package daemon

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Upstream's window.rs tests.

func testWindow(maxBytes, maxLines int) window {
	return window{maxBytes: maxBytes, maxLines: maxLines, minInterval: 0, bytesPerSecond: 1e12}
}

// totals is the bytes and newlines delivered or skipped, and the text after
// the last skip.
func totals(events []outputEvent) (uint64, uint64, string) {
	var bytes, newlines uint64
	after := ""
	for _, event := range events {
		if event.skipped != nil {
			bytes += event.skipped["bytes"].(uint64)
			newlines += event.skipped["newlines"].(uint64)
			after = ""
		}
		bytes += uint64(len(event.text))
		newlines += uint64(newlineCount(event.text))
		after += event.text
	}
	return bytes, newlines, after
}

func TestWindowCountsEverythingAndKeepsMoreThanTheWindowAfterASkip(t *testing.T) {
	p := newPending(testWindow(10, 2))
	var full strings.Builder
	var events []outputEvent
	for index := 0; index < 500; index++ {
		text := "line " + strconv.Itoa(index) + " é😀\n"
		full.WriteString(text)
		stream := "stdout"
		if index%7 == 0 {
			stream = "stderr"
		}
		p.push(stream, text)
		if index%50 == 49 {
			events = append(events, p.take()...)
		}
	}
	events = append(events, p.take()...)
	bytes, newlines, after := totals(events)
	if bytes != uint64(full.Len()) || newlines != uint64(newlineCount(full.String())) {
		t.Fatalf("counted %d bytes, %d newlines; want %d, %d", bytes, newlines, full.Len(), newlineCount(full.String()))
	}
	if !strings.HasSuffix(full.String(), after) || !(len(after) > 10 || newlineCount(after) > 2) {
		t.Fatalf("text after the last skip %q", after)
	}
	skipped := false
	for _, event := range events {
		skipped = skipped || event.skipped != nil
	}
	if !skipped {
		t.Fatal("no skip")
	}
}

func TestWindowDeliversStreamsApartWithoutSkips(t *testing.T) {
	p := newPending(testWindow(1000, 1000))
	p.push("stdout", "a")
	p.push("stderr", "b")
	p.push("stdout", "c")
	var streams []string
	for _, event := range p.take() {
		streams = append(streams, event.stream)
	}
	if strings.Join(streams, ",") != "stdout,stderr,stdout" {
		t.Fatalf("streams %v", streams)
	}
}

func TestWindowPacesDeliveryByWhatWasDelivered(t *testing.T) {
	p := newPending(window{maxBytes: 100, maxLines: 10, minInterval: 5 * time.Millisecond, bytesPerSecond: 1000})
	p.push("stdout", strings.Repeat("x", 50))
	before := time.Now()
	p.take()
	// 50 bytes at 1000 bytes per second: 50 ms, more than the minimum interval.
	if wait := p.nextSend.Sub(before); wait < 45*time.Millisecond || wait > 60*time.Millisecond {
		t.Fatalf("next send in %v", wait)
	}
}
