//go:build darwin || linux

package daemon

import (
	"testing"

	"golang.org/x/sys/unix"
)

func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
