package daemon

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dispositionIgnored reports whether sig is ignored, read with sigaction(2);
// the handler leads struct sigaction.
func dispositionIgnored(sig syscall.Signal) bool {
	var old [4]uint64
	_, _, errno := unix.Syscall(unix.SYS_SIGACTION, uintptr(sig), 0, uintptr(unsafe.Pointer(&old[0])))
	return errno == 0 && old[0] == 1 // SIG_IGN
}
