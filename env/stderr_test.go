package env

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// stderrGolden is testdata/stderr/479d6e5e2.json (capture.mts): node's
// util.stripVTControlCharacters and upstream's withStderr over a fuzz corpus.
type stderrGolden struct {
	Node      string `json:"node"`
	Message   string `json:"message"`
	TailChars int    `json:"tailChars"`
	TailLines int    `json:"tailLines"`
	Cases     []struct {
		Input      string `json:"input"`
		Strip      string `json:"strip"`
		WithStderr string `json:"withStderr"`
	} `json:"cases"`
	Long []struct {
		Runs       [][2]json.RawMessage `json:"runs"`
		WithStderr json.RawMessage      `json:"withStderr"`
	} `json:"long"`
}

func TestStderrMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/stderr/479d6e5e2.json")
	mustDo(t, err)
	var golden stderrGolden
	mustDo(t, json.Unmarshal(data, &golden))
	if len(golden.Cases) == 0 || len(golden.Long) == 0 {
		t.Fatal("testdata/stderr/479d6e5e2.json is incomplete; recapture it with capture.mts")
	}
	if golden.TailChars != stderrTailChars || golden.TailLines != stderrTailLines {
		t.Fatalf("upstream keeps %d characters and %d lines", golden.TailChars, golden.TailLines)
	}
	failures := 0
	for _, c := range golden.Cases {
		if got := stripVTControlCharacters(c.Input); got != c.Strip {
			t.Errorf("stripVTControlCharacters(%q) = %q, node %q", c.Input, got, c.Strip)
			failures++
		}
		if got := withStderr(golden.Message, c.Input); got != c.WithStderr {
			t.Errorf("withStderr(%q) = %q, upstream %q", c.Input, got, c.WithStderr)
			failures++
		}
		if failures > 20 {
			t.Fatal("too many failures")
		}
	}
	for _, c := range golden.Long {
		var input strings.Builder
		for _, run := range c.Runs {
			var piece string
			var n int
			mustDo(t, json.Unmarshal(run[0], &piece))
			mustDo(t, json.Unmarshal(run[1], &n))
			input.WriteString(strings.Repeat(piece, n))
		}
		// Upstream slices the tail after every chunk; any chunking keeps the
		// same last code units.
		var tail stderrTail
		text := input.String()
		for i := 0; i < len(text); {
			n := min(len(text)-i, 1+i%977)
			for n < len(text)-i && !utf8RuneStart(text[i+n]) {
				n++
			}
			tail.add(text[i : i+n])
			i += n
		}
		got := jstext.CodeUnits(withStderr(golden.Message, tail.String()))
		want := jsonStringUnits(t, c.WithStderr)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("withStderr of %s = %q, upstream %q", c.Runs, string(utf16.Decode(got)), string(utf16.Decode(want)))
		}
	}
}

func utf8RuneStart(b byte) bool { return b&0xc0 != 0x80 }

// jsonStringUnits is the UTF-16 code units of a JSON string, half a
// surrogate pair (which JSON.stringify writes as a \u escape) included;
// encoding/json would make it U+FFFD.
func jsonStringUnits(t *testing.T, raw json.RawMessage) []uint16 {
	t.Helper()
	s := string(raw)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		t.Fatalf("not a JSON string: %.40s", s)
	}
	s = s[1 : len(s)-1]
	var units []uint16
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			r, size := utf8.DecodeRuneInString(s[i:])
			units = utf16.AppendRune(units, r)
			i += size
			continue
		}
		switch s[i+1] {
		case 'u':
			unit, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
			mustDo(t, err)
			units = append(units, uint16(unit))
			i += 6
			continue
		case 'n':
			units = append(units, '\n')
		case 't':
			units = append(units, '\t')
		case 'r':
			units = append(units, '\r')
		case 'b':
			units = append(units, '\b')
		case 'f':
			units = append(units, '\f')
		default:
			units = append(units, uint16(s[i+1]))
		}
		i += 2
	}
	return units
}

// TestStderrDecoderMatchesSetEncoding: setEncoding("utf8") holds a character
// split between chunks for the later one, and its end makes an incomplete
// one U+FFFD; a byte-order mark stays.
func TestStderrDecoderMatchesSetEncoding(t *testing.T) {
	for _, c := range []struct {
		chunks []string
		want   []string
	}{
		{[]string{"prefix \xc3", "\xa9 suffix\n"}, []string{"prefix ", "\u00e9 suffix\n", ""}},
		{[]string{"a\xe9", "\x94\x99"}, []string{"a", "\u9519", ""}},
		{[]string{"\xf0\x9f", "\x98", "\x80!"}, []string{"", "", "\U0001f600!", ""}},
		{[]string{"x\xf0\x9f"}, []string{"x", "\ufffd"}},
		{[]string{"\xef\xbb\xbfbom"}, []string{"\ufeffbom", ""}},
		{[]string{"\xe0\x80", "\x80"}, []string{"", "\ufffd\ufffd\ufffd", ""}},
		{[]string{"\xf0\x9f", "A"}, []string{"", "\ufffdA", ""}},
		{[]string{"\x80\x80\x80\x80"}, []string{"\ufffd\ufffd\ufffd\ufffd", ""}},
		{[]string{"\xff\xc3"}, []string{"\ufffd", "\ufffd"}},
	} {
		var d stderrDecoder
		var got []string
		for _, chunk := range c.chunks {
			got = append(got, d.decode([]byte(chunk)))
		}
		got = append(got, d.end())
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %q, want %q", c.chunks, got, c.want)
		}
	}
}
