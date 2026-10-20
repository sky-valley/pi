//go:build !unix && !windows

package coding

import "os"

// shellPathExists ports pi's shellPathExists (shell.ts) where there is no
// access(2) to call: a stat that succeeds, which follows symlinks as access
// does.
func shellPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
