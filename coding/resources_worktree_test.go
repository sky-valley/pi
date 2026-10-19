package coding

import (
	"os"
	"path/filepath"
	"testing"
)

// Upstream cced6a21: a nested linked worktree's own AGENTS.md/CLAUDE.md and the
// main repo's copy are the same tracked file, so loading both loaded it twice.
// These are pi's nine resource-loader scenarios, with pi's hand-built worktree
// skeletons (no git binary needed).

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// linkWorktree builds a linked-worktree skeleton: the main repo's
// .git/worktrees/<name>/ holds HEAD plus a commondir pointing back at the main
// .git, and the worktree's working tree carries a .git *file* whose gitdir:
// resolves to it.
func linkWorktree(t *testing.T, mainDir, worktreeDir, name string) {
	t.Helper()
	gitDir := filepath.Join(mainDir, ".git", "worktrees", name)
	// The main repo's own .git is a real git dir with a HEAD, as git writes it.
	writeFile(t, filepath.Join(mainDir, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/feat\n")
	// commondir is relative to the worktree gitdir and points at the main .git.
	writeFile(t, filepath.Join(gitDir, "commondir"), "../..")
	writeFile(t, filepath.Join(worktreeDir, ".git"), "gitdir: "+gitDir+"\n")
}

// isolatedHome points AgentDir() at an empty dir so the developer's own global
// context file cannot join the results. Returns the home dir for tests that
// populate it.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func contextContents(files []ContextFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Content)
	}
	return out
}

func assertContents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// nestedWorktree lays out a main repo at <tmp>/outer/main with a linked
// worktree at main/worktrees/feat. Each case writes only the files it needs.
func nestedWorktree(t *testing.T) (outer, main, worktree, worktreeSrc string) {
	t.Helper()
	tempDir := t.TempDir()
	outer = filepath.Join(tempDir, "outer")
	main = filepath.Join(outer, "main")
	worktree = filepath.Join(main, "worktrees", "feat")
	worktreeSrc = filepath.Join(worktree, "src")
	if err := os.MkdirAll(worktreeSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkWorktree(t, main, worktree, "feat")
	return outer, main, worktree, worktreeSrc
}

func TestWorktreeSkipsMainRepoDuplicate(t *testing.T) {
	isolatedHome(t)
	_, main, worktree, worktreeSrc := nestedWorktree(t)
	writeFile(t, filepath.Join(main, "AGENTS.md"), "main repo instructions")
	writeFile(t, filepath.Join(worktree, "AGENTS.md"), "worktree instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(worktreeSrc)), "worktree instructions")
}

func TestWorktreeInheritsMainRepoContextWhenItHasNone(t *testing.T) {
	isolatedHome(t)
	_, main, _, worktreeSrc := nestedWorktree(t)
	writeFile(t, filepath.Join(main, "AGENTS.md"), "main repo instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(worktreeSrc)), "main repo instructions")
}

// The repo tracks CLAUDE.md; the worktree adds an AGENTS.md, which
// loadContextFileFromDir prefers. The main repo's CLAUDE.md is nobody's
// duplicate, so dropping it would lose its content entirely.
func TestWorktreeSkipsOnlyTheSameFilename(t *testing.T) {
	isolatedHome(t)
	_, main, worktree, worktreeSrc := nestedWorktree(t)
	writeFile(t, filepath.Join(main, "CLAUDE.md"), "main repo instructions")
	writeFile(t, filepath.Join(worktree, "AGENTS.md"), "worktree instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(worktreeSrc)),
		"main repo instructions", "worktree instructions")
}

// `git clone --bare proj/.bare` + `git worktree add ../main` makes commondir
// `../..`, so the parent of the common git dir is `proj` — a plain directory
// that tracks nothing. Its AGENTS.md is not a duplicate of the worktree's.
func TestBareLayoutKeepsContainerContext(t *testing.T) {
	isolatedHome(t)
	tempDir := t.TempDir()
	proj := filepath.Join(tempDir, "proj")
	bare := filepath.Join(proj, ".bare")
	worktree := filepath.Join(proj, "main")
	worktreeGitDir := filepath.Join(bare, "worktrees", "main")
	writeFile(t, filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(worktreeGitDir, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(worktreeGitDir, "commondir"), "../..")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+worktreeGitDir+"\n")
	writeFile(t, filepath.Join(proj, "AGENTS.md"), "container instructions")
	writeFile(t, filepath.Join(worktree, "AGENTS.md"), "worktree instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(worktree)),
		"container instructions", "worktree instructions")
}

func TestWorktreeKeepsAncestorsAboveMainRepo(t *testing.T) {
	isolatedHome(t)
	outer, main, worktree, worktreeSrc := nestedWorktree(t)
	writeFile(t, filepath.Join(outer, "AGENTS.md"), "outer instructions")
	writeFile(t, filepath.Join(main, "AGENTS.md"), "main repo instructions")
	writeFile(t, filepath.Join(worktree, "AGENTS.md"), "worktree instructions")

	// Only the main repo root's duplicate is dropped; the unrelated dir above it stays.
	assertContents(t, contextContents(LoadProjectContextFiles(worktreeSrc)),
		"outer instructions", "worktree instructions")
}

// `git worktree add ../feat` puts the worktree beside the main repo, so no
// duplicate is ever encountered and ancestors above it are unrelated.
func TestSiblingWorktreeSkipsNothing(t *testing.T) {
	isolatedHome(t)
	tempDir := t.TempDir()
	outer := filepath.Join(tempDir, "outer")
	main := filepath.Join(outer, "main")
	sib := filepath.Join(outer, "sib-feat")
	sibSrc := filepath.Join(sib, "src")
	if err := os.MkdirAll(sibSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(outer, "AGENTS.md"), "outer instructions")
	writeFile(t, filepath.Join(sib, "AGENTS.md"), "sibling worktree instructions")
	linkWorktree(t, main, sib, "sib")

	assertContents(t, contextContents(LoadProjectContextFiles(sibSrc)),
		"outer instructions", "sibling worktree instructions")
}

// A submodule's .git file is also gitdir:-style, but its gitdir has no
// commondir, so it resolves under .git/modules — never an ancestor of cwd.
func TestSubmoduleKeepsSuperprojectContext(t *testing.T) {
	isolatedHome(t)
	tempDir := t.TempDir()
	sup := filepath.Join(tempDir, "super")
	sub := filepath.Join(sup, "vendor", "lib")
	subSrc := filepath.Join(sub, "src")
	if err := os.MkdirAll(subSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(sup, "AGENTS.md"), "superproject instructions")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "submodule instructions")
	subGitDir := filepath.Join(sup, ".git", "modules", "vendor", "lib")
	writeFile(t, filepath.Join(subGitDir, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: "+subGitDir+"\n")

	assertContents(t, contextContents(LoadProjectContextFiles(subSrc)),
		"superproject instructions", "submodule instructions")
}

func TestOrdinaryRepoKeepsClimbing(t *testing.T) {
	isolatedHome(t)
	tempDir := t.TempDir()
	outer := filepath.Join(tempDir, "outer")
	repo := filepath.Join(outer, "repo")
	leaf := filepath.Join(repo, "src")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(outer, "AGENTS.md"), "outer instructions")
	writeFile(t, filepath.Join(repo, "AGENTS.md"), "repo instructions")
	writeFile(t, filepath.Join(leaf, "AGENTS.md"), "leaf instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(leaf)),
		"outer instructions", "repo instructions", "leaf instructions")
}

func TestNonexistentGitdirTargetClimbsNormally(t *testing.T) {
	isolatedHome(t)
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "corrupt")
	src := filepath.Join(repo, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: /nonexistent/path/worktrees/feat\n")
	writeFile(t, filepath.Join(repo, "AGENTS.md"), "repo instructions")
	writeFile(t, filepath.Join(src, "AGENTS.md"), "src instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(src)),
		"repo instructions", "src instructions")
}

// `git worktree add` writes the .git file's gitdir: target in realpath form
// while cwd may still be reached through a symlink, so the shadow check must
// compare canonicalized paths. The other cases cannot see this: they build the
// gitdir: target from the same uncanonicalized temp path, so a port with no
// canonicalization at all still passes them.
func TestWorktreeShadowSurvivesSymlinkedCwd(t *testing.T) {
	isolatedHome(t)
	tempDir := t.TempDir()
	real := filepath.Join(tempDir, "real")
	main := filepath.Join(real, "main")
	worktree := filepath.Join(main, "worktrees", "feat")
	worktreeSrc := filepath.Join(worktree, "src")
	if err := os.MkdirAll(worktreeSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// The skeleton records realpaths, as git does.
	realMain := canonicalizePath(main)
	gitDir := filepath.Join(realMain, ".git", "worktrees", "feat")
	writeFile(t, filepath.Join(realMain, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/feat\n")
	writeFile(t, filepath.Join(gitDir, "commondir"), "../..")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+gitDir+"\n")
	writeFile(t, filepath.Join(main, "AGENTS.md"), "main repo instructions")
	writeFile(t, filepath.Join(worktree, "AGENTS.md"), "worktree instructions")

	// cwd is reached through a symlink that git never recorded.
	link := filepath.Join(tempDir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkedSrc := filepath.Join(link, "main", "worktrees", "feat", "src")

	assertContents(t, contextContents(LoadProjectContextFiles(linkedSrc)), "worktree instructions")
}

// An unreadable candidate is skipped in favour of the next one, so the shadow
// must follow the file that would actually be loaded. A name-only existence
// probe would shadow main/AGENTS.md here and drop its content entirely.
func TestWorktreeShadowFollowsTheLoadableCandidate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	isolatedHome(t)
	_, main, worktree, worktreeSrc := nestedWorktree(t)
	writeFile(t, filepath.Join(main, "AGENTS.md"), "main agents")
	writeFile(t, filepath.Join(main, "CLAUDE.md"), "main claude")
	writeFile(t, filepath.Join(worktree, "AGENTS.md"), "worktree agents")
	writeFile(t, filepath.Join(worktree, "CLAUDE.md"), "worktree claude")
	if err := os.Chmod(filepath.Join(worktree, "AGENTS.md"), 0o000); err != nil {
		t.Skipf("chmod unavailable: %v", err)
	}

	// The worktree yields CLAUDE.md, so main's CLAUDE.md is the duplicate and
	// main's AGENTS.md must survive.
	assertContents(t, contextContents(LoadProjectContextFiles(worktreeSrc)),
		"main agents", "worktree claude")
}

func contextPaths(files []ContextFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// resource-loader.test.ts at f1b2e77f5 (#10681): the shadow check compares the
// walked directory's real path plus the file name, not the file's real path,
// so a worktree AGENTS.md symlinked to the main repo's copy is the worktree's
// own and is loaded once.
func TestWorktreeLoadsContextSymlinkedToTheMainRepo(t *testing.T) {
	t.Run("the worktree's file links to the main repo's", func(t *testing.T) {
		isolatedHome(t)
		_, main, worktree, worktreeSrc := nestedWorktree(t)
		writeFile(t, filepath.Join(main, "AGENTS.md"), "main repo instructions")
		if err := os.Symlink(filepath.Join(main, "AGENTS.md"), filepath.Join(worktree, "AGENTS.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		assertContents(t, contextPaths(LoadProjectContextFiles(worktreeSrc)), filepath.Join(worktree, "AGENTS.md"))
	})
	t.Run("both link to one shared file", func(t *testing.T) {
		isolatedHome(t)
		outer, main, worktree, worktreeSrc := nestedWorktree(t)
		shared := filepath.Join(outer, "shared-agents.md")
		writeFile(t, shared, "shared instructions")
		if err := os.Symlink(shared, filepath.Join(main, "AGENTS.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink(shared, filepath.Join(worktree, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		assertContents(t, contextPaths(LoadProjectContextFiles(worktreeSrc)), filepath.Join(worktree, "AGENTS.md"))
	})
}
