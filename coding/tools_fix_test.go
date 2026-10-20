package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// ---------------------------------------------------------------------------
// I1: per-tool promptGuidelines (byte-exact from pi read.ts:214, bash.ts:329,
// edit.ts:299-304, write.ts:192)
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
		// pi bb3d7d39: bash advertises the PI_* session metadata it exports.
		"bash": {"You can inspect PI_* environment variables for current model and session details."},
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
	for _, name := range []string{"grep", "find", "ls"} {
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

// pi ca21c1686: some models send a single {oldText,newText} edit object — raw
// or JSON-stringified — instead of a one-element edits array; both normalize to
// [that edit]. Shapes that are neither an array nor a single edit stay
// untouched (and then fail schema validation, as upstream).
func TestEditPrepareArgumentsSingleEditObject(t *testing.T) {
	single := map[string]any{"oldText": "a", "newText": "b"}

	got := prepareEditArguments(map[string]any{"path": "f.txt", "edits": single})
	if arr, ok := got["edits"].([]any); !ok || len(arr) != 1 ||
		arr[0].(map[string]any)["oldText"] != "a" || arr[0].(map[string]any)["newText"] != "b" {
		t.Fatalf("raw single edit object not wrapped: %#v", got["edits"])
	}

	got = prepareEditArguments(map[string]any{"path": "f.txt", "edits": `{"oldText":"a","newText":"b"}`})
	if arr, ok := got["edits"].([]any); !ok || len(arr) != 1 ||
		arr[0].(map[string]any)["oldText"] != "a" || arr[0].(map[string]any)["newText"] != "b" {
		t.Fatalf("stringified single edit object not wrapped: %#v", got["edits"])
	}

	// Not a single edit: oldText is not a string — passes through untouched.
	bad := map[string]any{"oldText": 1, "newText": "b"}
	got = prepareEditArguments(map[string]any{"path": "f.txt", "edits": bad})
	if _, ok := got["edits"].([]any); ok {
		t.Fatalf("non-edit object must not be wrapped: %#v", got["edits"])
	}

	// A stringified object that is not a single edit stays the string.
	got = prepareEditArguments(map[string]any{"path": "f.txt", "edits": `{"other":true}`})
	if _, ok := got["edits"].(string); !ok {
		t.Fatalf("stringified non-edit must stay a string: %#v", got["edits"])
	}

	// The wrapped shape validates against the tool schema end-to-end.
	tool := editTool(t.TempDir())
	prepared := tool.PrepareArguments(map[string]any{"path": "f.txt", "edits": single})
	if _, err := ai.ValidateToolArguments(
		ai.Tool{Name: tool.Name, Parameters: tool.Parameters},
		ai.ToolCall{ID: "id", Name: tool.Name, Arguments: prepared},
	); err != nil {
		t.Fatalf("prepared single edit object should validate: %v", err)
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
	shell, args, _, err := getShellConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if shell != "/bin/bash" || len(args) != 1 || args[0] != "-c" {
		t.Fatalf("expected /bin/bash -c, got %q %v", shell, args)
	}

	// /bin/bash missing → bash on PATH.
	noBinBash := localShellHost
	noBinBash.exists = func(string) bool { return false }
	fakeBin := t.TempDir()
	fakeBash := filepath.Join(fakeBin, "bash")
	if err := os.WriteFile(fakeBash, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)
	shell, _, _, err = noBinBash.shellConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if shell != fakeBash {
		t.Fatalf("expected bash from PATH %q, got %q", fakeBash, shell)
	}

	// No /bin/bash, nothing on PATH → sh.
	t.Setenv("PATH", t.TempDir())
	shell, _, _, err = noBinBash.shellConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if shell != "sh" {
		t.Fatalf("expected sh fallback, got %q", shell)
	}
}

// TestLegacyWslBashDetection (upstream 1287b69f): only the Windows-bundled WSL
// launcher (System32/Sysnative bash.exe) is detected, and it switches the bash
// tool to `bash -s` + stdin transport. All other bash paths keep `bash -c`.
func TestLegacyWslBashDetection(t *testing.T) {
	legacy := []string{
		`C:\Windows\System32\bash.exe`,
		`C:/Windows/System32/bash.exe`,
		`c:\windows\system32\bash.exe`,
		`C:\Windows\Sysnative\bash.exe`,
	}
	for _, p := range legacy {
		if !isLegacyWslBashPath(p) {
			t.Fatalf("%q should be detected as legacy WSL bash", p)
		}
		shell, args, useStdin := getBashShellConfig(p)
		if shell != p || !useStdin || !reflect.DeepEqual(args, []string{"-s"}) {
			t.Fatalf("%q: got shell=%q args=%v stdin=%v, want -s + stdin", p, shell, args, useStdin)
		}
	}

	normal := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`/bin/bash`,
		`C:\Windows\System32\wsl.exe`,
		`D:\tools\bash.exe`,
		`C:\Windows\System32\drivers\bash.exe`,
	}
	for _, p := range normal {
		if isLegacyWslBashPath(p) {
			t.Fatalf("%q should NOT be detected as legacy WSL bash", p)
		}
		shell, args, useStdin := getBashShellConfig(p)
		if shell != p || useStdin || !reflect.DeepEqual(args, []string{"-c"}) {
			t.Fatalf("%q: got shell=%q args=%v stdin=%v, want -c + argv", p, shell, args, useStdin)
		}
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
	res, err := run(t, bashTool(dir, nil), map[string]any{"command": "exit 3"})
	if err != nil || !res.IsError {
		t.Fatalf("a non-zero exit must be an error result (upstream 8562bcf66), got IsError=%v err=%v", res.IsError, err)
	}
	// pi: formatOutput substitutes "(no output)" then appendStatus.
	if got, want := resultText(res), "(no output)\n\nCommand exited with code 3"; got != want {
		t.Fatalf("nonzero-exit error text\n got: %q\nwant: %q", got, want)
	}
}

// Upstream 8562bcf66 (tools.test.ts "should report non-zero exit codes as
// error results with structured content"): a non-zero exit is an error result
// rather than a thrown error, so it keeps its structured content — the
// output as the model sees it before the status line, the exit code and the
// wall time — and a success carries the same record.
func TestBashNonZeroExitIsAnErrorResultWithStructuredContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	dir := t.TempDir()
	res, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo out; exit 3"})
	if err != nil || !res.IsError {
		t.Fatalf("got IsError=%v err=%v, want an error result", res.IsError, err)
	}
	if got, want := resultText(res), "out\n\n\nCommand exited with code 3"; got != want {
		t.Fatalf("text\n got: %q\nwant: %q", got, want)
	}
	out, ok := res.StructuredContent.(BashToolOutput)
	if !ok || out.Output != "out\n" || out.Truncated || out.FullOutputPath != "" || out.ExitCode != 3 || out.WallTimeSeconds < 0 {
		t.Fatalf("structured content = %#v, want {output: \"out\\n\", truncated: false, exit_code: 3, wall_time_seconds: >= 0}", res.StructuredContent)
	}
	if res.Details != nil {
		t.Fatalf("details = %#v, want none for untruncated output (pi's undefined)", res.Details)
	}

	ok2, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo fine"})
	if err != nil || ok2.IsError {
		t.Fatalf("success got IsError=%v err=%v", ok2.IsError, err)
	}
	if out, ok := ok2.StructuredContent.(BashToolOutput); !ok || out.Output != "fine\n" || out.ExitCode != 0 {
		t.Fatalf("success structured content = %#v, want {output: \"fine\\n\", exit_code: 0}", ok2.StructuredContent)
	}

	// Upstream 1ff5b6fdd: no output is "(no output)" for the model and "" for
	// a script.
	empty, err := run(t, bashTool(dir, nil), map[string]any{"command": "true"})
	if err != nil || empty.IsError {
		t.Fatalf("empty got IsError=%v err=%v", empty.IsError, err)
	}
	if got := resultText(empty); got != "(no output)" {
		t.Fatalf("empty text = %q, want (no output)", got)
	}
	if out, ok := empty.StructuredContent.(BashToolOutput); !ok || out.Output != "" || out.Truncated {
		t.Fatalf("empty structured content = %#v, want {output: \"\", truncated: false}", empty.StructuredContent)
	}
}

// Upstream 1ff5b6fdd (tools.test.ts "should return up to 1 MiB of output in
// structured content"): a script gets the whole output up to 1 MiB, past the
// model-facing limits, and longer output as its first and last 512 KiB.
func TestBashStructuredContentUpTo1MiB(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses seq")
	}
	t.Setenv("TMPDIR", t.TempDir()) // the full-output files land here
	dir := t.TempDir()

	// 3000 lines exceed the model-facing 2000 line limit but not 1 MiB.
	medium, err := run(t, bashTool(dir, nil), map[string]any{"command": "seq 1 3000"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText(medium), "\n1\n2\n") {
		t.Error("the model-facing text should be cut to its last lines")
	}
	details, _ := medium.Details.(map[string]any)
	if tr, _ := details["truncation"].(TruncationResult); !tr.Truncated {
		t.Errorf("details.truncation = %#v, want truncated", details["truncation"])
	}
	var want strings.Builder
	for i := 1; i <= 3000; i++ {
		want.WriteString(strconv.Itoa(i) + "\n")
	}
	mediumOut, _ := medium.StructuredContent.(BashToolOutput)
	if mediumOut.Truncated || mediumOut.Output != want.String() {
		t.Errorf("structured content truncated=%t, %d bytes; want the whole %d-byte output", mediumOut.Truncated, len(mediumOut.Output), want.Len())
	}

	// About 2 MB: the first and last 512 KiB around an omission marker.
	large, err := run(t, bashTool(dir, nil), map[string]any{"command": "seq 1 300000"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := large.StructuredContent.(BashToolOutput)
	if !out.Truncated {
		t.Fatal("structured content should be truncated")
	}
	if !strings.HasPrefix(out.Output, "1\n2\n3\n") || !strings.HasSuffix(out.Output, "299999\n300000\n") {
		t.Errorf("output should keep both ends: %q ... %q", out.Output[:20], out.Output[len(out.Output)-20:])
	}
	if !regexp.MustCompile(`\n\n\[\.\.\. \d+ bytes omitted \.\.\.\]\n\n`).MatchString(out.Output) {
		t.Error("output has no omission marker")
	}
	if len(out.Output) >= 1024*1024+100 {
		t.Errorf("output is %d bytes, want under 1 MiB + 100", len(out.Output))
	}
	largeDetails, _ := large.Details.(map[string]any)
	if out.FullOutputPath == "" || out.FullOutputPath != largeDetails["fullOutputPath"] {
		t.Errorf("full_output_path = %q, want details.fullOutputPath %v", out.FullOutputPath, largeDetails["fullOutputPath"])
	}
	if full, err := os.ReadFile(out.FullOutputPath); err != nil || !strings.HasSuffix(string(full), "300000\n") {
		t.Errorf("the full output file should end with 300000 (err %v)", err)
	}
}

// When the full output cannot be saved, the shell tool fails the call with the
// cause and how to fix it, and that error comes first: pi's finishOutput
// awaits closeTempFile before it looks at an abort or a timeout (D90).
func TestBashFailsWhenTheFullOutputCannotBeSaved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses seq")
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	dir := t.TempDir()
	for _, tc := range []struct{ name, command string }{
		{"the command succeeds", "seq 1 3000"},
		{"the command times out", "seq 1 3000; sleep 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{"command": tc.command}
			if strings.Contains(tc.command, "sleep") {
				params["timeout"] = 1
			}
			_, err := run(t, bashTool(dir, nil), params)
			if err == nil {
				t.Fatal("the call succeeded; it should fail when the full output cannot be saved")
			}
			for _, want := range []string{"could not save the full command output", "no such file or directory", "point TMPDIR"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
		})
	}
}

// The wall time is pi's Math.round(ms / 100) / 10: tenths of a second.
func TestBashWallTimeSeconds(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		want    float64
	}{
		{0, 0}, {49900 * time.Microsecond, 0}, {50 * time.Millisecond, 0.1}, {149990 * time.Microsecond, 0.1},
		{150 * time.Millisecond, 0.2}, {1234 * time.Millisecond, 1.2}, {1250 * time.Millisecond, 1.3},
	} {
		if got := bashWallTimeSeconds(tc.elapsed); got != tc.want {
			t.Errorf("bashWallTimeSeconds(%v) = %v, want %v", tc.elapsed, got, tc.want)
		}
	}
}

// pi a8b3dd199 (#9577), renamed by 8562bcf66 'should report signal-killed
// commands as errors while preserving partial output': a signal termination is
// 128 + the signal number, so the command FAILS — an error result since
// 8562bcf66 — and the output produced before the signal is kept.
func TestBashSignalKilledFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX signals")
	}
	dir := t.TempDir()
	for _, tt := range []struct {
		signal string
		exit   int
	}{
		{signal: "KILL", exit: 137},
		{signal: "TERM", exit: 143},
	} {
		t.Run(tt.signal, func(t *testing.T) {
			res, err := run(t, bashTool(dir, nil), map[string]any{
				"command": "printf 'before-kill\n'; kill -" + tt.signal + " $$",
			})
			if err != nil || !res.IsError {
				t.Fatalf("a signal-killed command must be an error result, got IsError=%v err=%v", res.IsError, err)
			}
			// pi asserts this with a whitespace-tolerant regex, so the port
			// does too: the blank-line convention between output and status is
			// appendStatus's, pinned elsewhere, and not what this case is about.
			want := regexp.MustCompile(fmt.Sprintf(`before-kill\s+Command exited with code %d$`, tt.exit))
			if got := resultText(res); !want.MatchString(got) {
				t.Fatalf("signal-killed error text %q does not match %v", got, want)
			}
		})
	}
	// With no output at all, "(no output)" still applies.
	res, err := run(t, bashTool(dir, nil), map[string]any{"command": "kill -KILL $$"})
	if err != nil || !res.IsError {
		t.Fatalf("a signal-killed command must be an error result, got IsError=%v err=%v", res.IsError, err)
	}
	if got, want := resultText(res), "(no output)\n\nCommand exited with code 137"; got != want {
		t.Fatalf("no-output signal error text\n got: %q\nwant: %q", got, want)
	}
}

func TestBashFractionalTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	dir := t.TempDir()
	_, err := run(t, bashTool(dir, nil), map[string]any{"command": "sleep 5", "timeout": 0.5})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// pi prints the raw JS number: `Command timed out after 0.5 seconds`.
	if got, want := err.Error(), "Command timed out after 0.5 seconds"; got != want {
		t.Fatalf("fractional timeout\n got: %q\nwant: %q", got, want)
	}
}

func TestBashRejectsNonPositiveTimeout(t *testing.T) {
	dir := t.TempDir()
	// pi resolveTimeoutMs rejects <= 0 (and non-finite) before spawning; the raw
	// error surfaces as the tool result unchanged (bash.ts).
	for _, tv := range []any{0, 0.0, -1, -0.5} {
		_, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo hi", "timeout": tv})
		if err == nil {
			t.Fatalf("timeout=%v: expected rejection", tv)
		}
		if got, want := err.Error(), "Invalid timeout: must be a finite number of seconds"; got != want {
			t.Fatalf("timeout=%v\n got: %q\nwant: %q", tv, got, want)
		}
	}
}

func TestBashRejectsOversizedTimeout(t *testing.T) {
	dir := t.TempDir()
	// timeout * 1000 must not exceed INT32_MAX ms; the message renders
	// MAX_TIMEOUT_SECONDS (2147483.647) byte-identically to pi.
	_, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo hi", "timeout": 2147484.0})
	if err == nil {
		t.Fatal("expected oversized-timeout rejection")
	}
	if got, want := err.Error(), "Invalid timeout: maximum is 2147483.647 seconds"; got != want {
		t.Fatalf("oversized timeout\n got: %q\nwant: %q", got, want)
	}
	// The exact boundary (2147483647 ms) is accepted, not rejected.
	if _, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo hi", "timeout": 2147483.647}); err != nil {
		t.Fatalf("boundary timeout must be accepted: %v", err)
	}
}

func TestBashTempFilePattern(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses seq")
	}
	dir := t.TempDir()
	r, err := run(t, bashTool(dir, nil), map[string]any{"command": "seq 1 20000"})
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
	_, err := bashTool(t.TempDir(), nil).Execute(context.Background(), "id",
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

// TestRelativizeFindResultPath locks pi #6104: find results are relativized
// against the search root, including a filesystem root (where pi's prefix slice
// used to eat the first character of the first segment) and absolute results
// that are not prefixed by the root at all.
func TestRelativizeFindResultPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix separators")
	}
	cases := []struct{ result, search, want string }{
		{"/home/user/file.txt", "/", "home/user/file.txt"},
		{"/home/user/project", "/", "home/user/project"},
		{"/home/user/project/file.txt", "/home/user", "project/file.txt"},
		{"/ai/Models2/file.txt", "/ai/Models", "../Models2/file.txt"},
		{"/tmp/results/file.txt", "/workspace/project", "../../tmp/results/file.txt"},
		// fd marks directory results with a trailing separator, and pi carries it
		// across relativization (find.ts: hadTrailingSlash).
		{"/home/user/project/", "/", "home/user/project/"},
		{"/home/user/project/src/", "/home/user/project", "src/"},
		{"/ai/Models2/", "/ai/Models", "../Models2/"},
	}
	for _, c := range cases {
		if got := relativizeFindResultPath(c.result, c.search); got != c.want {
			t.Errorf("relativizeFindResultPath(%q, %q) = %q, want %q", c.result, c.search, got, c.want)
		}
	}
}

// TestFindRelativizesAgainstSearchRoot exercises the same relativization through
// the find tool itself.
func TestFindRelativizesAgainstSearchRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "nested", "a.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := findTool(dir).Execute(context.Background(), "id", map[string]any{"pattern": "**/*.ts"}, func(agent.AgentToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(ai.TextContent).Text
	if got != "src/nested/a.ts" {
		t.Fatalf("find output = %q, want %q", got, "src/nested/a.ts")
	}
}

// TestFindMarksDirectoryResults locks fd's directory marker: fd emits a trailing
// separator for directory results and pi carries it into the tool output
// (verified against fd 10.4.2 with pi's argv: "src/", "src/nested/").
func TestFindMarksDirectoryResults(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "nested", "a.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := findTool(dir).Execute(context.Background(), "id", map[string]any{"pattern": "**"}, func(agent.AgentToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(ai.TextContent).Text
	want := "src/\nsrc/nested/\nsrc/nested/a.ts"
	if got != want {
		t.Fatalf("find output = %q, want %q", got, want)
	}
}

// TestGetShellConfigCustomShellPath ports shell.ts getShellConfig's first step:
// an explicit shellPath wins over every platform default when it exists, and
// a missing one is an error rather than a silent fallback.
func TestGetShellConfigCustomShellPath(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "mybash")
	if err := os.WriteFile(custom, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shell, args, useStdin, err := getShellConfig(custom)
	if err != nil {
		t.Fatal(err)
	}
	if shell != custom || useStdin || !reflect.DeepEqual(args, []string{"-c"}) {
		t.Fatalf("got shell=%q args=%v stdin=%v, want %q -c", shell, args, useStdin, custom)
	}

	missing := filepath.Join(t.TempDir(), "missing", "bash")
	_, _, _, err = getShellConfig(missing)
	if err == nil {
		t.Fatalf("missing custom shell path: want an error, got none")
	}
	if got, want := err.Error(), "Custom shell path not found: "+missing; got != want {
		t.Fatalf("error\n got: %q\nwant: %q", got, want)
	}
}
