package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Upstream's list.rs tests.

type listTree struct {
	t    *testing.T
	root string
}

func newListTree(t *testing.T, files ...string) *listTree {
	tree := &listTree{t: t, root: t.TempDir()}
	for _, file := range files {
		tree.write(file, "")
	}
	return tree
}

func (tree *listTree) write(path, content string) {
	full := filepath.Join(tree.root, filepath.FromSlash(path))
	if strings.HasSuffix(path, "/") {
		if err := os.MkdirAll(full, 0o755); err != nil {
			tree.t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tree.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		tree.t.Fatal(err)
	}
}

func defaultListOptions() listOptions {
	o, _ := listOptionsFromJSON(object{})
	return o
}

func (tree *listTree) list(options listOptions) ([]string, bool) {
	tree.t.Helper()
	result, fail := listFiles(tree.root, options, func() bool { return false })
	if fail != nil {
		tree.t.Fatal(fail)
	}
	return records(tree.t, result), result.truncated
}

func records(t *testing.T, result listing) []string {
	var entries []string
	for _, record := range strings.Split(string(result.payload), "\x00") {
		if record != "" {
			entries = append(entries, string(record[0])+" "+record[1:])
		}
	}
	if uint64(len(entries)) != result.count {
		t.Fatalf("%d records, count %d", len(entries), result.count)
	}
	return entries
}

func TestListsFilesAndDirectoriesDepthFirstInNameOrder(t *testing.T) {
	tree := newListTree(t, "b.txt", "a/z.txt", "a/b/c.txt", "empty/", ".hidden/x", "A.txt")
	entries, truncated := tree.list(defaultListOptions())
	want := []string{"d .hidden", "f .hidden/x", "f A.txt", "d a", "d a/b", "f a/b/c.txt", "f a/z.txt", "f b.txt", "d empty"}
	if !slices.Equal(entries, want) || truncated {
		t.Fatalf("got %v %v", entries, truncated)
	}
	options := defaultListOptions()
	options.hidden = false
	entries, _ = tree.list(options)
	for _, entry := range entries {
		if strings.Contains(entry, ".hidden") {
			t.Fatalf("hidden entry listed: %v", entries)
		}
	}
}

func TestSkipsWhatGitIgnoresOnlyInWorkTrees(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	tree := newListTree(t, "keep.txt", "out/x.js", "sub/keep.txt", "sub/drop.log", "node_modules/m/i.js")
	tree.write(".gitignore", "out/\n*.log\n")
	tree.write("sub/.gitignore", "!keep.txt\n")
	// Not a work tree yet: everything is listed.
	if entries, _ := tree.list(defaultListOptions()); !slices.Contains(entries, "f out/x.js") {
		t.Fatalf("not a work tree: %v", entries)
	}
	tree.write(".git/HEAD", "ref: refs/heads/main\n")
	tree.write(".git/info/exclude", "keep.txt\n")
	options := defaultListOptions()
	options.exclude = []string{"node_modules"}
	entries, _ := tree.list(options)
	if want := []string{"f .gitignore", "d sub", "f sub/.gitignore", "f sub/keep.txt"}; !slices.Equal(entries, want) {
		t.Fatalf("work tree: got %v, want %v", entries, want)
	}
	options = defaultListOptions()
	options.gitignore = false
	entries, _ = tree.list(options)
	if !slices.Contains(entries, "f out/x.js") {
		t.Fatalf("gitignore off: %v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, ".git/") {
			t.Fatalf(".git listed: %v", entries)
		}
	}
	// A subdirectory of a work tree applies the rules of its parents.
	sub, fail := listFiles(filepath.Join(tree.root, "sub"), defaultListOptions(), func() bool { return false })
	if fail != nil || string(sub.payload) != "f.gitignore\x00fkeep.txt\x00" {
		t.Fatalf("subdirectory: %q %v", sub.payload, fail)
	}
}

func TestListsSymbolicLinksWithoutFollowingThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	tree := newListTree(t, "dir/inner.txt")
	symlink(t, filepath.Join(tree.root, "dir"), filepath.Join(tree.root, "link"))
	symlink(t, "missing", filepath.Join(tree.root, "dangling"))
	symlink(t, tree.root, filepath.Join(tree.root, "dir", "loop"))
	entries, _ := tree.list(defaultListOptions())
	if want := []string{"l dangling", "d dir", "f dir/inner.txt", "l dir/loop", "l link"}; !slices.Equal(entries, want) {
		t.Fatalf("got %v, want %v", entries, want)
	}
}

func TestStopsAtBoundsWithTruncated(t *testing.T) {
	tree := newListTree(t, "a", "b", "c")
	options := defaultListOptions()
	options.maxEntries = 2
	if entries, truncated := tree.list(options); len(entries) != 2 || !truncated {
		t.Fatalf("maxEntries 2: %v %v", entries, truncated)
	}
	options.maxEntries = 3
	if entries, truncated := tree.list(options); len(entries) != 3 || truncated {
		t.Fatalf("maxEntries 3: %v %v", entries, truncated)
	}
	options = defaultListOptions()
	options.maxBytes = 6
	if entries, truncated := tree.list(options); !slices.Equal(entries, []string{"f a", "f b"}) || !truncated {
		t.Fatalf("maxBytes 6: %v %v", entries, truncated)
	}
	options = defaultListOptions()
	options.timeout = time.Duration(0)
	if entries, truncated := tree.list(options); len(entries) != 0 || !truncated {
		t.Fatalf("timeout 0: %v %v", entries, truncated)
	}
}

func TestFailsOnMissingRootsFilesAndAborts(t *testing.T) {
	tree := newListTree(t, "file")
	never := func() bool { return false }
	if _, fail := listFiles(filepath.Join(tree.root, "missing"), defaultListOptions(), never); fail == nil || fail.code != "ENOENT" {
		t.Fatalf("missing: %v", fail)
	}
	if _, fail := listFiles(filepath.Join(tree.root, "file"), defaultListOptions(), never); fail == nil || fail.code != "ENOTDIR" {
		t.Fatalf("file: %v", fail)
	}
	if _, fail := listFiles(tree.root, defaultListOptions(), func() bool { return true }); fail == nil || fail.code != "aborted" {
		t.Fatalf("aborted: %v", fail)
	}
}
