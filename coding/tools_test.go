package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func run(t *testing.T, tool agent.AgentTool, args map[string]any) (agent.AgentToolResult, error) {
	t.Helper()
	return tool.Execute(context.Background(), "id", args, func(agent.AgentToolResult) {})
}

func resultText(r agent.AgentToolResult) string {
	for _, c := range r.Content {
		if tc, ok := c.(ai.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, writeTool(dir), map[string]any{"path": "sub/file.txt", "content": "line1\nline2\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "file.txt")); err != nil {
		t.Fatalf("file not created: %v", err)
	}
	r, err := run(t, readTool(dir), map[string]any{"path": "sub/file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "line1") || !strings.Contains(resultText(r), "line2") {
		t.Fatalf("read content wrong: %q", resultText(r))
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\nd\ne\n"), 0o644)
	r, _ := run(t, readTool(dir), map[string]any{"path": "f.txt", "offset": float64(2), "limit": float64(2)})
	text := resultText(r)
	if !strings.HasPrefix(text, "b\nc") {
		t.Fatalf("offset/limit wrong: %q", text)
	}
	if !strings.Contains(text, "more lines in file") {
		t.Fatalf("expected continuation note: %q", text)
	}
}

func TestEditUniqueReplacement(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path": "f.go",
		"edits": []any{
			map[string]any{"oldText": "func main() {}", "newText": "func main() { println(\"hi\") }"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if !strings.Contains(string(data), "println") {
		t.Fatalf("edit not applied: %s", data)
	}
}

func TestEditDuplicateFails(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\nx\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": "x", "newText": "y"}},
	})
	if err == nil || !strings.Contains(err.Error(), "occurrences") || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("expected pi-style duplicate error, got %v", err)
	}
}

func TestEditOverlapFails(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("abcdef\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "abcd", "newText": "X"},
			map[string]any{"oldText": "cdef", "newText": "Y"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlap error, got %v", err)
	}
}

func TestEditMultipleDisjoint(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("alpha beta gamma\n"), 0o644)
	_, err := run(t, editTool(dir), map[string]any{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "alpha", "newText": "ALPHA"},
			map[string]any{"oldText": "gamma", "newText": "GAMMA"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "ALPHA beta GAMMA\n" {
		t.Fatalf("disjoint edits wrong: %q", data)
	}
}

func TestBashTool(t *testing.T) {
	dir := t.TempDir()
	r, err := run(t, bashTool(dir), map[string]any{"command": "echo hello && pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "hello") {
		t.Fatalf("bash output wrong: %q", resultText(r))
	}
}

func TestBashNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, bashTool(dir), map[string]any{"command": "exit 3"})
	if err == nil || !strings.Contains(err.Error(), "code 3") {
		t.Fatalf("expected exit code 3 error, got %v", err)
	}
}

func TestLsTool(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	r, _ := run(t, lsTool(dir), map[string]any{})
	text := resultText(r)
	if !strings.Contains(text, "a.txt") || !strings.Contains(text, "sub/") {
		t.Fatalf("ls output wrong: %q", text)
	}
}

func TestFindGlob(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main_test.go"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "readme.md"), nil, 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "**/*.go"})
	text := resultText(r)
	if !strings.Contains(text, "src/main.go") || strings.Contains(text, "readme.md") {
		t.Fatalf("find result wrong: %q", text)
	}
}

func TestGrepTool(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("foo\nbar baz\nqux\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("nothing here\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "ba."})
	text := resultText(r)
	// pi grep.ts:264 format: "path:N: text" (space after the second separator).
	if !strings.Contains(text, "a.txt:2: bar baz") {
		t.Fatalf("grep result wrong: %q", text)
	}
}

// Note: this previously pinned grep applying .gitignore outside a git repo;
// that pinned a bug — rg only respects .gitignore inside a repository
// (verified empirically; tracker H4). The repo case lives here, the non-repo
// case in TestGrepGitignoreRequiresRepo.
func TestGrepGitignore(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755) // make it a repo
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("secret match\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("match here\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match"})
	text := resultText(r)
	if strings.Contains(text, "ignored.txt") {
		t.Fatalf("grep should respect .gitignore inside a repo: %q", text)
	}
	if !strings.Contains(text, "visible.txt") {
		t.Fatalf("grep missed visible file: %q", text)
	}
}

func TestToolSchemasValidateViaAgent(t *testing.T) {
	// Each tool's parameters must validate a well-formed call.
	for _, name := range ToolNames {
		tool, err := CreateTool(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if tool.Parameters == nil {
			t.Fatalf("tool %s has nil parameters", name)
		}
		if tool.Description == "" {
			t.Fatalf("tool %s has empty description", name)
		}
	}
}

func TestSystemPromptShape(t *testing.T) {
	p := BuildSystemPrompt(BuildSystemPromptOptions{
		SelectedTools: []string{"read", "bash", "edit", "write"},
		ToolSnippets:  ToolSnippets,
		Cwd:           "/work/project",
	})
	if !strings.Contains(p, "expert coding assistant operating inside pi") {
		t.Fatal("missing preamble")
	}
	if !strings.Contains(p, "- read: Read file contents") {
		t.Fatal("missing tool list")
	}
	if !strings.Contains(p, "Current working directory: /work/project") {
		t.Fatal("missing cwd footer")
	}
}
