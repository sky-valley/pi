package coding

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-valley/pi/agent"
)

// splitBOM/stripBOM mirror pi's splitBom/stripBom (utils/text.ts): exactly one
// leading U+FEFF comes off, a BOM anywhere else is ordinary text.
func TestSplitAndStripBOM(t *testing.T) {
	cases := []struct {
		name string
		in   string
		bom  string
		text string
	}{
		{"leading bom", "\ufeffcontent", "\ufeff", "content"},
		{"no bom", "content", "", "content"},
		{"empty", "", "", ""},
		{"bom only", "\ufeff", "\ufeff", ""},
		// pi slices ONE UTF-16 code unit, so a doubled BOM keeps the second.
		{"double bom", "\ufeff\ufeffcontent", "\ufeff", "\ufeffcontent"},
		// U+FEFF away from the start is ZWNBSP content, not a BOM.
		{"interior bom", "a\ufeffb", "", "a\ufeffb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bom, text := splitBOM(tc.in)
			if bom != tc.bom || text != tc.text {
				t.Fatalf("splitBOM(%q) = (%q, %q), want (%q, %q)", tc.in, bom, text, tc.bom, tc.text)
			}
			if got := stripBOM(tc.in); got != tc.text {
				t.Fatalf("stripBOM(%q) = %q, want %q", tc.in, got, tc.text)
			}
		})
	}
}

// The edit tool matches against the de-BOM'd text (the model never emits an
// invisible BOM in oldText) and re-prepends the BOM when writing back, so a
// BOM-prefixed file stays BOM-prefixed. Locks the behavior splitBOM serves at
// its only editing call site (pi edit.ts:361-370).
//
// Both want values were produced by pi's own edit path \u2014 utils/text.ts splitBom
// plus core/tools/edit-diff.ts extracted at 1355cd36e and executed in node
// through edit.ts's exact sequence (splitBom \u2192 detectLineEnding \u2192 normalizeToLF
// \u2192 applyEditsToNormalizedContent \u2192 bom + restoreLineEndings).
// Upstream d448c51e2 replaced restoreLineEndings with a splice into the
// original text; neither file holds a CR, so both values stand.
func TestEditToolPreservesLeadingBOM(t *testing.T) {
	cases := []struct {
		name    string
		initial string
		oldText string
		newText string
		want    string
	}{
		// Exact match. The BOM shares line 1 with the matched text, so it rides
		// along at offset 0 and comes back out at offset 0 whether or not it was
		// ever split off \u2014 this case cannot tell splitBOM from a no-op.
		{
			name:    "exact match on the BOM line",
			initial: "\ufeffhello world\n",
			oldText: "hello",
			newText: "goodbye",
			want:    "\ufeffgoodbye world\n",
		},
		// Fuzzy match touching a BOM-only first line \u2014 the shape where the split
		// is load-bearing. The trailing space in oldText defeats the exact match,
		// and normalizeForFuzzyMatch trims U+FEFF as JS whitespace (jstext.TrimEnd),
		// so a BOM-only line normalizes to empty. The touched line is then
		// rewritten from that fuzzy view, and only the BOM held aside by splitBOM
		// survives to be re-prepended. Match on the unsplit text and the file
		// silently loses its BOM.
		{
			name:    "fuzzy match touching a BOM-only line",
			initial: "\ufeff\nA\n",
			oldText: "\nA ",
			newText: "\nB",
			want:    "\ufeff\nB\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bom.txt")
			if err := os.WriteFile(path, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := editTool(dir).Execute(context.Background(), "id", map[string]any{
				"path":  "bom.txt",
				"edits": []any{map[string]any{"oldText": tc.oldText, "newText": tc.newText}},
			}, func(agent.AgentToolResult) {})
			if err != nil {
				t.Fatalf("edit failed: %v", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("edited %q = %q, want %q", tc.initial, string(got), tc.want)
			}
		})
	}
}
