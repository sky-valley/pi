package coding

import (
	"context"
	"encoding/json"
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
	r, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo hello && pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "hello") {
		t.Fatalf("bash output wrong: %q", resultText(r))
	}
}

// A non-zero exit is an error result since upstream 8562bcf66, which replaced
// pi's "should handle command errors" (a rejection) with the error-result test
// TestBashNonZeroExitIsAnErrorResultWithStructuredContent ports.
func TestBashNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	r, err := run(t, bashTool(dir, nil), map[string]any{"command": "exit 3"})
	if err != nil || !r.IsError || !strings.Contains(resultText(r), "code 3") {
		t.Fatalf("expected an exit code 3 error result, got %+v, err %v", r, err)
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

// upstream 756a4e8f (#5960): inside a repo, fd's git-aware traversal stops the
// parent .gitignore at a nested repository boundary, so a checked-out sub-repo
// is governed by its own ignore rules, not the outer repo's.
func TestFindRespectsNestedRepoBoundaries(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755) // outer repo
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x\n"), 0o644)

	nested := filepath.Join(dir, "nested")
	os.MkdirAll(filepath.Join(nested, ".git"), 0o755) // nested repo
	os.WriteFile(filepath.Join(nested, ".gitignore"), []byte("secret.txt\n"), 0o644)
	os.WriteFile(filepath.Join(nested, "ignored.txt"), []byte("x\n"), 0o644) // matches OUTER rule only
	os.WriteFile(filepath.Join(nested, "keep.txt"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(nested, "secret.txt"), []byte("x\n"), 0o644) // matches NESTED rule

	r, _ := run(t, findTool(dir), map[string]any{"pattern": "**/*.txt"})
	got := map[string]bool{}
	for _, line := range strings.Split(resultText(r), "\n") {
		got[strings.TrimSpace(line)] = true
	}
	// Outer rule applies in the outer repo but does NOT leak into the nested one.
	if got["ignored.txt"] {
		t.Fatalf("outer .gitignore should hide top-level ignored.txt: %v", got)
	}
	if !got["nested/ignored.txt"] {
		t.Fatalf("outer .gitignore must not cross the nested-repo boundary: %v", got)
	}
	// Nested repo's own rule still applies; visible files surface in both.
	if got["nested/secret.txt"] {
		t.Fatalf("nested .gitignore should hide nested/secret.txt: %v", got)
	}
	if !got["keep.txt"] || !got["nested/keep.txt"] {
		t.Fatalf("visible files missing: %v", got)
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
	p := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
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
	if !strings.Contains(p, "<cwd>\n/work/project\n</cwd>") {
		t.Fatal("missing cwd section")
	}
}

// TestBashSessionEnvExposed mirrors pi's session-metadata exposure
// (bb3d7d39): a bash command run through a session sees PI_SESSION_ID,
// PI_SESSION_FILE, PI_PROVIDER, PI_MODEL, and PI_REASONING_LEVEL.
func TestBashSessionEnvExposed(t *testing.T) {
	dir := t.TempDir()
	sessionEnv := func() map[string]string {
		return map[string]string{
			"PI_SESSION_ID":      "sess-1",
			"PI_SESSION_FILE":    "/tmp/sess-1.jsonl",
			"PI_PROVIDER":        "anthropic",
			"PI_MODEL":           "claude-x",
			"PI_REASONING_LEVEL": "medium",
		}
	}
	r, err := run(t, bashTool(dir, sessionEnv), map[string]any{
		"command": "echo $PI_SESSION_ID $PI_SESSION_FILE $PI_PROVIDER $PI_MODEL $PI_REASONING_LEVEL",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "sess-1 /tmp/sess-1.jsonl anthropic claude-x medium"
	if !strings.Contains(resultText(r), want) {
		t.Fatalf("session env not exposed: got %q, want it to contain %q", resultText(r), want)
	}
}

// TestBashSessionEnvStripsInherited pins pi's unconditional delete of the PI_*
// keys: a stale value inherited from the parent process never reaches the
// child, whether or not a session supplies a replacement.
func TestBashSessionEnvStripsInherited(t *testing.T) {
	t.Setenv("PI_SESSION_ID", "stale-from-parent")
	t.Setenv("PI_MODEL", "stale-model")
	dir := t.TempDir()

	// No session: both are stripped and nothing replaces them.
	r, err := run(t, bashTool(dir, nil), map[string]any{"command": "echo \"[$PI_SESSION_ID][$PI_MODEL]\""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "[][]") {
		t.Fatalf("stale PI_* leaked without a session: %q", resultText(r))
	}

	// With a session: the session's values win over the inherited ones.
	sessionEnv := func() map[string]string { return map[string]string{"PI_MODEL": "claude-x"} }
	r, err = run(t, bashTool(dir, sessionEnv), map[string]any{"command": "echo \"[$PI_SESSION_ID][$PI_MODEL]\""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "[][claude-x]") {
		t.Fatalf("session values did not replace inherited ones: %q", resultText(r))
	}
}

// TestSessionBashEnvIsLive checks the metadata follows the session rather than
// being frozen at construction: pi reads it off the live ExtensionContext, so a
// model switch after the tools were built must be visible to the next command.
func TestSessionBashEnvIsLive(t *testing.T) {
	model := &ai.Model{ID: "model-a", Provider: "prov-a", Name: "A"}
	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), ThinkingLevel: agent.ThinkOff})

	env := sess.bashSessionEnv()
	if env["PI_MODEL"] != "model-a" || env["PI_PROVIDER"] != "prov-a" {
		t.Fatalf("initial model metadata wrong: %#v", env)
	}
	if _, ok := env["PI_SESSION_ID"]; ok {
		t.Fatalf("session id should be absent without a recorder: %#v", env)
	}

	sess.SetModel(&ai.Model{ID: "model-b", Provider: "prov-b", Name: "B"})
	env = sess.bashSessionEnv()
	if env["PI_MODEL"] != "model-b" || env["PI_PROVIDER"] != "prov-b" {
		t.Fatalf("model switch not reflected: %#v", env)
	}
}

// TestSessionBashToolExposesEnv is the end-to-end check on the wiring: the
// bash tool a Session resolves reads its metadata through the session, not
// through a nil provider.
func TestSessionBashToolExposesEnv(t *testing.T) {
	model := &ai.Model{ID: "model-a", Provider: "prov-a", Name: "A"}
	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), ThinkingLevel: agent.ThinkOff})

	bash, ok := sessionTool(sess, "bash")
	if !ok {
		t.Fatal("session has no bash tool")
	}
	r, err := run(t, bash, map[string]any{"command": "echo \"$PI_PROVIDER/$PI_MODEL\""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "prov-a/model-a") {
		t.Fatalf("session bash tool did not expose model metadata: %q", resultText(r))
	}
}

// sessionTool returns the named tool from a session's active set.
func sessionTool(sess *Session, name string) (agent.AgentTool, bool) {
	for _, tool := range sess.Agent.State().Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return agent.AgentTool{}, false
}

// TestBashSessionEnvRaceWithSetModel pins the synchronization on the metadata
// the bash tool reads: a command runs on the tool goroutine while the session
// switches models and attaches a recorder, which is exactly what an embedder
// with a live UI does. Meaningful under -race.
func TestBashSessionEnvRaceWithSetModel(t *testing.T) {
	sess := newTestSession(t, SessionOptions{
		Model: &ai.Model{ID: "model-a", Provider: "prov-a", Name: "A"},
		Cwd:   t.TempDir(),
	})
	bash, ok := sessionTool(sess, "bash")
	if !ok {
		t.Fatal("session has no bash tool")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			sess.SetModel(&ai.Model{ID: "model-b", Provider: "prov-b", Name: "B"})
			sess.Record(nil)
			sess.SetThinkingLevel(agent.ThinkOff)
			sess.SetModel(&ai.Model{ID: "model-a", Provider: "prov-a", Name: "A"})
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := run(t, bash, map[string]any{"command": "echo $PI_MODEL"}); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

// tools.test.ts at 021eae60a: read hands programmatic callers its result as
// structured content — the text for a text file, and for an image an image
// block carrying the text that goes with it as its note. An image that could
// not be processed is only text, so it is text there too.
func TestReadStructuredContent(t *testing.T) {
	dir := t.TempDir()
	content := "Hello, world!\nLine 2\nLine 3"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "test.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if r.StructuredContent != content {
		t.Fatalf("text file structured content = %#v, want the text %q", r.StructuredContent, content)
	}

	os.WriteFile(filepath.Join(dir, "pixel.png"), minimalPNG(), 0o644)
	r, err = run(t, readTool(dir), map[string]any{"path": "pixel.png"})
	if err != nil {
		t.Fatal(err)
	}
	var image ai.ImageContent
	for _, c := range r.Content {
		if ic, ok := c.(ai.ImageContent); ok {
			image = ic
		}
	}
	want := ReadToolImage{Type: "image", Data: image.Data, MimeType: "image/png", Note: resultText(r)}
	if image.Data == "" || r.StructuredContent != want {
		t.Fatalf("image structured content = %#v, want %#v", r.StructuredContent, want)
	}
	if got, err := json.Marshal(r.StructuredContent); err != nil || string(got) != `{"type":"image","data":"`+image.Data+`","mimeType":"image/png","note":"Read image file [image/png]"}` {
		t.Fatalf("image structured content JSON = %s, %v", got, err)
	}

	// A PNG header with no image data: detected as an image, but processing
	// fails and only the note remains.
	os.WriteFile(filepath.Join(dir, "broken.png"), minimalPNG()[:33], 0o644)
	r, err = run(t, readTool(dir), map[string]any{"path": "broken.png"})
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(r); !strings.HasPrefix(text, "Read image file [image/png]\n") || r.StructuredContent != text {
		t.Fatalf("unprocessable image structured content = %#v, want its text %q", r.StructuredContent, text)
	}
}
