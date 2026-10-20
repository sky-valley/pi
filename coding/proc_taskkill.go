package coding

import (
	"os"
	"path/filepath"
)

// taskkillPath resolves Windows' taskkill.exe by absolute path so process-tree
// cleanup does not depend on PATH, porting upstream 7af2d27dc (pi#6596) as
// 61060ddf8 (pi#9490) left it:
//
//	join(process.env.SystemRoot || "C:\\Windows", "System32", "taskkill.exe")
//
// It lives outside proc_windows.go — with no build constraint — purely so the
// path arithmetic is testable on every host; killProcessTree is its only caller.
//
// `||` takes the fallback for an empty SystemRoot as well as an absent one, so
// the path stays absolute; Getenv's "" covers both.
func taskkillPath() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return filepath.Join(systemRoot, "System32", "taskkill.exe")
}
