package coding

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// Upstream 9e870f6f0 (pi test/truncate-surrogates.test.ts, issue #7121):
// truncation never cuts an emoji in half.
func TestTruncateLineKeepsSurrogatePairsWhole(t *testing.T) {
	const emoji = "\U0001F600" // two UTF-16 code units
	got, was := TruncateLine(strings.Repeat("a", 499)+emoji+"tail", 500)
	if !was || got != strings.Repeat("a", 499)+"... [truncated]" {
		t.Errorf("emoji straddling the limit: %q, %v", got, was)
	}
	got, _ = TruncateLine(strings.Repeat("a", 498)+emoji+"tail", 500)
	if want := strings.Repeat("a", 498) + emoji + "... [truncated]"; got != want {
		t.Errorf("emoji ending at the limit: %q, want %q", got, want)
	}
}

// pi's "sliceHead ... never split a pair" and "match slice on text without
// surrogates" (the sliceTail half has no Go consumer yet: its callers are
// the MCP and codemode extensions, Scope queue rows 21 and 22).
func TestSliceHead(t *testing.T) {
	const text = "\U0001F600x\U0001F600" // 5 UTF-16 units
	for units := 0; units <= 6; units++ {
		head := sliceHead(text, units)
		if !utf8.ValidString(head) || !strings.HasPrefix(text, head) {
			t.Errorf("sliceHead(%d) = %q", units, head)
		}
		if n := jstext.UTF16Length(head); n > units || n < min(units, 5)-1 {
			t.Errorf("sliceHead(%d) holds %d units", units, n)
		}
	}
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{{"abcdef", 3, "abc"}, {"abc", 10, "abc"}, {"abc", 0, ""}, {"abc", -1, ""}} {
		if got := sliceHead(tc.s, tc.n); got != tc.want {
			t.Errorf("sliceHead(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
