package daemon

import (
	"strings"
	"testing"

	"github.com/sky-valley/pi/env/internal/frame"
)

// Upstream's watch.rs tests (102d12871).

func TestChangeEventListsPathsThatFit(t *testing.T) {
	got := marshal(changeEvent([]string{"/a", "/b"}))
	if want := `{"kind":"change","paths":["/a","/b"]}`; string(got) != want {
		t.Fatalf("changeEvent = %s, want %s", got, want)
	}
}

func TestChangeEventOverflowsWhenPathsDoNotFitInAFrame(t *testing.T) {
	path := strings.Repeat("x", 4096)
	count := frame.MaxPayload/len(path) + 1
	paths := make([]string, count)
	for i := range paths {
		paths[i] = path
	}
	got := marshal(changeEvent(paths))
	if want := `{"kind":"change","overflow":true}`; string(got) != want {
		t.Fatalf("changeEvent of %d paths = %.200s, want %s", count, got, want)
	}
}

// The limit is on the encoded event, not on the paths alone: a list whose
// paths fit exactly but whose event does not still overflows, and one byte
// less fits.
func TestChangeEventLimitIsTheEncodedEvent(t *testing.T) {
	envelope := len(marshal(changeEvent([]string{""})))
	fits := strings.Repeat("y", frame.MaxPayload-envelope)
	if got := changeEvent([]string{fits}); got["overflow"] != nil {
		t.Fatalf("an event of exactly MaxPayload bytes overflowed")
	}
	if got := changeEvent([]string{fits + "y"}); got["overflow"] != true {
		t.Fatalf("an event one byte over MaxPayload did not overflow")
	}
	// Escapes count: a quote encodes as two bytes.
	quoted := strings.Repeat(`"`, (frame.MaxPayload-envelope)/2+1)
	if got := changeEvent([]string{quoted}); got["overflow"] != true {
		t.Fatalf("escaped bytes were not counted")
	}
}
