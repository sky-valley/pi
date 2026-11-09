package providers

import (
	"fmt"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Port of upstream 45344bd3a's strict-tool-limits.test.ts: marked sessions are
// remembered until ai.CleanupSessionResources frees them (one session, or all
// for ""), marking no session does nothing, and past 10,000 sessions the first
// marked is dropped.

func TestStrictToolLimitSessionsRememberUntilCleanup(t *testing.T) {
	t.Cleanup(func() { _ = ai.CleanupSessionResources("") })
	s := strictToolLimitSessions
	s.mark("a")
	s.mark("b")
	s.mark("")
	if !s.exceeded("a") || s.exceeded("c") || s.exceeded("") {
		t.Fatalf(`exceeded a=%v c=%v ""=%v, want true false false`, s.exceeded("a"), s.exceeded("c"), s.exceeded(""))
	}
	if err := ai.CleanupSessionResources("a"); err != nil {
		t.Fatal(err)
	}
	if s.exceeded("a") || !s.exceeded("b") {
		t.Fatalf("after cleanup of a: a=%v b=%v, want false true", s.exceeded("a"), s.exceeded("b"))
	}
	if err := ai.CleanupSessionResources(""); err != nil {
		t.Fatal(err)
	}
	if s.exceeded("b") {
		t.Fatal("b survived the cleanup of every session")
	}
}

func TestStrictToolLimitSessionsDropTheOldestPastTheCap(t *testing.T) {
	s := newStrictToolLimitSessionSet(maxSessionsOverStrictToolLimits)
	for i := 0; i <= maxSessionsOverStrictToolLimits; i++ {
		s.mark(fmt.Sprintf("session-%d", i))
	}
	if s.exceeded("session-0") || !s.exceeded("session-1") || !s.exceeded("session-10000") {
		t.Fatalf("session-0=%v session-1=%v session-10000=%v, want false true true",
			s.exceeded("session-0"), s.exceeded("session-1"), s.exceeded("session-10000"))
	}
}

// Marking a remembered session again keeps its place, as re-adding to pi's Set
// does, so it is still the first dropped.
func TestStrictToolLimitSessionsRemarkKeepsOrder(t *testing.T) {
	s := newStrictToolLimitSessionSet(2)
	s.mark("a")
	s.mark("b")
	s.mark("a")
	s.mark("c")
	if s.exceeded("a") || !s.exceeded("b") || !s.exceeded("c") {
		t.Fatalf("a=%v b=%v c=%v, want false true true", s.exceeded("a"), s.exceeded("b"), s.exceeded("c"))
	}
}
