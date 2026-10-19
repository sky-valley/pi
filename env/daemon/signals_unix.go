//go:build darwin || linux

package daemon

import (
	"os"
	"os/signal"
	"syscall"
)

// prepareSignals makes a write to a closed stdout an error rather than a
// fatal SIGPIPE (Rust ignores SIGPIPE), and has Go handle every signal the
// daemon was started ignoring, so children get the default disposition, as
// libuv and the Rust daemon give them, while the daemon keeps ignoring them.
func prepareSignals() {
	sink := make(chan os.Signal, 16)
	signal.Notify(sink, syscall.SIGPIPE)
	for sig := 1; sig < 32; sig++ {
		s := syscall.Signal(sig)
		if s == syscall.SIGKILL || s == syscall.SIGSTOP {
			continue
		}
		if signal.Ignored(s) {
			signal.Notify(sink, s)
		}
	}
	go func() {
		for range sink {
		}
	}()
}
