//go:build darwin || linux

package daemon

import (
	"os"
	"os/signal"
	"syscall"
)

// prepareSignals makes a write to a closed stdout an error rather than a
// fatal SIGPIPE (Rust ignores SIGPIPE), and has Go take over every signal the
// daemon was started ignoring, so that children get the default disposition,
// as libuv and the Rust daemon give them, while the daemon keeps ignoring it.
//
// A child gets SIG_DFL for the signals Go handles and inherits the rest. Go
// handles every signal at start except SIGHUP and SIGINT when they were
// ignored (signal.Ignored reports those) and SIGTSTP, SIGTTIN and SIGTTOU,
// which it leaves as they were (sigaction(2) reports those).
func prepareSignals() {
	sink := make(chan os.Signal, 16)
	signal.Notify(sink, syscall.SIGPIPE)
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT} {
		if signal.Ignored(sig) {
			signal.Notify(sink, sig)
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU} {
		if dispositionIgnored(sig) {
			signal.Notify(sink, sig)
		}
	}
	go func() {
		for range sink {
		}
	}()
}
