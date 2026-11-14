package coding

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The suite below ports test/suite/regressions/3302-find-path-glob.test.ts as
// of upstream 463b8f893, which added the direct-child, non-crossing,
// brace/class, result-limit and backslash cases and the "dir/** does not
// match dir" rule. pi gets there on Windows by listing every entry with fd and
// matching path patterns with minimatch (sharkdp/fd#2067); the port's fd
// emulation (matchFdGlob) already matched fd's POSIX semantics on every
// platform, so these pin that it keeps doing so.

func pathGlobTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"some/parent/child/file.ext":     "",
		"some/parent/child/test.spec.ts": "",
		"src/foo/bar/example.spec.ts":    "",
		"src/direct.spec.ts":             "",
	})
	return root
}

func runPathGlobFind(t *testing.T, root, pattern string) []string {
	t.Helper()
	r, err := run(t, findTool(root), map[string]any{"pattern": pattern})
	if err != nil {
		t.Fatalf("find %q: %v", pattern, err)
	}
	text := resultText(r)
	if text == "No files found matching pattern" {
		return nil
	}
	var files []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "[") {
			files = append(files, line)
		}
	}
	slices.Sort(files)
	return files
}

func TestFindPathGlob3302(t *testing.T) {
	root := pathGlobTree(t)
	for _, c := range []struct {
		pattern string
		want    []string
	}{
		{"*.spec.ts", []string{"some/parent/child/test.spec.ts", "src/direct.spec.ts", "src/foo/bar/example.spec.ts"}},
		{"src/**/*.spec.ts", []string{"src/direct.spec.ts", "src/foo/bar/example.spec.ts"}},
		{"src/*.spec.ts", []string{"src/direct.spec.ts"}},
		{"**/parent/child/*", []string{"some/parent/child/file.ext", "some/parent/child/test.spec.ts"}},
	} {
		if got := runPathGlobFind(t, root, c.pattern); !slices.Equal(got, c.want) {
			t.Errorf("find %q = %v, want %v", c.pattern, got, c.want)
		}
	}

	// Like fd's glob, 'dir/**' matches entries below 'dir', not 'dir' itself.
	got := runPathGlobFind(t, root, "some/parent/child/**")
	for _, want := range []string{"some/parent/child/file.ext", "some/parent/child/test.spec.ts"} {
		if !slices.Contains(got, want) {
			t.Errorf("some/parent/child/** = %v, missing %s", got, want)
		}
	}
	if slices.Contains(got, "some/parent/child/") {
		t.Errorf("some/parent/child/** = %v, must not list the directory itself", got)
	}
}

func TestFindPathGlobBracesClassesLiterals(t *testing.T) {
	root := pathGlobTree(t)
	writeFiles(t, root, map[string]string{"src/a&b (1).ts": ""})
	if got, want := runPathGlobFind(t, root, "{src,some}/**/[et]*.spec.ts"), []string{"some/parent/child/test.spec.ts", "src/foo/bar/example.spec.ts"}; !slices.Equal(got, want) {
		t.Errorf("braces/classes = %v, want %v", got, want)
	}
	if got, want := runPathGlobFind(t, root, "src/[!x]&b (1).ts"), []string{"src/a&b (1).ts"}; !slices.Equal(got, want) {
		t.Errorf("literal regex characters = %v, want %v", got, want)
	}
}

func TestFindPathGlobStopsAtLimit(t *testing.T) {
	root := pathGlobTree(t)
	r, err := run(t, findTool(root), map[string]any{"pattern": "src/**/*.spec.ts", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	details, _ := r.Details.(map[string]any)
	if details["resultLimitReached"] != 1 {
		t.Fatalf("details = %v, want resultLimitReached 1", r.Details)
	}
	first := strings.Split(resultText(r), "\n")[0]
	if first != "src/direct.spec.ts" && first != "src/foo/bar/example.spec.ts" {
		t.Fatalf("first line %q, want one of the spec files", first)
	}
}

// On Windows `\` is a path separator (#9262); on POSIX it escapes.
func TestFindPathGlobBackslash(t *testing.T) {
	root := pathGlobTree(t)
	if runtime.GOOS == "windows" {
		for pattern, want := range map[string][]string{
			`src\**\*.spec.ts`:  {"src/direct.spec.ts", "src/foo/bar/example.spec.ts"},
			`src\*.spec.ts`:     {"src/direct.spec.ts"},
			`**\parent\child\*`: {"some/parent/child/file.ext", "some/parent/child/test.spec.ts"},
			`src\**/*.spec.ts`:  {"src/direct.spec.ts", "src/foo/bar/example.spec.ts"},
		} {
			if got := runPathGlobFind(t, root, pattern); !slices.Equal(got, want) {
				t.Errorf("find %q = %v, want %v", pattern, got, want)
			}
		}
		return
	}
	writeFiles(t, root, map[string]string{"src/*.ts": "", "src/other.ts": ""})
	if got, want := runPathGlobFind(t, root, `src/\*.ts`), []string{"src/*.ts"}; !slices.Equal(got, want) {
		t.Errorf(`find src/\*.ts = %v, want %v`, got, want)
	}
}

// Port of "should ignore a leading ./ in the pattern" (tools.test.ts,
// upstream bbfc6c7ce, #12020): fd matches nothing for a leading "./", so the
// tool drops it.
func TestFindIgnoresLeadingDotSlash(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"src/a.ts": "a", "package.json": "{}"})
	for _, pattern := range []string{"./src/*.ts", "./src/**/*.ts", "././src/*.ts"} {
		if got, want := runPathGlobFind(t, root, pattern), []string{"src/a.ts"}; !slices.Equal(got, want) {
			t.Errorf("find %q = %v, want %v", pattern, got, want)
		}
	}
	// Without a "/" left, "./*.json" matches basenames at any depth, like "*.json".
	writeFiles(t, root, map[string]string{"src/b.json": "{}"})
	if got, want := runPathGlobFind(t, root, "./*.json"), []string{"package.json", "src/b.json"}; !slices.Equal(got, want) {
		t.Errorf("find ./*.json = %v, want %v", got, want)
	}
}

// TestFindDoesNotWalkAfterAbort ports find-abort-before-spawn.test.ts
// (upstream a07f25129): once the call is aborted before fd would start, find
// rejects with "Operation aborted" and searches nothing.
func TestFindDoesNotWalkAfterAbort(t *testing.T) {
	root := pathGlobTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := findTool(root).Execute(ctx, "call-1", map[string]any{"pattern": "*.ts"}, nil)
	if err == nil || err.Error() != "Operation aborted" {
		t.Fatalf("find after abort = %q, %v; want pi's \"Operation aborted\"", resultText(res), err)
	}
}

// The suite below ports test/suite/regressions/12066-find-ancestor-glob.test.ts
// (upstream 282b01c80): fd matches --full-path globs against the absolute path
// and find put "**/" in front of path patterns, so a pattern could match
// through folders above the search path — with the project in ".../src/app",
// "src/**/*.ts" returned every .ts file. Path patterns are now anchored at the
// search path, like grep's glob.

func ancestorGlobRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func createAncestorGlobProject(t *testing.T, root string) {
	t.Helper()
	writeFiles(t, root, map[string]string{"src/a.ts": "", "lib/b.ts": "", "test/c.ts": "", "pkg/src/d.ts": ""})
}

func TestFindAncestorGlob12066NoMatchThroughAncestor(t *testing.T) {
	project := filepath.Join(ancestorGlobRoot(t), "src", "app")
	createAncestorGlobProject(t, project)
	if got, want := runPathGlobFind(t, project, "src/**/*.ts"), []string{"src/a.ts"}; !slices.Equal(got, want) {
		t.Errorf("find src/**/*.ts = %v, want %v", got, want)
	}
	if got := runPathGlobFind(t, project, "app/**/*.ts"); len(got) != 0 {
		t.Errorf("find app/**/*.ts = %v, want none", got)
	}
}

func TestFindAncestorGlob12066AnchorsPathPatterns(t *testing.T) {
	project := filepath.Join(ancestorGlobRoot(t), "src", "app")
	createAncestorGlobProject(t, project)
	for _, c := range []struct {
		pattern string
		want    []string
	}{
		{"src/*.ts", []string{"src/a.ts"}},
		{"./src/*.ts", []string{"src/a.ts"}},
		{"pkg/src/*.ts", []string{"pkg/src/d.ts"}},
		{"**/src/*.ts", []string{"pkg/src/d.ts", "src/a.ts"}},
		{"**/src/**/*.ts", []string{"pkg/src/d.ts", "src/a.ts"}},
		// Basename and absolute patterns are unchanged.
		{"*.ts", []string{"lib/b.ts", "pkg/src/d.ts", "src/a.ts", "test/c.ts"}},
		{filepath.ToSlash(project) + "/lib/*.ts", []string{"lib/b.ts"}},
	} {
		if got := runPathGlobFind(t, project, c.pattern); !slices.Equal(got, c.want) {
			t.Errorf("find %q = %v, want %v", c.pattern, got, c.want)
		}
	}
}

func TestFindAncestorGlob12066GlobCharactersInSearchPath(t *testing.T) {
	// `*` and `?` are not allowed in Windows file names.
	name := "w*?[1]{x,y}"
	if runtime.GOOS == "windows" {
		name = "w[1]{x,y}"
	}
	project := filepath.Join(ancestorGlobRoot(t), name, "src")
	createAncestorGlobProject(t, project)
	if got, want := runPathGlobFind(t, project, "src/*.ts"), []string{"src/a.ts"}; !slices.Equal(got, want) {
		t.Errorf("find src/*.ts = %v, want %v", got, want)
	}
}

func TestFindAncestorGlob12066SmartCaseWithUpperCaseSearchPath(t *testing.T) {
	project := filepath.Join(ancestorGlobRoot(t), "Upper")
	writeFiles(t, project, map[string]string{"src/A.ts": "", "src/b.ts": ""})
	if got, want := runPathGlobFind(t, project, "src/a.ts"), []string{"src/A.ts"}; !slices.Equal(got, want) {
		t.Errorf("find src/a.ts = %v, want %v", got, want)
	}
	if got := runPathGlobFind(t, project, "src/B.ts"); len(got) != 0 {
		t.Errorf("find src/B.ts = %v, want none", got)
	}
}

func TestFindAncestorGlob12066SymlinkedSearchPath(t *testing.T) {
	tempRoot := ancestorGlobRoot(t)
	project := filepath.Join(tempRoot, "src", "app")
	createAncestorGlobProject(t, project)
	link := filepath.Join(tempRoot, "src", "link")
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if got, want := runPathGlobFind(t, link, "src/**/*.ts"), []string{"src/a.ts"}; !slices.Equal(got, want) {
		t.Errorf("find src/**/*.ts = %v, want %v", got, want)
	}
}
