//go:build darwin

package daemon

import (
	"path/filepath"
	"testing"
)

// realpathWalk is Android's realpath(3) (glibc's walk; bionic is taken to
// agree, unverified). On macOS it runs here against the system's own
// realpath(3) port (pinned to the Rust daemon by TestOracle): for names
// spelled as stored, the two must agree on every result and error. This
// checks the walk's mechanics only; Linux's realpath is musl's, pinned by
// TestOracle/musl and TestRealpathAgainstRust.
func TestRealpathWalkAgreesWithTheSystemRealpath(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]string{"dir/sub/x": "", "file": ""})
	symlink(t, "dir", filepath.Join(root, "rel"))
	symlink(t, filepath.Join(root, "dir", "sub"), filepath.Join(root, "abs"))
	symlink(t, "loop2", filepath.Join(root, "loop1"))
	symlink(t, "loop1", filepath.Join(root, "loop2"))
	symlink(t, "nowhere", filepath.Join(root, "dangling"))
	symlink(t, "../..", filepath.Join(root, "dir", "up"))
	symlink(t, "sub/", filepath.Join(root, "dir", "slash"))
	paths := []string{
		"", "/", "/tmp", "/tmp/", "/tmp/.", "/tmp/..", "//tmp", root, root + "/", root + "/.",
		root + "/rel/sub", root + "/rel/sub/x", root + "/abs", root + "/abs/../sub/.", root + "/abs/..",
		root + "/loop1", root + "/loop1/x", root + "/dangling", root + "/dangling/x", root + "/missing",
		root + "/missing/x", root + "/missing/..", root + "/file", root + "/file/", root + "/file/.",
		root + "/file/..", root + "/file/x", root + "/dir/up", root + "/dir/up/dir", root + "/dir/slash/x",
		root + "//dir///sub//", root + "/dir/./sub/../sub", root + "/../" + filepath.Base(root) + "/dir",
	}
	// glibc refuses a file named with a trailing "/", "/." or "/.." (ENOTDIR),
	// where Apple's realpath(3) resolves it.
	glibcOnly := map[string]bool{root + "/file/": true, root + "/file/.": true, root + "/file/..": true}
	for _, path := range paths {
		got, gotErr := realpathWalk(path)
		if glibcOnly[path] {
			if gotErr == nil || errorName(gotErr) != "ENOTDIR" {
				t.Errorf("%q: walk gives %q %v, want ENOTDIR", path, got, gotErr)
			}
			continue
		}
		want, wantErr := realpath(path)
		if got != want || (wantErr == nil) != (gotErr == nil) || (wantErr != nil && errorName(wantErr) != errorName(gotErr)) {
			t.Errorf("%q: walk gives %q %v, realpath(3) %q %v", path, got, gotErr, want, wantErr)
		}
	}
}
