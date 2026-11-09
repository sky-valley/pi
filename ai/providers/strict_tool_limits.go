package providers

import (
	"container/list"
	"slices"
	"strings"
	"sync"

	"github.com/sky-valley/pi/ai"
)

// Port of pi's packages/ai/src/api/strict-tool-limits.ts (upstream 45344bd3a):
// how Claude says a request's strict tools exceed its limits, and which
// sessions it already said so for.

// strictToolLimitErrorMessages: Claude rejects (on the Anthropic API and on
// Bedrock) a request whose strict tools exceed its limits with one of these
// messages. Observed limits: 20 strict tools, and 16 optional or union-typed
// parameters across all strict tools. Port of STRICT_TOOL_LIMIT_ERROR_MESSAGES.
var strictToolLimitErrorMessages = []string{
	"The compiled grammar is too large",
	"Too many strict tools (",
	"Schemas contains too many parameters with union types (",
}

// isStrictToolLimitErrorMessage returns true when a provider error message
// says the request's strict tools exceed the provider's limits. Port of
// isStrictToolLimitErrorMessage.
func isStrictToolLimitErrorMessage(message string) bool {
	return slices.ContainsFunc(strictToolLimitErrorMessages, func(limitMessage string) bool {
		return strings.Contains(message, limitMessage)
	})
}

// maxSessionsOverStrictToolLimits bounds strictToolLimitSessions (pi
// MAX_SESSIONS_OVER_STRICT_TOOL_LIMITS). Entries are normally freed by
// ai.CleanupSessionResources when a session ends; the cap bounds memory for
// callers that never call it, and a dropped session just pays one more
// rejection.
const maxSessionsOverStrictToolLimits = 10_000

// strictToolLimitSessionSet is the sessions whose strict tools Claude rejected
// for exceeding its limits, oldest first. Their later requests send
// strict:"prefer" tools non-strict right away: the tool set rarely changes
// between turns, so trying strict again would only repeat the rejection, and
// switching back to strict later would rewrite the tool definitions and miss
// the prompt cache. Like pi's Set, marking a session again keeps its place.
type strictToolLimitSessionSet struct {
	mu    sync.Mutex
	max   int
	order *list.List // of string, first marked first
	ids   map[string]*list.Element
}

func newStrictToolLimitSessionSet(max int) *strictToolLimitSessionSet {
	return &strictToolLimitSessionSet{max: max, order: list.New(), ids: map[string]*list.Element{}}
}

// strictToolLimitSessions is the process-wide set, as pi's module-level Set
// is. Shared by every provider running Claude's limits (pi: Anthropic and
// Bedrock; the port has no Bedrock adapter yet, Scope queue row 9).
var strictToolLimitSessions = newStrictToolLimitSessionSet(maxSessionsOverStrictToolLimits)

func init() {
	ai.RegisterSessionResourceCleanup(func(sessionID string) error {
		strictToolLimitSessions.cleanup(sessionID)
		return nil
	})
}

// mark records that a session's strict tools exceeded Claude's limits; it
// does nothing without a session (pi markStrictToolLimitsExceeded). Past max
// sessions the first marked is dropped.
func (s *strictToolLimitSessionSet) mark(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[sessionID]; ok {
		return
	}
	s.ids[sessionID] = s.order.PushBack(sessionID)
	if s.order.Len() > s.max {
		delete(s.ids, s.order.Remove(s.order.Front()).(string))
	}
}

// exceeded reports whether an earlier request of this session had its strict
// tools rejected for exceeding Claude's limits (pi
// hasExceededStrictToolLimits).
func (s *strictToolLimitSessionSet) exceeded(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[sessionID]
	return ok
}

// cleanup forgets one session, or every session when sessionID is empty.
func (s *strictToolLimitSessionSet) cleanup(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == "" {
		s.order.Init()
		clear(s.ids)
		return
	}
	if e, ok := s.ids[sessionID]; ok {
		s.order.Remove(e)
		delete(s.ids, sessionID)
	}
}
