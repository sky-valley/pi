package coding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

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

// A lone CR is not a line break: line numbers stay rg's, and the CR stays in
// the printed line (tools.test.ts, upstream fecc19e4e and 8da141c2a).
func TestGrepLoneCRKeepsLineNumbers(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lone-cr.txt")
	os.WriteFile(file, []byte("before\rprogress\nneedle\nafter"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle", "path": file, "context": float64(1)})
	text := resultText(r)
	for _, want := range []string{"lone-cr.txt-1- before\rprogress", "lone-cr.txt:2: needle", "lone-cr.txt-3- after"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "progress", "path": file})
	if text := resultText(r); text != "lone-cr.txt:1: before\rprogress" {
		t.Fatalf("match line = %q, pi %q", text, "lone-cr.txt:1: before\rprogress")
	}
}

// rg matches each line as the file holds it, a CRLF line's \r included (no
// --crlf): "foo$" misses "foo\r\n" and "o\r" hits it. Only pi's display
// lines drop the CR.
func TestGrepMatchesCRLFLinesAsRipgrep(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "crlf.txt")
	os.WriteFile(file, []byte("foo\r\nbar\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "foo$", "path": file})
	if text := resultText(r); text != "No matches found" {
		t.Fatalf(`"foo$" = %q, rg no match`, text)
	}
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": `o\r`, "path": file})
	if text := resultText(r); text != "crlf.txt:1: foo" {
		t.Fatalf(`"o\r" = %q, rg %q`, text, "crlf.txt:1: foo")
	}
}

// rg never sees a line after a file's final \n, nor any line in an empty
// file: an empty-matching pattern hits none there. Rows measured with pi's
// flags (rg --no-config --json --line-number --color=never --hidden -e P,
// ripgrep 14.1.1); with rg on PATH each row is re-measured too.
func TestGrepNoPhantomLineAfterFinalNewline(t *testing.T) {
	rgPath, _ := exec.LookPath("rg")
	for _, tc := range []struct {
		name, content, pattern string
		want                   []int
	}{
		{"blank-line", "a\n\nb\n", "^$", []int{2}},
		{"blank-line", "a\n\nb\n", "x*", []int{1, 2, 3}},
		{"crlf", "foo\r\nbar\r\n", "^$", nil},
		{"crlf", "foo\r\nbar\r\n", "x*", []int{1, 2}},
		{"empty", "", "^$", nil},
		{"empty", "", "x*", nil},
		{"lone-cr", "a\r\rb\n", "^$", nil},
		{"lone-cr", "a\r\rb\n", "x*", []int{1}},
		{"no-final-newline", "a\nb", "^$", nil},
		{"no-final-newline", "a\nb", "x*", []int{1, 2}},
	} {
		dir := t.TempDir()
		file := filepath.Join(dir, "a.txt")
		os.WriteFile(file, []byte(tc.content), 0o644)
		label := tc.name + " " + tc.pattern
		if rgPath != "" {
			if got := rgMatchLines(t, rgPath, tc.pattern, file); !slices.Equal(got, tc.want) {
				t.Errorf("%s: rg matches lines %v, row says %v", label, got, tc.want)
			}
		}
		r, _ := run(t, grepTool(dir), map[string]any{"pattern": tc.pattern, "path": file})
		var got []int
		for _, line := range strings.Split(resultText(r), "\n") {
			var n int
			if _, err := fmt.Sscanf(line, "a.txt:%d:", &n); err == nil {
				got = append(got, n)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: grep matches lines %v, rg %v (%q)", label, got, tc.want, resultText(r))
		}
	}

	// A phantom match must not consume the limit either: rg's 3 matches stay
	// under limit=4.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n\nb\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "x*", "limit": float64(4)})
	if text := resultText(r); strings.Contains(text, "limit reached") {
		t.Errorf("3 real matches tripped limit=4: %q", text)
	}

	// Context comes from rg too (upstream 8da141c2a), so it has no phantom
	// line either.
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "b", "context": float64(2)})
	want := "a.txt-1- a\na.txt-2- \na.txt:3: b"
	if text := resultText(r); text != want {
		t.Errorf("context=2 = %q, pi %q", text, want)
	}
}

// rgMatchLines runs rg with pi's grep flags and returns the matched line numbers.
func rgMatchLines(t *testing.T, rgPath, pattern, file string) []int {
	t.Helper()
	out, err := exec.Command(rgPath, "--no-config", "--json", "--line-number", "--color=never", "--hidden", "-e", pattern, "--", file).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Fatalf("rg %q: %v", pattern, err)
		}
	}
	var lines []int
	for _, raw := range bytes.Split(out, []byte("\n")) {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				LineNumber int `json:"line_number"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.Type == "match" {
			lines = append(lines, ev.Data.LineNumber)
		}
	}
	return lines
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
// grep: context the way ripgrep sends it (tools.test.ts, upstream 8da141c2a)
// ---------------------------------------------------------------------------

// rg merges overlapping context, and after the last allowed match it still
// sends that match's trailing context; a match inside it shows as context
// (#9276).
func TestGrepMergesContextAndKeepsTrailingContextAtLimit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rg-context.txt")
	os.WriteFile(file, []byte(strings.Join([]string{"a", "match one", "b", "match two", "match three", "c", "match four"}, "\n")), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match", "path": file, "context": float64(1), "limit": float64(2)})
	want := strings.Join([]string{
		"rg-context.txt-1- a",
		"rg-context.txt:2: match one",
		"rg-context.txt-3- b",
		"rg-context.txt:4: match two",
		"rg-context.txt-5- match three",
	}, "\n")
	if got := strings.Split(resultText(r), "\n\n")[0]; got != want {
		t.Fatalf("got %q, pi %q", got, want)
	}
}

// With rg on PATH, the rows grep prints with context are exactly the match
// and context events rg sends with pi's flags plus --context N, in order.
func TestGrepContextRowsMatchRipgrep(t *testing.T) {
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg not on PATH")
	}
	content := strings.Join([]string{"m", "a", "b", "m", "c", "d", "e", "f", "m", "m", "g", "h", "i", "j", "k", "m"}, "\n") + "\n"
	for _, n := range []int{1, 2, 3, 4, 20} {
		dir := t.TempDir()
		file := filepath.Join(dir, "f.txt")
		os.WriteFile(file, []byte(content), 0o644)
		out, err := exec.Command(rgPath, "--no-config", "--json", "--line-number", "--color=never", "--hidden", "--context", fmt.Sprint(n), "--", "m", file).Output()
		if err != nil {
			t.Fatalf("rg: %v", err)
		}
		var want []string
		for _, raw := range bytes.Split(out, []byte("\n")) {
			var ev struct {
				Type string `json:"type"`
				Data struct {
					LineNumber int `json:"line_number"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &ev) == nil && (ev.Type == "match" || ev.Type == "context") {
				sep := map[string]string{"match": ":", "context": "-"}[ev.Type]
				want = append(want, fmt.Sprintf("f.txt%s%d%s", sep, ev.Data.LineNumber, sep))
			}
		}
		r, _ := run(t, grepTool(dir), map[string]any{"pattern": "m", "path": file, "context": float64(n)})
		var got []string
		for _, row := range strings.Split(resultText(r), "\n") {
			got = append(got, row[:strings.IndexByte(row, ' ')])
		}
		if !slices.Equal(got, want) {
			t.Errorf("context=%d: grep rows %v, rg %v", n, got, want)
		}
	}
}

func TestGrepKeepsCountingMatchesPastByteCap(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "byte-cap.txt")
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = "match " + strings.Repeat("x", 400)
	}
	os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match", "path": file, "limit": float64(150)})
	text := resultText(r)
	for _, want := range []string{"150 matches limit reached", "50.0KB limit reached"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text[max(0, len(text)-200):])
		}
	}
	details, _ := r.Details.(map[string]any)
	if got := details["matchLimitReached"]; got != 150 {
		t.Fatalf("matchLimitReached = %v, pi 150", got)
	}
	if tr, _ := details["truncation"].(TruncationResult); tr.TotalLines != 150 {
		t.Fatalf("truncation.totalLines = %d, pi 150", tr.TotalLines)
	}
}

func TestGrepReportsTruncationWhenStoredOutputEndsAtByteCap(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "x")
	// 103 rows of 500 bytes plus one of 12 bytes, with newlines, join to
	// exactly 50KB; one more match follows.
	var lines []string
	for range 103 {
		lines = append(lines, strings.Repeat("x", 490))
	}
	lines = append(lines, strings.Repeat("x", 7), "extra x")
	os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "x", "path": file, "limit": float64(1000)})
	if text := resultText(r); !strings.Contains(text, "[50.0KB limit reached]") {
		t.Fatalf("missing byte-cap notice in %q", text[max(0, len(text)-200):])
	}
	details, _ := r.Details.(map[string]any)
	if tr, _ := details["truncation"].(TruncationResult); tr.TotalLines != 105 {
		t.Fatalf("truncation.totalLines = %d, pi 105", tr.TotalLines)
	}
}

func TestGrepAcceptsHugeContext(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "huge-context.txt")
	os.WriteFile(file, []byte("before\nneedle\nafter"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle", "path": file, "context": 1e30})
	want := "huge-context.txt-1- before\nhuge-context.txt:2: needle\nhuge-context.txt-3- after"
	if got := resultText(r); got != want {
		t.Fatalf("got %q, pi %q", got, want)
	}
}

// rg sniffs a UTF-16 BOM and searches the decoded text, so context lines are
// decoded too (#11423).
func TestGrepDecodesUTF16WithContext(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "utf16.txt")
	var data []byte
	for _, u := range utf16.Encode([]rune("\uFEFFbefore\nneedle 中文\nafter\n")) {
		data = append(data, byte(u), byte(u>>8))
	}
	os.WriteFile(file, data, 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle", "path": file, "context": float64(1)})
	want := "utf16.txt-1- before\nutf16.txt:2: needle 中文\nutf16.txt-3- after"
	if got := resultText(r); got != want {
		t.Fatalf("got %q, pi %q", got, want)
	}
}

// rg drops a UTF-8 BOM, searches a UTF-16 file in a directory as text (its
// NULs are gone once decoded), and sends a non-UTF-8 line as bytes that pi
// decodes with U+FFFD replacement.
func TestGrepDecodesLinesAsRipgrep(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bom.txt"), []byte("\xef\xbb\xbfneedle bom\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "latin1.txt"), []byte("caf\xe9 needle\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "utf16be.txt"), []byte("\xfe\xff\x00n\x00e\x00e\x00d\x00l\x00e\x00\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle"})
	got := strings.Split(resultText(r), "\n")
	slices.Sort(got)
	want := []string{"bom.txt:1: needle bom", "latin1.txt:1: caf� needle", "utf16be.txt:1: needle"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, pi %q", got, want)
	}
}

// Only the line terminator is stripped: a lone CR is content, as in read.
func TestGrepKeepsLoneCRAndStripsCRLF(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mixed-cr.txt")
	os.WriteFile(file, []byte("one\rtwo needle\r\nnext\r\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle", "path": file})
	if got, want := resultText(r), "mixed-cr.txt:1: one\rtwo needle"; got != want {
		t.Fatalf("plain = %q, pi %q", got, want)
	}
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "needle", "path": file, "context": float64(1)})
	if got, want := resultText(r), "mixed-cr.txt:1: one\rtwo needle\nmixed-cr.txt-2- next"; got != want {
		t.Fatalf("context = %q, pi %q", got, want)
	}
}

// The limit notice needs proof of one more match than the limit.
func TestGrepNoLimitNoticeWithExactlyLimitMatches(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "exact-limit.txt")
	os.WriteFile(file, []byte(strings.Join([]string{"match one", "x", "match two", "y"}, "\n")), 0o644)
	const notice = "[1 matches limit reached. Use limit=2 for more, or refine pattern]"
	for _, ctx := range []float64{0, 1} {
		r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match", "path": file, "limit": float64(2), "context": ctx})
		if text := resultText(r); strings.Contains(text, "limit reached") {
			t.Errorf("context=%v: exactly limit matches reported the limit: %q", ctx, text)
		}
		if details, _ := r.Details.(map[string]any); details["matchLimitReached"] != nil {
			t.Errorf("context=%v: matchLimitReached = %v, pi undefined", ctx, details["matchLimitReached"])
		}
		r, _ = run(t, grepTool(dir), map[string]any{"pattern": "match", "path": file, "limit": float64(1), "context": ctx})
		if text := resultText(r); !strings.Contains(text, notice) {
			t.Errorf("context=%v: missing %q in %q", ctx, notice, text)
		}
		if details, _ := r.Details.(map[string]any); details["matchLimitReached"] != 1 {
			t.Errorf("context=%v: matchLimitReached = %v, pi 1", ctx, details["matchLimitReached"])
		}
	}
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match", "path": file, "limit": 1.5})
	if text := resultText(r); !strings.Contains(text, notice) {
		t.Errorf("limit=1.5: missing %q in %q", notice, text)
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
