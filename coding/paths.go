package coding

import (
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// pathsCaseInsensitive is whether paths compare case-insensitively: on
// Windows, where `c:\Users` and `C:\Users` are the same directory. NTFS can
// enable per-directory case sensitivity, but pi ignores it as rarely used
// outside WSL interop. A variable so tests can simulate Windows, as pi's read
// process.platform at call time.
var pathsCaseInsensitive = runtime.GOOS == "windows"

// pathComparisonKey is pi's getPathComparisonKey (utils/paths.ts, 1be312b46):
// the resolved absolute path, folded by JavaScript's toLowerCase on Windows.
// It does not touch the filesystem. Use it only to compare, never to display
// or open.
func pathComparisonKey(path string) string {
	resolved, err := filepath.Abs(path)
	if err != nil {
		// Abs fails only when the process cwd is gone, where node's resolve
		// throws. A comparison key degrades to a lexical compare instead.
		resolved = filepath.Clean(path)
	}
	if pathsCaseInsensitive {
		return jstext.ToLower(resolved)
	}
	return resolved
}

// pathsEqual reports whether two paths name the same location by their
// pathComparisonKey (pi pathsEqual).
func pathsEqual(left, right string) bool {
	return pathComparisonKey(left) == pathComparisonKey(right)
}

// isPathWithin reports whether child is parent or inside it, by their
// pathComparisonKey (pi isPathWithin). Whole components only: /home/me2 is
// not within /home/me. A filesystem root (`/`, `C:\`) already ends with a
// separator, so everything under it is within it.
func isPathWithin(parent, child string) bool {
	parentKey, childKey := pathComparisonKey(parent), pathComparisonKey(child)
	if childKey == parentKey {
		return true
	}
	if !strings.HasSuffix(parentKey, string(filepath.Separator)) {
		parentKey += string(filepath.Separator)
	}
	return strings.HasPrefix(childKey, parentKey)
}
