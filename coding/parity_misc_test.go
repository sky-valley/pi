package coding

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// edit: detectLineEnding (CRLF only if first CRLF precedes first bare LF)
// ---------------------------------------------------------------------------

func TestDetectLineEndingMixed(t *testing.T) {
	// "a\nb\r\nc": first bare LF (idx 1) precedes first CRLF (idx 3) → LF.
	if got := detectLineEnding("a\nb\r\nc"); got != "\n" {
		t.Fatalf("mixed EOL with bare LF first should stay LF, got %q", got)
	}
	// CRLF first → CRLF.
	if got := detectLineEnding("a\r\nb\nc"); got != "\r\n" {
		t.Fatalf("CRLF first should detect CRLF, got %q", got)
	}
	// Pure CRLF.
	if got := detectLineEnding("a\r\nb\r\n"); got != "\r\n" {
		t.Fatalf("pure CRLF, got %q", got)
	}
	// No newline.
	if got := detectLineEnding("abc"); got != "\n" {
		t.Fatalf("no newline → LF, got %q", got)
	}
}

// A file whose first line ends LF but later has CRLF keeps LF on write.
func TestEditMixedEOLStaysLF(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nTARGET\r\nc"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "TARGET", "newText": "X"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	// detected ending is LF, so the surviving CRLF before "c" becomes LF.
	if strings.Contains(string(data), "\r\n") {
		t.Fatalf("LF-detected file should not gain CRLF: %q", data)
	}
}

// ---------------------------------------------------------------------------
// edit: prepareArguments — stringified edits + legacy oldText/newText
// ---------------------------------------------------------------------------

func TestEditStringifiedEdits(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello world\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": `[{"oldText":"world","newText":"there"}]`, // JSON string, not array
	})
	if err != nil {
		t.Fatalf("stringified edits should be parsed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "hello there\n" {
		t.Fatalf("stringified edit not applied: %q", data)
	}
}

func TestEditLegacyOldNewText(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("foo bar\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":    "f.txt",
		"oldText": "bar",
		"newText": "baz",
	})
	if err != nil {
		t.Fatalf("legacy oldText/newText should fold into edits[]: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "foo baz\n" {
		t.Fatalf("legacy edit not applied: %q", data)
	}
}

// NFKC fuzzy match: full-width chars in the file match an ASCII oldText.
func TestEditNFKCFuzzyMatch(t *testing.T) {
	dir := t.TempDir()
	// "ＡＢＣ" are full-width A B C (U+FF21..). NFKC folds them to "ABC".
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x ＡＢＣ y\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "ABC", "newText": "Z"}},
	})
	if err != nil {
		t.Fatalf("NFKC fuzzy match should match full-width vs ascii: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if !strings.Contains(string(data), "Z") {
		t.Fatalf("NFKC edit not applied: %q", data)
	}
}

// ---------------------------------------------------------------------------
// resolveToCwd: ~, @, unicode-space (path-utils.ts:48-50)
// ---------------------------------------------------------------------------

func TestResolveToCwdTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	got := resolveToCwd("~/sub/f.txt", "/base")
	want := filepath.Join(home, "sub", "f.txt")
	if got != want {
		t.Fatalf("~ expansion: got %q want %q", got, want)
	}
}

func TestResolveToCwdAtPrefix(t *testing.T) {
	got := resolveToCwd("@rel/f.txt", "/base")
	want := filepath.Clean("/base/rel/f.txt")
	if got != want {
		t.Fatalf("@ strip: got %q want %q", got, want)
	}
}

func TestResolveToCwdUnicodeSpace(t *testing.T) {
	// A non-breaking space (U+00A0) in the input becomes a regular space.
	got := resolveToCwd("my file.txt", "/base")
	want := filepath.Clean("/base/my file.txt")
	if got != want {
		t.Fatalf("unicode space fold: got %q want %q", got, want)
	}
}

// resolveReadPath macOS curly-quote fallback resolves to an existing file.
func TestResolveReadPathCurlyQuoteFallback(t *testing.T) {
	dir := t.TempDir()
	// Real file uses the curly apostrophe U+2019.
	real := filepath.Join(dir, "d’etat.txt")
	os.WriteFile(real, []byte("x"), 0o644)
	// User types a straight apostrophe.
	got := resolveReadPath("d'etat.txt", dir)
	if got != real {
		t.Fatalf("curly-quote fallback: got %q want %q", got, real)
	}
}

// ---------------------------------------------------------------------------
// bash: partial-last-line footer (single giant line, pi bash.ts:358-360)
// ---------------------------------------------------------------------------

func TestBashPartialLastLineFooter(t *testing.T) {
	dir := t.TempDir()
	// One giant line with no newlines, larger than the 50KB byte cap.
	size := DefaultMaxBytes + 5000
	r, err := run(t, bashTool(dir), map[string]any{
		"command": "printf 'x%.0s' $(seq 1 " + strconv.Itoa(size) + ")",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(r)
	if !strings.Contains(text, "[Showing last ") || !strings.Contains(text, " of line 1 (line is ") {
		t.Fatalf("expected partial-last-line footer, got tail: %q", tail(text))
	}
	if !strings.Contains(text, "Full output: ") {
		t.Fatalf("expected Full output path in footer: %q", tail(text))
	}
}

func tail(s string) string {
	if len(s) <= 300 {
		return s
	}
	return "..." + s[len(s)-300:]
}
