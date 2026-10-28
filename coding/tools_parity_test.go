package coding

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// ---------------------------------------------------------------------------
// write: exact success string (countless, pi write.ts:87)
// ---------------------------------------------------------------------------

func TestWriteSuccessMessage(t *testing.T) {
	dir := t.TempDir()
	// pi dropped the count entirely (upstream e583b290a): it reported JS
	// `content.length` — UTF-16 code units — mislabeled as bytes. The remaining
	// message is path-only, and pi's result carries `details: undefined`
	// (write.ts:88), so the count cannot come back through the details channel
	// either. "héllo" is 5 code units / 6 UTF-8 bytes, "🎈x" is 3 code units /
	// 5 UTF-8 bytes: no spelling of a length may appear for either.
	for _, tc := range []struct{ path, content string }{
		{"f.txt", "héllo"},
		{"sub/g.txt", "🎈x"},
	} {
		r, err := run(t, writeTool(dir), map[string]any{"path": tc.path, "content": tc.content})
		if err != nil {
			t.Fatalf("write %s: %v", tc.path, err)
		}
		if got, want := resultText(r), "Successfully wrote to "+tc.path; got != want {
			t.Fatalf("write success string\n got: %q\nwant: %q", got, want)
		}
		if r.Details != nil {
			t.Fatalf("write details = %#v, want nil (pi write.ts:88 `details: undefined`)", r.Details)
		}
		// "Successfully wrote to" is only true if the bytes actually landed.
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(tc.path)))
		if err != nil {
			t.Fatalf("read back %s: %v", tc.path, err)
		}
		if string(got) != tc.content {
			t.Fatalf("file contents\n got: %q\nwant: %q", got, tc.content)
		}
	}
}

// A detail-less tool result must leave `details` off the wire entirely: pi sets
// `details: undefined` (write.ts:88, ls.ts:135, find.ts:137, grep.ts:258) or
// never assigns it at all (read.ts:108, both image branches), and JSON.stringify
// drops the key from the session entry and the server bridge. Go's `omitempty`
// elides a nil interface but not a non-nil empty map, so an empty map here would
// serialize as `"details":{}` — a key pi never writes.
func TestDetaillessToolResultOmitsDetailsOnTheWire(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "emptydir")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := minimalPNG()
	for i := len(corrupt) - 20; i < len(corrupt)-8; i++ {
		corrupt[i] ^= 0xFF // valid PNG header, undecodable pixels: processing fails
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.png"), minimalPNG(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.png"), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		tool agent.AgentTool
		args map[string]any
	}{
		{"write", writeTool(dir), map[string]any{"path": "f.txt", "content": "hi"}},
		{"ls-empty", lsTool(dir), map[string]any{"path": "emptydir"}},
		{"find-none", findTool(dir), map[string]any{"pattern": "*.nomatch"}},
		{"grep-none", grepTool(dir), map[string]any{"pattern": "nomatchanywhere"}},
		{"read-image", readTool(dir), map[string]any{"path": "ok.png"}},
		{"read-image-omitted", readTool(dir), map[string]any{"path": "broken.png"}},
	} {
		r, err := run(t, tc.tool, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.Details != nil {
			t.Errorf("%s: details = %#v, want nil (pi leaves it undefined)", tc.name, r.Details)
		}
		raw, err := json.Marshal(ai.ToolResultMessage{
			ToolCallID: "1", ToolName: tc.name, Content: r.Content, Details: r.Details,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if bytes.Contains(raw, []byte(`"details"`)) {
			t.Errorf("%s: result carries a details key pi omits: %s", tc.name, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// grep: format + empty + lines-truncated notice (pi grep.ts:264-265,311,342,352)
// ---------------------------------------------------------------------------

func TestGrepMatchAndContextFormat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\nMATCH\nbeta\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "MATCH", "context": float64(1)})
	text := resultText(r)
	// Match line "path:N: text", context lines "path-N- text".
	if !strings.Contains(text, "a.txt:2: MATCH") {
		t.Fatalf("match line format wrong: %q", text)
	}
	if !strings.Contains(text, "a.txt-1- alpha") || !strings.Contains(text, "a.txt-3- beta") {
		t.Fatalf("context line format wrong: %q", text)
	}
}

// A lone CR is not a line break: line numbers stay rg's, and the CR is
// dropped from the printed line (tools.test.ts, upstream fecc19e4e).
func TestGrepLoneCRKeepsLineNumbers(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lone-cr.txt")
	os.WriteFile(file, []byte("before\rprogress\nneedle\nafter"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle", "path": file, "context": float64(1)})
	text := resultText(r)
	for _, want := range []string{"lone-cr.txt-1- beforeprogress", "lone-cr.txt:2: needle", "lone-cr.txt-3- after"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	// pi strips every CR from a printed line, the match line's too.
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "progress", "path": file})
	if text := resultText(r); text != "lone-cr.txt:1: beforeprogress" {
		t.Fatalf("match line = %q, pi %q", text, "lone-cr.txt:1: beforeprogress")
	}
}

func TestGrepNoMatchesString(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("nothing\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "zzz"})
	if got := resultText(r); got != "No matches found" {
		t.Fatalf("empty grep string: %q", got)
	}
}

func TestGrepLineTruncationNotice(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", GrepMaxLineLength+50) + " needle"
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(long+"\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle"})
	text := resultText(r)
	want := "Some lines truncated to 500 chars. Use read tool to see full lines"
	if !strings.Contains(text, want) {
		t.Fatalf("expected line-truncation notice %q in %q", want, text)
	}
	if !strings.Contains(text, "... [truncated]") {
		t.Fatalf("expected truncated marker in %q", text)
	}
}

func TestGrepMatchLimitNotice(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 5; i++ {
		b.WriteString("hit\n")
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(b.String()), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "hit", "limit": float64(2)})
	text := resultText(r)
	want := "2 matches limit reached. Use limit=4 for more, or refine pattern"
	if !strings.Contains(text, want) {
		t.Fatalf("expected match-limit notice %q in %q", want, text)
	}
}

// ---------------------------------------------------------------------------
// find: empty + limit strings (pi find.ts:294,322)
// ---------------------------------------------------------------------------

func TestFindNoFilesString(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.go"})
	if got := resultText(r); got != "No files found matching pattern" {
		t.Fatalf("empty find string: %q", got)
	}
}

func TestFindResultLimitNotice(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.go", "limit": float64(2)})
	text := resultText(r)
	want := "2 results limit reached. Use limit=4 for more, or refine pattern"
	if !strings.Contains(text, want) {
		t.Fatalf("expected find-limit notice %q in %q", want, text)
	}
}

// find basename-default glob: pattern without "/" matches basename at any depth.
func TestFindBasenameGlob(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "deep"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "deep", "x.ts"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "y.md"), nil, 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.ts"})
	text := resultText(r)
	if !strings.Contains(text, "src/deep/x.ts") {
		t.Fatalf("basename glob should match nested file: %q", text)
	}
	if strings.Contains(text, "y.md") {
		t.Fatalf("should not match .md: %q", text)
	}
}

// find path glob (pattern with "/") matches against full path with fd's leading **/.
func TestFindPathGlob(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "other"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "a", "t.spec.ts"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "other", "t.spec.ts"), nil, 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "src/**/*.spec.ts"})
	text := resultText(r)
	if !strings.Contains(text, "src/a/t.spec.ts") {
		t.Fatalf("path glob should match src subtree: %q", text)
	}
	if strings.Contains(text, "other/t.spec.ts") {
		t.Fatalf("path glob should not match outside src: %q", text)
	}
}

// ---------------------------------------------------------------------------
// ls: errors, case-insensitive sort, symlink-to-dir suffix, empty string
// ---------------------------------------------------------------------------

func TestLsPathNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, lsTool(dir), map[string]any{"path": "nope"})
	if err == nil || !strings.HasPrefix(err.Error(), "Path not found: ") {
		t.Fatalf("expected Path not found error, got %v", err)
	}
}

func TestLsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), nil, 0o644)
	_, err := run(t, lsTool(dir), map[string]any{"path": "f.txt"})
	if err == nil || !strings.HasPrefix(err.Error(), "Not a directory: ") {
		t.Fatalf("expected Not a directory error, got %v", err)
	}
}

func TestLsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	r, err := run(t, lsTool(dir), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(r); got != "(empty directory)" {
		t.Fatalf("empty dir string: %q", got)
	}
}

func TestLsCaseInsensitiveSort(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"Banana", "apple", "Cherry"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	r, _ := run(t, lsTool(dir), map[string]any{})
	lines := strings.Split(strings.TrimSpace(resultText(r)), "\n")
	want := []string{"apple", "Banana", "Cherry"}
	if len(lines) != 3 || lines[0] != want[0] || lines[1] != want[1] || lines[2] != want[2] {
		t.Fatalf("case-insensitive sort wrong: %v", lines)
	}
}

func TestLsSymlinkToDirSuffix(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "realdir")
	os.Mkdir(target, 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	r, _ := run(t, lsTool(dir), map[string]any{})
	text := resultText(r)
	// stat follows the symlink → it is a directory → gets "/".
	if !strings.Contains(text, "link/") {
		t.Fatalf("symlink-to-dir should get / suffix: %q", text)
	}
}

func TestLsEntryLimitNotice(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	r, _ := run(t, lsTool(dir), map[string]any{"limit": float64(2)})
	text := resultText(r)
	want := "2 entries limit reached. Use limit=4 for more"
	if !strings.Contains(text, want) {
		t.Fatalf("expected entry-limit notice %q in %q", want, text)
	}
}
