package daemon

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dispositionIgnored reports whether sig is ignored, read with
// rt_sigaction(2); the handler leads struct kernel_sigaction.
func dispositionIgnored(sig syscall.Signal) bool {
	var old [8]uint64
	_, _, errno := unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(sig), 0, uintptr(unsafe.Pointer(&old[0])), 8, 0, 0)
	return errno == 0 && old[0] == 1 // SIG_IGN
}
