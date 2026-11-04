package ai

import (
	"errors"
	"fmt"
	"sync"
)

// SessionResourceCleanup frees what a provider keeps for one session, or for
// every session when sessionID is empty (pi SessionResourceCleanup, whose
// `sessionId?` is undefined for "all").
type SessionResourceCleanup func(sessionID string) error

var sessionResourceCleanups struct {
	mu sync.Mutex
	// entries keeps registration order, as pi's Set iterates it; each entry is
	// a distinct pointer so unregistering removes exactly one registration.
	entries []*SessionResourceCleanup
}

// RegisterSessionResourceCleanup registers a cleanup that CleanupSessionResources
// runs, and returns a function that unregisters it (pi
// registerSessionResourceCleanup). Providers that keep per-session state, such
// as the Anthropic message threads, register one at init.
func RegisterSessionResourceCleanup(cleanup SessionResourceCleanup) (unregister func()) {
	entry := &cleanup
	r := &sessionResourceCleanups
	r.mu.Lock()
	r.entries = append(r.entries, entry)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, e := range r.entries {
			if e == entry {
				r.entries = append(r.entries[:i:i], r.entries[i+1:]...)
				return
			}
		}
	}
}

// CleanupSessionResources frees the state every provider keeps for sessionID,
// or for every session when sessionID is empty (pi cleanupSessionResources).
// Call it when a session ends. Every registered cleanup runs even when an
// earlier one fails; their errors are returned together.
func CleanupSessionResources(sessionID string) error {
	r := &sessionResourceCleanups
	r.mu.Lock()
	entries := r.entries[:len(r.entries):len(r.entries)]
	r.mu.Unlock()
	var errs []error
	for _, cleanup := range entries {
		if err := (*cleanup)(sessionID); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("Failed to cleanup session resources: %w", errors.Join(errs...))
	}
	return nil
}
