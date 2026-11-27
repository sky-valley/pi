package daemon

import "testing"

// Ported from exec.rs's test (upstream 3d13a5243).
func TestARegistrationNeverTouchesALaterOneWithTheSamePid(t *testing.T) {
	// A pid no process has (above Linux's largest), so a wrong kill signals
	// nothing.
	const pid = 99_999_999
	running := newGroups()
	first := running.register(pid)
	// The first command was reaped and its pid reused: a later command
	// registers it.
	second := running.register(pid)
	first.forget()
	first.kill()
	if token, ok := running.pids[pid]; !ok || token != second.token {
		t.Fatalf("the later registration: %d, %v; want %d", token, ok, second.token)
	}
	second.forget()
	if len(running.pids) != 0 {
		t.Fatalf("left registered: %v", running.pids)
	}
}
