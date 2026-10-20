//go:build unix

package coding

import "syscall"

// accessFOK is access(2)'s F_OK: test for existence only.
const accessFOK = 0

// shellPathExists ports pi's shellPathExists (shell.ts, upstream 28bea006b):
// accessSync(path, F_OK) succeeds. On Unix that is access(2) itself, which
// follows symlinks, so a dangling one does not exist. It is deliberately not
// pathExistsFS: pi checks shell paths with access and read paths with stat,
// and on Windows the two disagree.
func shellPathExists(path string) bool {
	return syscall.Access(path, accessFOK) == nil
}
