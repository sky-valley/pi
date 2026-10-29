package daemon

// prepareSignals: Windows children do not inherit signal dispositions.
func prepareSignals() {}

// exitOnSignals: nothing to do. The daemon cannot catch TerminateProcess;
// when it ends, the job object kills each command's own process, while the
// processes those start leave the job (silent breakaway, as in libuv) and
// keep running.
func exitOnSignals(*groups) {}
