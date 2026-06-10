package coding

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGitGlobals points git's global config/excludes at empty locations so
// the developer's real global gitignore cannot leak into repo-based tests.
func isolateGitGlobals(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func mkRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Glob primitive semantics (fd/globset: braces, [!x], smart-case, **)
// ---------------------------------------------------------------------------

func TestMatchFdGlobTable(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		abs     string
		want    bool
	}{
		// basename matching for patterns without "/"
		{"*.ts", "src/deep/x.ts", "/r/src/deep/x.ts", true},
		{"*.ts", "y.md", "/r/y.md", false},
		// brace alternation
		{"*.{go,md}", "a.go", "/r/a.go", true},
		{"*.{go,md}", "b.md", "/r/b.md", true},
		{"*.{go,md}", "c.ts", "/r/c.ts", false},
		{"{a,b}*.txt", "alpha.txt", "/r/alpha.txt", true},
		{"{a,b}*.txt", "beta.txt", "/r/beta.txt", true},
		{"{a,b}*.txt", "gamma.txt", "/r/gamma.txt", false},
		// negated character classes
		{"[!a]*.go", "b.go", "/r/b.go", true},
		{"[!a]*.go", "ab.go", "/r/ab.go", false},
		// smart-case: all-lowercase pattern is case-insensitive
		{"readme.md", "README.MD", "/r/README.MD", true},
		{"*.ts", "X.TS", "/r/X.TS", true},
		// ...but any uppercase makes it case-sensitive
		{"Readme.MD", "README.md", "/r/README.md", false},
		{"README*", "readme.md", "/r/readme.md", false},
		{"README*", "README.md", "/r/README.md", true},
		// slash patterns are matched against the full (absolute) path with **/
		{"src/**/*.spec.ts", "src/a/t.spec.ts", "/r/src/a/t.spec.ts", true},
		{"src/**/*.spec.ts", "other/t.spec.ts", "/r/other/t.spec.ts", false},
		// leading "/" anchors to the absolute path (fd --full-path, no **/)
		{"/r/src/*.go", "src/main.go", "/r/src/main.go", true},
		{"/x/src/*.go", "src/main.go", "/r/src/main.go", false},
		// "**" matches everything
		{"**", "any/depth/file.txt", "/r/any/depth/file.txt", true},
		// "**/" already prefixed: not double-prepended
		{"**/x.ts", "deep/x.ts", "/r/deep/x.ts", true},
	}
	for _, c := range cases {
		if got := matchFdGlob(c.pattern, c.rel, c.abs); got != c.want {
			t.Errorf("matchFdGlob(%q, %q, %q) = %v, want %v", c.pattern, c.rel, c.abs, got, c.want)
		}
	}
}

func TestMatchRgGlobTable(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		// no slash → basename at any depth (rg verified)
		{"*.spec.ts", "f.spec.ts", true},
		{"*.spec.ts", "x/f.spec.ts", true},
		{"*.spec.ts", "x/sub/f.spec.ts", true},
		// slash → anchored to the search root, NO **/ prepend (rg verified:
		// -g 'sub/*.spec.ts' does not match x/sub/f.spec.ts)
		{"x/*.spec.ts", "x/f.spec.ts", true},
		{"x/*.spec.ts", "x/sub/f.spec.ts", false},
		{"sub/*.spec.ts", "x/sub/f.spec.ts", false},
		// rg -g globs are case-sensitive (verified)
		{"X/*.SPEC.ts", "x/f.spec.ts", false},
		// ** crossing
		{"x/**/*.ts", "x/a/b/f.ts", true},
		// leading "/" is equivalent to anchored
		{"/x/*.spec.ts", "x/f.spec.ts", true},
		// braces (globset)
		{"*.{ts,js}", "deep/f.js", true},
	}
	for _, c := range cases {
		if got := matchRgGlob(c.pattern, c.rel); got != c.want {
			t.Errorf("matchRgGlob(%q, %q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// gitignore matcher semantics
// ---------------------------------------------------------------------------

func TestGitignoreMatchTable(t *testing.T) {
	pat := func(line string) ignorePattern {
		pats := parseGitignore([]byte(line))
		if len(pats) != 1 {
			t.Fatalf("parse %q: %d patterns", line, len(pats))
		}
		return pats[0]
	}
	cases := []struct {
		line  string
		rel   string
		isDir bool
		want  bool
	}{
		// dir-only pattern must NOT match a plain file, even nested (the
		// glob.go:184 carve-out bug)
		{"build/", "build", false, false},
		{"build/", "build", true, true},
		{"build/", "x/build", false, false},
		{"build/", "x/build", true, true},
		{"build/", "build/deep/f.txt", false, true}, // under a matching dir
		{"build/", "builders.txt", false, false},
		// "**" crosses slashes
		{"**/gen/*.out", "src/gen/a.out", false, true},
		{"**/gen/*.out", "gen/a.out", false, true},
		{"**/gen/*.out", "src/gen/sub/b.out", false, false},
		// anchoring: a pattern with "/" is anchored to the .gitignore dir
		{"src/*.tmp", "src/x.tmp", false, true},
		{"src/*.tmp", "other/src/x.tmp", false, false},
		{"src/*.tmp", "x.tmp", false, false},
		// leading "/" anchors a basename pattern
		{"/top.txt", "top.txt", false, true},
		{"/top.txt", "sub/top.txt", false, false},
		// unanchored basename matches at any depth
		{"*.log", "deep/nest/a.log", false, true},
		{"node_modules", "x/node_modules/y.js", false, true},
		// "a/**" matches contents but not the directory itself (globset/git)
		{"a/**", "a", true, false},
		{"a/**", "a/b", false, true},
		{"a/**", "a/b/c", false, true},
	}
	for _, c := range cases {
		if got := gitignoreMatch(pat(c.line), c.rel, c.isDir); got != c.want {
			t.Errorf("gitignoreMatch(%q, %q, dir=%v) = %v, want %v", c.line, c.rel, c.isDir, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// find: fd engine behaviors
// ---------------------------------------------------------------------------

func TestFindMatchesDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"src/main.go": "", "other.txt": ""})
	r, err := run(t, findTool(dir), map[string]any{"pattern": "src"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(resultText(r)), "\n")
	if len(lines) != 1 || lines[0] != "src" {
		t.Fatalf("fd matches directories; want [src], got %v", lines)
	}
}

func TestFindNodeModulesOnlyIgnoredViaGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"node_modules/pkg/x.js": "", "y.js": ""})
	// No gitignore → node_modules is NOT hard-ignored.
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.js"})
	text := resultText(r)
	if !strings.Contains(text, "node_modules/pkg/x.js") {
		t.Fatalf("node_modules must not be hard-ignored: %q", text)
	}
	// Gitignored → excluded (find applies gitignore without requiring a repo).
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644)
	r, _ = run(t, findTool(dir), map[string]any{"pattern": "*.js"})
	text = resultText(r)
	if strings.Contains(text, "node_modules") {
		t.Fatalf("gitignored node_modules should be excluded: %q", text)
	}
	if !strings.Contains(text, "y.js") {
		t.Fatalf("y.js should remain: %q", text)
	}
}

func TestFindGitignoreWithoutRepo(t *testing.T) {
	dir := t.TempDir() // no .git — fd --no-require-git still applies .gitignore
	writeFiles(t, dir, map[string]string{"ignored.txt": "", "visible.txt": ""})
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.txt"})
	text := resultText(r)
	if strings.Contains(text, "ignored.txt") || !strings.Contains(text, "visible.txt") {
		t.Fatalf("find must apply .gitignore without a repo: %q", text)
	}
}

func TestFindSkipsDotGit(t *testing.T) {
	dir := t.TempDir()
	mkRepo(t, dir)
	isolateGitGlobals(t)
	writeFiles(t, dir, map[string]string{".git/config": "", "a.txt": ""})
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*"})
	text := resultText(r)
	if strings.Contains(text, ".git") {
		t.Fatalf(".git must always be skipped: %q", text)
	}
}

func TestFindDirOnlyGitignoreCarveOut(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"build/deep/f.txt": "",
		"sub/build":        "", // a plain FILE named build
		"builders.txt":     "",
	})
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*"})
	text := resultText(r)
	if strings.Contains(text, "build/deep") || strings.Contains(text, "f.txt") {
		t.Fatalf("dir-only pattern must ignore the build dir subtree: %q", text)
	}
	if !strings.Contains(text, "sub/build") {
		t.Fatalf("dir-only pattern must NOT match the nested FILE sub/build: %q", text)
	}
	if !strings.Contains(text, "builders.txt") {
		t.Fatalf("builders.txt is not matched by build/: %q", text)
	}
}

func TestFindGitignoreNegation(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.log": "", "keep.log": "", "b.txt": ""})
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n!keep.log\n"), 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*"})
	text := resultText(r)
	if strings.Contains(text, "a.log") {
		t.Fatalf("a.log should be ignored: %q", text)
	}
	if !strings.Contains(text, "keep.log") {
		t.Fatalf("negation should re-include keep.log: %q", text)
	}
}

func TestFindNestedGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"sub/secret.txt":   "",
		"sub/ok.txt":       "",
		"other/secret.txt": "",
	})
	os.WriteFile(filepath.Join(dir, "sub", ".gitignore"), []byte("secret.txt\n"), 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.txt"})
	text := resultText(r)
	if strings.Contains(text, "sub/secret.txt") {
		t.Fatalf("nested .gitignore should apply to its subtree: %q", text)
	}
	if !strings.Contains(text, "other/secret.txt") || !strings.Contains(text, "sub/ok.txt") {
		t.Fatalf("nested .gitignore must not leak outside its dir: %q", text)
	}
}

func TestFindGitInfoExclude(t *testing.T) {
	dir := t.TempDir()
	mkRepo(t, dir)
	isolateGitGlobals(t)
	writeFiles(t, dir, map[string]string{"secret.txt": "", "ok.txt": ""})
	os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("secret.txt\n"), 0o644)
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.txt"})
	text := resultText(r)
	if strings.Contains(text, "secret.txt") {
		t.Fatalf(".git/info/exclude should apply inside a repo: %q", text)
	}
	if !strings.Contains(text, "ok.txt") {
		t.Fatalf("ok.txt should remain: %q", text)
	}
}

func TestFindGlobalCoreExcludesFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	excl := filepath.Join(home, "global-ignore")
	os.WriteFile(excl, []byte("globalignored.txt\n"), 0o644)
	cfg := filepath.Join(home, "gitconfig")
	os.WriteFile(cfg, []byte("[core]\n\texcludesFile = "+excl+"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	dir := t.TempDir()
	mkRepo(t, dir)
	writeFiles(t, dir, map[string]string{"globalignored.txt": "", "ok.txt": ""})
	r, _ := run(t, findTool(dir), map[string]any{"pattern": "*.txt"})
	text := resultText(r)
	if strings.Contains(text, "globalignored.txt") {
		t.Fatalf("global core.excludesFile should apply inside a repo: %q", text)
	}
	if !strings.Contains(text, "ok.txt") {
		t.Fatalf("ok.txt should remain: %q", text)
	}
}

func TestFindAncestorGitignoreInsideRepo(t *testing.T) {
	repo := t.TempDir()
	mkRepo(t, repo)
	isolateGitGlobals(t)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.secret\n"), 0o644)
	writeFiles(t, repo, map[string]string{"sub/a.secret": "", "sub/b.txt": ""})
	// Search rooted BELOW the repo root: the repo-root .gitignore still applies.
	r, _ := run(t, findTool(filepath.Join(repo, "sub")), map[string]any{"pattern": "*"})
	text := resultText(r)
	if strings.Contains(text, "a.secret") {
		t.Fatalf("ancestor .gitignore (repo root) should apply: %q", text)
	}
	if !strings.Contains(text, "b.txt") {
		t.Fatalf("b.txt should remain: %q", text)
	}
}

// ---------------------------------------------------------------------------
// grep: rg engine behaviors
// ---------------------------------------------------------------------------

func TestGrepGitignoreRequiresRepo(t *testing.T) {
	dir := t.TempDir() // NOT a repo
	writeFiles(t, dir, map[string]string{"ignored.txt": "secret match\n", "visible.txt": "match here\n"})
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match"})
	text := resultText(r)
	// rg only respects .gitignore inside a git repository (verified).
	if !strings.Contains(text, "ignored.txt") {
		t.Fatalf("outside a repo, grep must NOT apply .gitignore: %q", text)
	}
	if !strings.Contains(text, "visible.txt") {
		t.Fatalf("visible.txt missing: %q", text)
	}
}

func TestGrepNodeModulesNotHardIgnored(t *testing.T) {
	dir := t.TempDir()
	mkRepo(t, dir)
	isolateGitGlobals(t)
	writeFiles(t, dir, map[string]string{"node_modules/pkg/x.js": "match in nm\n", "y.js": "match here\n"})
	// Not gitignored → searched (rg verified: node_modules matched in a repo
	// when not gitignored).
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match"})
	text := resultText(r)
	if !strings.Contains(text, "node_modules/pkg/x.js") {
		t.Fatalf("node_modules must not be hard-ignored by grep: %q", text)
	}
	// Gitignored → skipped.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644)
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "match"})
	text = resultText(r)
	if strings.Contains(text, "node_modules") {
		t.Fatalf("gitignored node_modules should be skipped: %q", text)
	}
}

func TestGrepSkipsDotGit(t *testing.T) {
	dir := t.TempDir()
	mkRepo(t, dir)
	isolateGitGlobals(t)
	writeFiles(t, dir, map[string]string{".git/config": "match inside git\n", "a.txt": "match here\n"})
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match"})
	text := resultText(r)
	if strings.Contains(text, ".git/config") {
		t.Fatalf(".git must be skipped: %q", text)
	}
	if !strings.Contains(text, "a.txt") {
		t.Fatalf("a.txt missing: %q", text)
	}
}

func TestGrepGlobAnchoring(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"x/f.spec.ts":     "needle\n",
		"sub/x/f.spec.ts": "needle\n",
		"f.spec.ts":       "needle\n",
	})
	// Slash pattern: anchored to the search root, no **/ prepend (rg verified).
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "needle", "glob": "x/*.spec.ts"})
	text := resultText(r)
	if !strings.Contains(text, "x/f.spec.ts:1:") {
		t.Fatalf("anchored glob should match x/f.spec.ts: %q", text)
	}
	if strings.Contains(text, "sub/x/f.spec.ts") {
		t.Fatalf("anchored glob must not match sub/x/f.spec.ts: %q", text)
	}
	// No-slash pattern: basename at any depth.
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "needle", "glob": "*.spec.ts"})
	text = resultText(r)
	for _, want := range []string{"f.spec.ts:1:", "x/f.spec.ts:1:", "sub/x/f.spec.ts:1:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("basename glob should match %s: %q", want, text)
		}
	}
}

func TestGrepSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("match\x00binary"), 0o644)
	os.WriteFile(filepath.Join(dir, "t.txt"), []byte("match here\n"), 0o644)
	r, _ := run(t, grepTool(dir), map[string]any{"pattern": "match"})
	text := resultText(r)
	if strings.Contains(text, "bin.dat") {
		t.Fatalf("NUL-sniffed binary files must be skipped in directory mode: %q", text)
	}
	if !strings.Contains(text, "t.txt") {
		t.Fatalf("t.txt missing: %q", text)
	}
	// Explicitly-named files are still searched (rg verified).
	r, _ = run(t, grepTool(dir), map[string]any{"pattern": "match", "path": "bin.dat"})
	if !strings.Contains(resultText(r), "bin.dat:1:") {
		t.Fatalf("explicit binary file should still be searched: %q", resultText(r))
	}
}

func TestGrepVeryLongLine(t *testing.T) {
	dir := t.TempDir()
	// Longer than the old 8MB bufio.Scanner cap; rg has no line cap.
	long := strings.Repeat("x", 9*1024*1024) + " needle"
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(long+"\n"), 0o644)
	r, err := run(t, grepTool(dir), map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(r)
	if !strings.Contains(text, "big.txt:1:") {
		t.Fatalf("matches on >8MB lines must be found: %q", tail(text))
	}
}
