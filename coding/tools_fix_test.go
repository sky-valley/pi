package coding

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// ---------------------------------------------------------------------------
// I1: per-tool promptGuidelines (byte-exact from pi read.ts:214, edit.ts:299-304,
// write.ts:192)
// ---------------------------------------------------------------------------

func TestToolPromptGuidelines(t *testing.T) {
	dir := t.TempDir()
	want := map[string][]string{
		"read": {"Use read to examine files instead of cat or sed."},
		"edit": {
			"Use edit for precise changes (edits[].oldText must match exactly)",
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		},
		"write": {"Use write only for new files or complete rewrites."},
	}
	for name, guidelines := range want {
		tool, err := CreateTool(name, dir)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tool.PromptGuidelines, guidelines) {
			t.Fatalf("%s PromptGuidelines\n got: %#v\nwant: %#v", name, tool.PromptGuidelines, guidelines)
		}
	}
	// Tools without guidelines in pi must not carry any.
	for _, name := range []string{"bash", "grep", "find", "ls"} {
		tool, _ := CreateTool(name, dir)
		if len(tool.PromptGuidelines) != 0 {
			t.Fatalf("%s should have no PromptGuidelines, got %#v", name, tool.PromptGuidelines)
		}
	}
}

// ---------------------------------------------------------------------------
// H1: edit PrepareArguments wired as the harness pre-validation hook
// ---------------------------------------------------------------------------

func TestEditPrepareArgumentsWired(t *testing.T) {
	tool := editTool(t.TempDir())
	if tool.PrepareArguments == nil {
		t.Fatal("edit tool must set PrepareArguments (harness runs it pre-validation)")
	}
	// Raw stringified edits fail schema validation WITHOUT the hook...
	raw := map[string]any{"path": "f.txt", "edits": `[{"oldText":"a","newText":"b"}]`}
	if _, err := ai.ValidateToolArguments(
		ai.Tool{Name: tool.Name, Parameters: tool.Parameters},
		ai.ToolCall{ID: "id", Name: tool.Name, Arguments: raw},
	); err == nil {
		t.Fatal("stringified edits should fail validation without PrepareArguments")
	}
	// ...and pass WITH it (the loop's order: PrepareArguments → validate).
	prepared := tool.PrepareArguments(raw)
	if _, err := ai.ValidateToolArguments(
		ai.Tool{Name: tool.Name, Parameters: tool.Parameters},
		ai.ToolCall{ID: "id", Name: tool.Name, Arguments: prepared},
	); err != nil {
		t.Fatalf("prepared stringified edits should validate: %v", err)
	}
	// Legacy oldText/newText likewise.
	legacy := tool.PrepareArguments(map[string]any{"path": "f.txt", "oldText": "a", "newText": "b"})
	if _, err := ai.ValidateToolArguments(
		ai.Tool{Name: tool.Name, Parameters: tool.Parameters},
		ai.ToolCall{ID: "id", Name: tool.Name, Arguments: legacy},
	); err != nil {
		t.Fatalf("prepared legacy oldText/newText should validate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// H2: shell selection (shell.ts:66-109) — never $SHELL, never cmd
// ---------------------------------------------------------------------------

func TestGetShellConfigUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell selection")
	}
	// $SHELL must never be consulted.
	t.Setenv("SHELL", "/bin/zsh")
	shell, args, err := getShellConfig()
	if err != nil {
		t.Fatal(err)
	}
	if shell != "/bin/bash" || len(args) != 1 || args[0] != "-c" {
		t.Fatalf("expected /bin/bash -c, got %q %v", shell, args)
	}

	// /bin/bash missing → bash on PATH.
	orig := shellExists
	shellExists = func(string) bool { return false }
	t.Cleanup(func() { shellExists = orig })
	fakeBin := t.TempDir()
	fakeBash := filepath.Join(fakeBin, "bash")
	if err := os.WriteFile(fakeBash, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)
	shell, _, err = getShellConfig()
	if err != nil {
		t.Fatal(err)
	}
	if shell != fakeBash {
		t.Fatalf("expected bash from PATH %q, got %q", fakeBash, shell)
	}

	// No /bin/bash, nothing on PATH → sh.
	t.Setenv("PATH", t.TempDir())
	shell, _, err = getShellConfig()
	if err != nil {
		t.Fatal(err)
	}
	if shell != "sh" {
		t.Fatalf("expected sh fallback, got %q", shell)
	}
}

// ---------------------------------------------------------------------------
// H3: negative/zero limits never panic; read follows pi's JS slice semantics
// ---------------------------------------------------------------------------

func TestReadNegativeLimit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\nd\ne\n"), 0o644)
	// JS: allLines = 6 entries (trailing ""); slice(0, -1) = first 5;
	// userLimitedLines = -1 → remaining 7, nextOffset 0.
	r, err := run(t, readTool(dir), map[string]any{"path": "f.txt", "limit": float64(-1)})
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nb\nc\nd\ne\n\n[7 more lines in file. Use offset=0 to continue.]"
	if got := resultText(r); got != want {
		t.Fatalf("limit=-1\n got: %q\nwant: %q", got, want)
	}
}

func TestReadZeroLimit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\nd\ne\n"), 0o644)
	// JS: slice(0, 0) = [] → empty content; remaining 6, nextOffset 1.
	r, err := run(t, readTool(dir), map[string]any{"path": "f.txt", "limit": float64(0)})
	if err != nil {
		t.Fatal(err)
	}
	want := "\n\n[6 more lines in file. Use offset=1 to continue.]"
	if got := resultText(r); got != want {
		t.Fatalf("limit=0\n got: %q\nwant: %q", got, want)
	}
}

func TestReadNegativeLimitWithOffset(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\nd\ne\n"), 0o644)
	// JS: startLine=2, endLine=min(2-1, 6)=1, slice(2, 1)=[] → empty;
	// userLimitedLines=-1 → remaining 5, nextOffset 2.
	r, err := run(t, readTool(dir), map[string]any{"path": "f.txt", "offset": float64(3), "limit": float64(-1)})
	if err != nil {
		t.Fatal(err)
	}
	want := "\n\n[5 more lines in file. Use offset=2 to continue.]"
	if got := resultText(r); got != want {
		t.Fatalf("offset=3 limit=-1\n got: %q\nwant: %q", got, want)
	}
}

func TestFindLimitZeroUnlimited(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	// fd treats --max-results 0 as unlimited; pi still computes
	// resultLimitReached = len >= 0 → the (odd, but faithful) notice appears.
	r, err := run(t, findTool(dir), map[string]any{"pattern": "*.go", "limit": float64(0)})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(r)
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		if !strings.Contains(text, n) {
			t.Fatalf("limit=0 should be unlimited; missing %s: %q", n, text)
		}
	}
	if !strings.Contains(text, "0 results limit reached") {
		t.Fatalf("expected pi's limit-0 notice: %q", text)
	}
}

func TestFindLimitNegativeNoPanic(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	r, err := run(t, findTool(dir), map[string]any{"pattern": "*.go", "limit": float64(-1)})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(r)
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "b.go") {
		t.Fatalf("negative limit should not slice/panic: %q", text)
	}
	if strings.Contains(text, "limit reached") {
		t.Fatalf("negative limit should not produce a limit notice: %q", text)
	}
}

func TestGrepLimitClampedToOne(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hit\nhit\nhit\n"), 0o644)
	for _, limit := range []float64{0, -1} {
		r, err := run(t, grepTool(dir), map[string]any{"pattern": "hit", "limit": limit})
		if err != nil {
			t.Fatal(err)
		}
		text := resultText(r)
		// pi: Math.max(1, limit) → exactly one match plus the limit notice.
		if !strings.Contains(text, "a.txt:1: hit") {
			t.Fatalf("limit=%v: expected first match: %q", limit, text)
		}
		if strings.Contains(text, "a.txt:2:") {
			t.Fatalf("limit=%v: expected clamp to 1 match: %q", limit, text)
		}
		if !strings.Contains(text, "1 matches limit reached. Use limit=2 for more, or refine pattern") {
			t.Fatalf("limit=%v: expected clamp-to-1 notice: %q", limit, text)
		}
	}
}

// ---------------------------------------------------------------------------
// H6: bash output contract
// ---------------------------------------------------------------------------

func TestBashNoOutputOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, bashTool(dir), map[string]any{"command": "exit 3"})
	if err == nil {
		t.Fatal("expected error")
	}
	// pi: formatOutput substitutes "(no output)" then appendStatus.
	if got, want := err.Error(), "(no output)\n\nCommand exited with code 3"; got != want {
		t.Fatalf("nonzero-exit error text\n got: %q\nwant: %q", got, want)
	}
}

func TestBashSignalKilledIsSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX signals")
	}
	dir := t.TempDir()
	// The shell kills itself → exit code is null in pi (-1 in Go) → success.
	r, err := run(t, bashTool(dir), map[string]any{"command": "echo before; kill -KILL $$"})
	if err != nil {
		t.Fatalf("signal-killed child must be success like pi (exitCode null): %v", err)
	}
	if !strings.Contains(resultText(r), "before") {
		t.Fatalf("expected captured output, got %q", resultText(r))
	}
	// With no output at all, "(no output)" still applies.
	r, err = run(t, bashTool(dir), map[string]any{"command": "kill -KILL $$"})
	if err != nil {
		t.Fatalf("signal-killed child must be success: %v", err)
	}
	if got := resultText(r); got != "(no output)" {
		t.Fatalf("expected (no output), got %q", got)
	}
}

func TestBashFractionalTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	dir := t.TempDir()
	_, err := run(t, bashTool(dir), map[string]any{"command": "sleep 5", "timeout": 0.5})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// pi prints the raw JS number: `Command timed out after 0.5 seconds`.
	if got, want := err.Error(), "Command timed out after 0.5 seconds"; got != want {
		t.Fatalf("fractional timeout\n got: %q\nwant: %q", got, want)
	}
}

func TestBashTempFilePattern(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses seq")
	}
	dir := t.TempDir()
	r, err := run(t, bashTool(dir), map[string]any{"command": "seq 1 20000"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(r)
	m := regexp.MustCompile(`Full output: (\S+)\]`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("expected Full output footer: %q", tail(text))
	}
	base := filepath.Base(m[1])
	// pi: pi-bash-<16 hex>.log
	if !regexp.MustCompile(`^pi-bash-[0-9a-f]{16}\.log$`).MatchString(base) {
		t.Fatalf("temp file name %q does not match pi-bash-<16hex>.log", base)
	}
	data, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	full := string(data)
	if !strings.HasPrefix(full, "1\n2\n") || !strings.Contains(full, "\n20000\n") {
		t.Fatalf("temp file should hold the FULL output; got %d bytes", len(data))
	}
	// Truncation details carry pi's shape.
	details, _ := r.Details.(map[string]any)
	if details == nil || details["fullOutputPath"] != m[1] {
		t.Fatalf("expected details.fullOutputPath, got %#v", r.Details)
	}
	tr, ok := details["truncation"].(TruncationResult)
	if !ok || !tr.Truncated || tr.TotalLines != 20000 {
		t.Fatalf("expected details.truncation with totalLines=20000, got %#v", details["truncation"])
	}
}

func TestBashInitialEmptyUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses echo")
	}
	var mu sync.Mutex
	var first *agent.AgentToolResult
	calls := 0
	onUpdate := func(r agent.AgentToolResult) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if first == nil {
			first = &r
		}
	}
	_, err := bashTool(t.TempDir()).Execute(context.Background(), "id",
		map[string]any{"command": "echo hi"}, onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 || first == nil {
		t.Fatal("expected updates")
	}
	// pi emits an initial empty update before spawning (bash.ts:332-334).
	if len(first.Content) != 0 {
		t.Fatalf("first update should be empty, got %#v", first.Content)
	}
}

// ---------------------------------------------------------------------------
// H8: output accumulator — bounded memory, incremental temp-file writes
// ---------------------------------------------------------------------------

func TestOutputAccumulatorBoundedMemory(t *testing.T) {
	acc := newOutputAccumulator(10, 1024, "pi-test")
	// ~1MB of numbered lines in chunks: memory must stay ~4× maxBytes, the temp
	// file must hold everything, and the snapshot must be the correct tail.
	var written strings.Builder
	line := 0
	for written.Len() < 1<<20 {
		var chunk strings.Builder
		for i := 0; i < 50; i++ {
			line++
			chunk.WriteString("line-")
			chunk.WriteString(strings.Repeat("x", 10))
			chunk.WriteString("-")
			chunk.WriteString(intToStr(line))
			chunk.WriteString("\n")
		}
		acc.append([]byte(chunk.String()))
		written.WriteString(chunk.String())
		if got, max := len(acc.tail), 4*1024+chunk.Len(); got > max {
			t.Fatalf("rolling tail unbounded: %d > %d", got, max)
		}
	}
	acc.finish()
	snap := acc.snapshot(true)
	acc.closeTempFile()
	if !snap.truncation.Truncated {
		t.Fatal("expected truncation")
	}
	if snap.truncation.TotalLines != line {
		t.Fatalf("totalLines: got %d want %d", snap.truncation.TotalLines, line)
	}
	if snap.truncation.TotalBytes != written.Len() {
		t.Fatalf("totalBytes: got %d want %d", snap.truncation.TotalBytes, written.Len())
	}
	// Tail correctness: snapshot content must be the last lines of the output.
	if !strings.HasSuffix(strings.TrimSuffix(written.String(), "\n"), snap.content) {
		t.Fatalf("snapshot is not a suffix of the full output: %q", snap.content)
	}
	if !strings.Contains(snap.content, "-"+intToStr(line)) {
		t.Fatalf("snapshot missing final line %d: %q", line, snap.content)
	}
	// Temp file holds the complete stream.
	data, err := os.ReadFile(snap.fullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != written.String() {
		t.Fatalf("temp file does not hold full output: %d vs %d bytes", len(data), written.Len())
	}
	if !strings.HasPrefix(filepath.Base(snap.fullOutputPath), "pi-test-") {
		t.Fatalf("temp file prefix wrong: %q", snap.fullOutputPath)
	}
	os.Remove(snap.fullOutputPath)
}

func TestOutputAccumulatorSmallOutputUnchanged(t *testing.T) {
	acc := newOutputAccumulator(0, 0, "pi-bash")
	acc.append([]byte("hello "))
	acc.append([]byte("world\n"))
	acc.finish()
	snap := acc.snapshot(true)
	if snap.truncation.Truncated {
		t.Fatal("small output must not truncate")
	}
	if snap.content != "hello world\n" {
		t.Fatalf("content: %q", snap.content)
	}
	if snap.fullOutputPath != "" {
		t.Fatalf("no temp file expected for small output, got %q", snap.fullOutputPath)
	}
	if acc.getLastLineBytes() != 0 {
		t.Fatalf("closed last line should be 0 bytes open, got %d", acc.getLastLineBytes())
	}
}

func intToStr(n int) string { return strconv.Itoa(n) }

// ---------------------------------------------------------------------------
// H9: ls localeCompare ordering (node-verified: _x < .gitignore < ax)
// ---------------------------------------------------------------------------

func TestLsLocaleCompareOrdering(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"ax", ".gitignore", "_x"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	r, err := run(t, lsTool(dir), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(resultText(r)), "\n")
	want := []string{"_x", ".gitignore", "ax"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("localeCompare ordering\n got: %v\nwant: %v", lines, want)
	}
}

// ---------------------------------------------------------------------------
// H10 sweep
// ---------------------------------------------------------------------------

func TestEditENOENTWording(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "x.txt",
		"edits": []any{map[string]any{"oldText": "a", "newText": "b"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "Could not edit file: x.txt. Error code: ENOENT."; got != want {
		t.Fatalf("ENOENT wording\n got: %q\nwant: %q", got, want)
	}
}

func TestReadDirectoryEISDIR(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	_, err := run(t, readTool(dir), map[string]any{"path": "sub"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "EISDIR: illegal operation on a directory, read"; got != want {
		t.Fatalf("EISDIR wording\n got: %q\nwant: %q", got, want)
	}
}

func TestTruncateLineUTF16(t *testing.T) {
	// 499 BMP chars + one astral char: utf16 length is 501 > 500; JS slice(0,500)
	// splits the surrogate pair, leaving a lone high surrogate (→ U+FFFD).
	line := strings.Repeat("x", 499) + "🎈"
	got, was := TruncateLine(line, 0)
	if !was {
		t.Fatal("expected truncation (501 UTF-16 units)")
	}
	if want := strings.Repeat("x", 499) + "�" + "... [truncated]"; got != want {
		t.Fatalf("surrogate split\n got: %q\nwant: %q", got, want)
	}
	// Astral counts as 2 units: balloon + 499 x = 501 units → keeps balloon + 498 x.
	line = "🎈" + strings.Repeat("x", 499)
	got, was = TruncateLine(line, 0)
	if !was {
		t.Fatal("expected truncation")
	}
	if want := "🎈" + strings.Repeat("x", 498) + "... [truncated]"; got != want {
		t.Fatalf("astral counting\n got: %q\nwant: %q", got, want)
	}
	// Exactly 500 units (astral = 2) is NOT truncated even though it is 501 runes... not: 499 runes.
	line = "🎈" + strings.Repeat("x", 498)
	if _, was := TruncateLine(line, 0); was {
		t.Fatal("500 UTF-16 units must not truncate")
	}
}

func TestTrimEndJSWhitespaceSet(t *testing.T) {
	// U+FEFF (ZWNBSP) is JS whitespace → trimmed; U+0085 (NEL) is not.
	if got := normalizeForFuzzyMatch("a\ufeff"); got != "a" {
		t.Fatalf("U+FEFF should be trimmed (JS trimEnd), got %q", got)
	}
	if got := normalizeForFuzzyMatch("a\u0085"); got != "a\u0085" {
		t.Fatalf("U+0085 must NOT be trimmed (JS trimEnd), got %q", got)
	}
}

func TestMutationQueueDrainsEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if _, err := withFileMutationQueue(p, func() (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	mutationMu.Lock()
	n := len(mutationLocks)
	mutationMu.Unlock()
	if n != 0 {
		t.Fatalf("drained mutation queue entries must be deleted; %d remain", n)
	}
}

func TestMutationQueueRealpathErrorPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses symlink loops")
	}
	dir := t.TempDir()
	// Symlink loop → EvalSymlinks fails with ELOOP (not ENOENT/ENOTDIR), which
	// must propagate like pi's getMutationQueueKey re-throw.
	os.Symlink(filepath.Join(dir, "b"), filepath.Join(dir, "a"))
	os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "b"))
	_, err := withFileMutationQueue(filepath.Join(dir, "a", "f.txt"), func() (int, error) { return 1, nil })
	if err == nil {
		t.Fatal("expected realpath error to propagate")
	}
}

func TestReadDetailsTruncation(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < DefaultMaxLines+10; i++ {
		b.WriteString("line\n")
	}
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	details, _ := r.Details.(map[string]any)
	tr, ok := details["truncation"].(TruncationResult)
	if !ok || !tr.Truncated || tr.TruncatedBy != "lines" {
		t.Fatalf("expected details.truncation, got %#v", r.Details)
	}
}
