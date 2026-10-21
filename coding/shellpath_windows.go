package coding

import "syscall"

// shellPathExists ports pi's shellPathExists (shell.ts, upstream 28bea006b):
// accessSync(path, F_OK) succeeds. Node namespaces the path
// (windowsAccessPath) and libuv implements the check as GetFileAttributesW
// succeeding (src/win/fs.c fs__access), which reads the entry itself without
// following a reparse point. That is why pi uses it over existsSync, whose
// extra stat rejects runnable Store app-execution aliases
// (nodejs/node#36790), and why a dangling symlink counts as existing here.
func shellPathExists(path string) bool {
	p, err := syscall.UTF16PtrFromString(windowsAccessPath(path))
	if err != nil {
		return false
	}
	_, err = syscall.GetFileAttributes(p)
	return err == nil
}
