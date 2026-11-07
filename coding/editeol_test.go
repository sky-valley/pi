package coding

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Upstream d448c51e2 (#11751, #8544): the edit tool splices its replacements
// into the original text, so untouched lines keep their line endings, and
// inserted line breaks take the ending of the line the replaced range starts
// on. Each case is one of pi's tools.test.ts cases at d448c51e2, same bytes.
func TestEditKeepsLineEndingsOfUntouchedLines(t *testing.T) {
	type edit struct{ oldText, newText string }
	cases := []struct {
		name    string
		initial string
		edits   []edit
		want    string
	}{
		{"CRLF for inserted lines in CRLF files", "first\r\nsecond\r\nthird",
			[]edit{{"second\nthird", "2a\r\n2b\nthird!"}}, "first\r\n2a\r\n2b\r\nthird!"},
		{"LF for inserted lines in LF files", "first\nsecond\nthird",
			[]edit{{"second\nthird", "2a\r\n2b\nthird!"}}, "first\n2a\n2b\nthird!"},
		{"CRLF lines kept when the first line ends with LF", "a\nb\r\nX = 1\r\n",
			[]edit{{"X = 1", "X = 2"}}, "a\nb\r\nX = 2\r\n"},
		{"LF lines kept when the first line ends with CRLF", "a\r\nb\nc",
			[]edit{{"c", "C"}}, "a\r\nb\nC"},
		{"a lone CR kept in an untouched line", "a\rb\nX = 1\n",
			[]edit{{"X = 1", "X = 2"}}, "a\rb\nX = 2\n"},
		{"inserted lines take the ending of the line they replace, multi-edit", "lf1\nlf2\ncr1\r\ncr2\r\n",
			[]edit{{"lf1\n", "A\nB\n"}, {"cr1\n", "C\nD\n"}}, "A\nB\nlf2\nC\r\nD\r\ncr2\r\n"},
		{"untouched endings kept when fuzzy matching widens to whole lines", "a\r\nfoo   \r\nb\n",
			[]edit{{"foo\n", "FOO\n"}}, "a\r\nFOO\r\nb\n"},
		// A kept CR and an LF meeting at a splice seam must not merge into one
		// CRLF line break.
		{"inserted text starting with LF after a kept lone CR", "a\rb\nc",
			[]edit{{"b", "\nB"}}, "a\r\n\nB\nc"},
		{"deleted text between a kept lone CR and a kept LF", "a\rb\nc",
			[]edit{{"b", ""}}, "a\r\n\nc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			var edits []any
			for _, e := range tc.edits {
				edits = append(edits, map[string]any{"oldText": e.oldText, "newText": e.newText})
			}
			if _, err := run(t, editTool(dir), map[string]any{"path": "f.txt", "edits": edits}); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
			if string(got) != tc.want {
				t.Fatalf("wrote %q, want %q", got, tc.want)
			}
		})
	}
}

// pi's "should not merge an inserted CR line ending with a kept line feed
// after it": only files without LF give inserted text CR endings, so the seam
// is checked directly.
func TestApplyReplacementsToOriginalCRSeam(t *testing.T) {
	written := applyReplacementsToOriginal("a\rb\nc", []matchedEdit{{matchIndex: 2, matchLength: 1, newText: "\r"}})
	if got := normalizeToLF(written); got != "a\n\n\nc" {
		t.Fatalf("normalized %q = %q, want %q", written, got, "a\n\n\nc")
	}
}

// pi's "should write content that normalizes to the diffed content for random
// mixed line endings", with its generator: the same 3000 runs.
func TestApplyReplacementsToOriginalNormalizesToResult(t *testing.T) {
	// JavaScript computes seed*1103515245 + 12345 in doubles, past 2^53, and
	// "&" takes ToInt32 of the rounded result; this reproduces both roundings.
	seed := 1.0
	random := func(n int) int {
		product := seed*1103515245 + 12345
		seed = float64(uint32(int64(math.Mod(product, 1<<32))) & 0x7fffffff)
		// The high bits: the low bits of this generator repeat with a short period.
		return int(math.Floor(seed / 0x80000000 * float64(n)))
	}
	endings := []string{"\n", "\r\n", "\r", ""}
	newTexts := []string{"", "\n", "X\n", "\nY", "\r", "Z"}
	applied := 0
	for run := 0; run < 3000; run++ {
		original := ""
		for line := random(12) + 1; line > 0; line-- {
			original += "w" + strconv.Itoa(random(100))
			if random(5) == 0 {
				original += "  "
			}
			original += endings[random(4)]
		}
		normalized := normalizeToLF(original)
		start := random(len(normalized))
		end := min(start+1+random(12), len(normalized))
		oldText := normalized[start:end]
		newText := newTexts[random(6)]
		_, result, replacements, err := applyEditsToNormalizedContent(normalized, []editEntry{{oldText: oldText, newText: newText}}, "f.txt")
		if err != nil {
			continue // duplicate, not found or no change
		}
		applied++
		if got := normalizeToLF(applyReplacementsToOriginal(original, replacements)); got != result {
			t.Fatalf("run %d: original %q, edit %q -> %q: spliced text normalizes to %q, want %q", run, original, oldText, newText, got, result)
		}
	}
	if applied == 0 {
		t.Fatal("no run applied an edit")
	}
}
