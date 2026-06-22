package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/agent"
)

// TestEditFuzzyTrailingWhitespace: the model's oldText has different trailing
// whitespace than the file; exact match fails, fuzzy match succeeds (the #1
// real-world edit failure mode).
func TestEditFuzzyTrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	// File has trailing spaces after "foo()".
	os.WriteFile(filepath.Join(dir, "f.go"), []byte("func main() {\n    foo()   \n}\n"), 0o644)
	// Model emits oldText without the trailing spaces.
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.go",
		"edits": []any{map[string]any{"oldText": "    foo()", "newText": "    bar()"}},
	})
	if err != nil {
		t.Fatalf("fuzzy edit should have matched despite trailing whitespace: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if !strings.Contains(string(data), "bar()") {
		t.Fatalf("edit not applied: %q", data)
	}
}

// TestEditFuzzySmartQuotes: file has ASCII quotes, model emits smart quotes.
func TestEditFuzzySmartQuotes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("print(\"hello\")\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "print(“hello”)", "newText": "print(\"world\")"}},
	})
	if err != nil {
		t.Fatalf("fuzzy edit should tolerate smart quotes: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if !strings.Contains(string(data), "world") {
		t.Fatalf("edit not applied: %q", data)
	}
}

func TestEditExactStillPreferredAndPreservesContent(t *testing.T) {
	dir := t.TempDir()
	// Exact match must keep original (non-normalized) surrounding content intact.
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nKEEP   \nb\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "a", "newText": "A"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	// "KEEP   " (with trailing spaces) is untouched because exact match was used.
	if string(data) != "A\nKEEP   \nb\n" {
		t.Fatalf("exact-match edit should not normalize untouched lines: %q", data)
	}
}

// TestEditFuzzyPreservesUntouchedLines (upstream 128330e3): when a fuzzy edit
// rewrites a line, other lines keep their ORIGINAL bytes (e.g. trailing
// whitespace) instead of being globally fuzzy-normalized. The replaced line here
// equals a nearby line, so it also guards against aligning to the wrong one.
func TestEditFuzzyPreservesUntouchedLines(t *testing.T) {
	dir := t.TempDir()
	original := "replace me   \nafter   \n"
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte(original), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "replace me\n", "newText": "after\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	// First line rewritten; the untouched second line keeps its trailing spaces.
	if want := "after\nafter   \n"; string(data) != want {
		t.Fatalf("fuzzy edit must preserve untouched lines: got %q, want %q", data, want)
	}
}

// TestEditFuzzyMultiEditPreservesUntouchedLines: a multi-edit fuzzy operation
// rewrites only its targeted line-blocks and copies every other line back
// verbatim (trailing whitespace on the "keep" lines must survive).
func TestEditFuzzyMultiEditPreservesUntouchedLines(t *testing.T) {
	dir := t.TempDir()
	original := strings.Join([]string{
		"keep before  ",
		"first target  ",
		"first after",
		"keep middle   ",
		"second target  ",
		"second after",
		"keep after  ",
		"",
	}, "\n")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte(original), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "first target\nfirst after", "newText": "FIRST\nFIRST2"},
			map[string]any{"oldText": "second target\nsecond after", "newText": "SECOND\nSECOND2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	want := strings.Join([]string{
		"keep before  ",
		"FIRST",
		"FIRST2",
		"keep middle   ",
		"SECOND",
		"SECOND2",
		"keep after  ",
		"",
	}, "\n")
	if string(data) != want {
		t.Fatalf("multi fuzzy edit must preserve untouched lines:\n got %q\nwant %q", data, want)
	}
}

func TestEditNoChangeError(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "hello", "newText": "hello"}},
	})
	if err == nil || !strings.Contains(err.Error(), "No changes made") {
		t.Fatalf("expected no-change error, got %v", err)
	}
}

func TestEditPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("line1\r\nTARGET\r\nline3\r\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "TARGET", "newText": "CHANGED"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if !strings.Contains(string(data), "\r\n") || !strings.Contains(string(data), "CHANGED") {
		t.Fatalf("CRLF endings not preserved: %q", data)
	}
}

// TestMutationQueueSerializesSameFile: concurrent edits to the same file must
// not corrupt it — each lands, none lost (the queue serializes them).
func TestMutationQueueSerializesSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "counter.txt")
	os.WriteFile(path, []byte("alpha beta gamma delta epsilon\n"), 0o644)
	tool := editTool(dir)

	replacements := map[string]string{
		"alpha": "ALPHA", "beta": "BETA", "gamma": "GAMMA", "delta": "DELTA", "epsilon": "EPSILON",
	}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error
	for old, new := range replacements {
		wg.Add(1)
		go func(old, new string) {
			defer wg.Done()
			_, err := tool.Execute(context.Background(), "id", map[string]any{
				"path":  "counter.txt",
				"edits": []any{map[string]any{"oldText": old, "newText": new}},
			}, func(agent.AgentToolResult) {})
			if err != nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
		}(old, new)
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent edits errored: %v", errs)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	// All five edits must have landed without clobbering each other.
	for _, want := range []string{"ALPHA", "BETA", "GAMMA", "DELTA", "EPSILON"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lost edit %q under concurrency; final: %q", want, got)
		}
	}
}
