package providers

import (
	"reflect"
	"testing"
)

// D7d: a trailing comma INSIDE an open string is content, not a dangling
// token; completion must not strip it (vectors verified against partial-json
// 0.1.7, pi's streaming-JSON dependency).
func TestParseStreamingJSONTrailingCommaInsideString(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]any
	}{
		{`{"a":"x,`, map[string]any{"a": "x,"}},
		// partial-json trims the whole input first, so trailing whitespace
		// after the comma is dropped even inside the open string.
		{`{"a":"x, `, map[string]any{"a": "x,"}},
		// Real dangling commas (outside strings) are still stripped.
		{`{"a":1,`, map[string]any{"a": float64(1)}},
		{`{"a":1, `, map[string]any{"a": float64(1)}},
		{`{"a":"x,","b`, map[string]any{"a": "x,"}},
		{`{"key":`, map[string]any{}},
	}
	for _, c := range cases {
		if got := parseStreamingJSON(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseStreamingJSON(%q) = %#v want %#v", c.in, got, c.want)
		}
	}
}

// D7g: unpaired surrogates are DELETED (pi replaces them with ""), not
// substituted with U+FFFD; properly paired surrogates are preserved.
func TestSanitizeSurrogatesDeletesUnpaired(t *testing.T) {
	loneHigh := "Text \xed\xa0\xbd here"       // U+D83D unpaired (WTF-8)
	loneLow := "lo\xed\xb9\x88w"               // U+DE48 unpaired (WTF-8)
	paired := "go \xed\xa0\xbd\xed\xb9\x88 go" // U+D83D U+DE48 = 🙈 as WTF-8 pair

	if got := sanitizeSurrogates(loneHigh); got != "Text  here" {
		t.Errorf("lone high surrogate: %q want %q", got, "Text  here")
	}
	if got := sanitizeSurrogates(loneLow); got != "low" {
		t.Errorf("lone low surrogate: %q want %q", got, "low")
	}
	if got := sanitizeSurrogates(paired); got != "go 🙈 go" {
		t.Errorf("paired surrogates must be preserved: %q", got)
	}
	// Valid UTF-8 (including real emoji and U+FFFD characters) passes through.
	for _, s := range []string{"Hello 🙈 World", "kept � char", ""} {
		if got := sanitizeSurrogates(s); got != s {
			t.Errorf("valid string mutated: %q -> %q", s, got)
		}
	}
}
