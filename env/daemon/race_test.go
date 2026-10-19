//go:build race

package daemon

// raceBuild makes the tests build the daemon they run with the race detector
// when they run with it themselves.
func init() { buildFlags = append(buildFlags, "-race") }
