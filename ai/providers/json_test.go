package providers

import (
	"strconv"
	"testing"
)

// hasLoneSurrogateEscape reports whether text, JSON that JSON.stringify
// wrote, escapes a surrogate code unit, which it does only for half a pair.
func hasLoneSurrogateEscape(text string) bool {
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		if text[i+1] == 'u' && i+5 < len(text) {
			if unit, err := strconv.ParseUint(text[i+2:i+6], 16, 16); err == nil && unit >= 0xd800 && unit <= 0xdfff {
				return true
			}
		}
		i++ // the escaped character
	}
	return false
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
