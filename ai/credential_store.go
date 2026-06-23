package ai

import "sync"

// InMemoryCredentialStore is the default credential store (pi
// packages/ai/src/auth/credential-store.ts). Keyed by provider id, one
// credential per provider; writes are serialized per provider id. Apps inject
// persistent stores. pi serializes via a per-id promise chain; Go uses a
// per-provider mutex, the idiomatic synchronous equivalent.
type InMemoryCredentialStore struct {
	mu      sync.Mutex
	creds   map[string]*Credential
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewInMemoryCredentialStore returns an empty in-memory store.
func NewInMemoryCredentialStore() *InMemoryCredentialStore {
	return &InMemoryCredentialStore{creds: map[string]*Credential{}, locks: map[string]*sync.Mutex{}}
}

// providerLock returns the per-provider serialization lock, creating it once.
func (s *InMemoryCredentialStore) providerLock(id string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	l := s.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

// Read returns a copy of the stored credential, or (nil, nil) when none.
func (s *InMemoryCredentialStore) Read(providerID string) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.creds[providerID]
	if c == nil {
		return nil, nil
	}
	clone := *c
	return &clone, nil
}

// Modify is the only write path: a serialized read-modify-write per provider
// id. fn sees a copy of the current credential (nil when none) and returns the
// new credential, or nil to leave the entry unchanged. Returns the post-write
// credential. An error from fn propagates and leaves the entry unchanged.
func (s *InMemoryCredentialStore) Modify(
	providerID string,
	fn func(current *Credential) (*Credential, error),
) (*Credential, error) {
	lock := s.providerLock(providerID)
	lock.Lock()
	defer lock.Unlock()

	s.mu.Lock()
	current := s.creds[providerID]
	var currentCopy *Credential
	if current != nil {
		c := *current
		currentCopy = &c
	}
	s.mu.Unlock()

	next, err := fn(currentCopy)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if next != nil {
		stored := *next
		s.creds[providerID] = &stored
		ret := stored
		return &ret, nil
	}
	// Unchanged: return the still-current credential (pi's `next ?? current`).
	if cur := s.creds[providerID]; cur != nil {
		ret := *cur
		return &ret, nil
	}
	return nil, nil
}

// Delete removes a credential (logout), serialized against Modify.
func (s *InMemoryCredentialStore) Delete(providerID string) error {
	lock := s.providerLock(providerID)
	lock.Lock()
	defer lock.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, providerID)
	return nil
}
