package coding

import (
	"context"
	"slices"
	"sync"

	"github.com/sky-valley/pi/ai"
)

// RuntimeCredentials is pi's RuntimeCredentials (core/runtime-credentials.ts):
// an ai.CredentialStore that lays non-persistent runtime API keys over the
// store it wraps. A runtime key masks whatever the store holds for its
// provider — an OAuth login, or a stored key together with its env — and is
// never written to the store.
//
// In pi it lives inside ModelRuntime, whose setRuntimeApiKey is how an SDK
// host gives a session a key: createAgentSession takes no key of its own, and
// every request the session makes resolves its auth through the runtime. The
// port has no ModelRuntime (its composition half — auth.json, models.json,
// extension providers — is not ported), so a host builds the session's Models
// over a RuntimeCredentials instead:
//
//	credentials := coding.NewRuntimeCredentials(nil)
//	credentials.SetRuntimeApiKey("anthropic", key)
//	sess, err := coding.NewSession(coding.SessionOptions{
//		Model:  model,
//		Models: ai.BuiltinModels(&ai.CreateModelsOptions{Credentials: credentials}),
//	})
//
// The provider's own auth then turns the key into the request's auth, as
// pi's does: a Cloudflare key, for one, still takes its account id from the
// environment, which fills the endpoint.
type RuntimeCredentials struct {
	store ai.CredentialStore

	mu sync.RWMutex
	// overrides are the runtime keys by provider id; order is their ids in the
	// order they were first set, pi's Map insertion order, which List keeps.
	overrides map[string]string
	order     []string
}

// NewRuntimeCredentials lays runtime API keys over store. A nil store is an
// empty ai.InMemoryCredentialStore; pi's ModelRuntime defaults to its
// auth.json store, which the port does not have.
func NewRuntimeCredentials(store ai.CredentialStore) *RuntimeCredentials {
	if store == nil {
		store = ai.NewInMemoryCredentialStore()
	}
	return &RuntimeCredentials{store: store, overrides: map[string]string{}}
}

// SetRuntimeApiKey sets providerID's runtime API key, replacing any earlier
// one. Read then returns it as an api_key credential instead of what the
// store holds — unless it is empty, which reads through to the store as pi's
// falsy key does, while HasRuntimeApiKey and List still count it.
func (c *RuntimeCredentials) SetRuntimeApiKey(providerID, apiKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.overrides[providerID]; !ok {
		c.order = append(c.order, providerID)
	}
	c.overrides[providerID] = apiKey
}

// RemoveRuntimeApiKey drops providerID's runtime API key, if any; the store's
// credential shows through again.
func (c *RuntimeCredentials) RemoveRuntimeApiKey(providerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.overrides[providerID]; !ok {
		return
	}
	delete(c.overrides, providerID)
	c.order = slices.DeleteFunc(c.order, func(id string) bool { return id == providerID })
}

// HasRuntimeApiKey reports whether providerID has a runtime API key.
func (c *RuntimeCredentials) HasRuntimeApiKey(providerID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.overrides[providerID]
	return ok
}

// Read returns providerID's runtime API key as an api_key credential, or the
// store's credential when it has none.
func (c *RuntimeCredentials) Read(ctx context.Context, providerID string) (*ai.Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	override := c.overrides[providerID]
	c.mu.RUnlock()
	if override != "" {
		return &ai.Credential{Type: ai.CredentialAPIKey, Key: override}, nil
	}
	return c.store.Read(ctx, providerID)
}

// List returns the store's entries with every provider that has a runtime
// key listed as api_key: a stored entry keeps its place, the others follow in
// the order their keys were set. No key is exposed.
func (c *RuntimeCredentials) List(ctx context.Context) ([]ai.CredentialInfo, error) {
	stored, err := c.store.List(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries := slices.Clone(stored)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, providerID := range c.order {
		entry := ai.CredentialInfo{ProviderID: providerID, Type: ai.CredentialAPIKey}
		if i := slices.IndexFunc(entries, func(e ai.CredentialInfo) bool { return e.ProviderID == providerID }); i >= 0 {
			entries[i] = entry
		} else {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// Modify writes through to the store; a runtime key keeps masking the result.
func (c *RuntimeCredentials) Modify(ctx context.Context, providerID string, fn func(current *ai.Credential) (*ai.Credential, error)) (*ai.Credential, error) {
	return c.store.Modify(ctx, providerID, fn)
}

// Delete removes providerID's credential from the store and then its runtime
// key; a failed store delete keeps the key.
func (c *RuntimeCredentials) Delete(ctx context.Context, providerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.store.Delete(ctx, providerID); err != nil {
		return err
	}
	c.RemoveRuntimeApiKey(providerID)
	return nil
}
