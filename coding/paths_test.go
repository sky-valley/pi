package coding

import (
	"path/filepath"
	"strings"
	"testing"
)

// simulateCaseInsensitivePaths makes paths compare as on Windows for the rest
// of the test, the way pi's paths.test.ts simulates process.platform "win32".
func simulateCaseInsensitivePaths(t *testing.T, on bool) {
	t.Helper()
	saved := pathsCaseInsensitive
	pathsCaseInsensitive = on
	t.Cleanup(func() { pathsCaseInsensitive = saved })
}

// pi paths.test.ts "path comparison" (1be312b46, #10488), on this platform's
// path rules.
func TestPathsEqual(t *testing.T) {
	simulateCaseInsensitivePaths(t, false)
	if pathsEqual("/home/me", "/home/Me") {
		t.Error("case-sensitive platforms keep casing")
	}
	if !pathsEqual("/home/me/", "/home/me") || !pathsEqual("/home/me/./x/..", "/home/me") {
		t.Error("paths compare after resolution")
	}
	if rel, abs := "some/dir", filepath.Join(mustAbs(t, "."), "some", "dir"); !pathsEqual(rel, abs) {
		t.Errorf("a relative path resolves against the process cwd: %q vs %q", rel, abs)
	}

	simulateCaseInsensitivePaths(t, true)
	if !pathsEqual("/home/me", "/HOME/Me/") {
		t.Error("case-insensitive platforms fold casing")
	}
	if pathsEqual("/home/me", "/home/me2") {
		t.Error("different paths stay different")
	}
	// The fold is JavaScript's toLowerCase: U+0130 lowers to "i" + U+0307, so
	// it is not the "i" a simple one-to-one fold would make it.
	if pathsEqual("/home/İ", "/home/i") {
		t.Error("U+0130 must not fold to a bare i")
	}
	if !pathsEqual("/home/İ", "/home/i̇") {
		t.Error("U+0130 folds to i + U+0307")
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// pi session-manager.ts (1be312b46): sessionCwdMatches and the
// default-directory test of list/findById/continueRecent compare with
// pathsEqual, so on Windows a header cwd or an explicit session directory in
// another casing names the same place.
func TestSessionDiscoveryComparesPathsCaseInsensitivelyOnWindows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	project := filepath.Join(root, "Project")
	sessionDir := filepath.Join(root, "sessions")
	recorded := writeSessionFile(t, sessionDir, "s.jsonl", sessionHeaderLine("s-id", filepath.Join(root, "project")), sessionMessageLine)

	simulateCaseInsensitivePaths(t, false)
	if got, ok := FindSessionByID(project, "s-id", sessionDir); ok {
		t.Fatalf("case-sensitive: a cwd in another casing is another cwd, got %q", got)
	}
	if _, filter, _ := sessionListingDir(project, DefaultSessionDir(project)+"/"); filter {
		t.Error("the default directory, spelled with a trailing separator, is still the default")
	}

	simulateCaseInsensitivePaths(t, true)
	if got, ok := FindSessionByID(project, "s-id", sessionDir); !ok || got != recorded {
		t.Fatalf("case-insensitive: FindSessionByID = %q, %v; want %q, true", got, ok, recorded)
	}
	if got := ListSessions(project, sessionDir); len(got) != 1 || got[0].ID != "s-id" {
		t.Fatalf("case-insensitive: ListSessions = %+v; want s-id", got)
	}
	if latest, ok := LatestSession(project, sessionDir); !ok || latest.ID != "s-id" {
		t.Fatalf("case-insensitive: LatestSession = %+v, %v; want s-id", latest, ok)
	}
	defaultDir := DefaultSessionDir(project)
	upper := strings.ToUpper(defaultDir)
	if _, filter, _ := sessionListingDir(project, upper); filter {
		t.Errorf("case-insensitive: the default directory in another casing (%q) is still the default", upper)
	}
	if _, filter, _ := sessionListingDir(project, defaultDir+"x"); !filter {
		t.Error("another directory is filtered")
	}

	simulateCaseInsensitivePaths(t, false)
	if _, filter, _ := sessionListingDir(project, upper); !filter {
		t.Error("case-sensitive: the default directory in another casing is another directory")
	}
}
