package coding

import (
	"path/filepath"
	"runtime"

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
