package coding

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// Ports pi's test/runtime-credentials.test.ts (d2e7d8b7c), case by case, plus
// the empty-key and listing-order rows probed on npm 1.3.0's
// RuntimeCredentials: an empty override reads the stored credential while
// hasRuntimeApiKey is true, and list() keeps the store's entries first, then
// the other overridden providers in the order their keys were set.

func storeWith(t *testing.T, credentials map[string]ai.Credential) *ai.InMemoryCredentialStore {
	t.Helper()
	store := ai.NewInMemoryCredentialStore()
	for id, credential := range credentials {
		stored := credential
		if _, err := store.Modify(context.Background(), id, func(*ai.Credential) (*ai.Credential, error) { return &stored, nil }); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func readCredential(t *testing.T, store ai.CredentialStore, id string) *ai.Credential {
	t.Helper()
	credential, err := store.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestRuntimeOverridesMaskStoredCredentialsWithoutPersisting(t *testing.T) {
	storage := storeWith(t, map[string]ai.Credential{"anthropic": {Type: ai.CredentialAPIKey, Key: "stored-key"}})
	credentials := NewRuntimeCredentials(storage)

	credentials.SetRuntimeApiKey("anthropic", "runtime-key")
	if got := readCredential(t, credentials, "anthropic"); !reflect.DeepEqual(got, &ai.Credential{Type: ai.CredentialAPIKey, Key: "runtime-key"}) {
		t.Errorf("read with a runtime key: %+v, pi {api_key runtime-key}", got)
	}
	if got := readCredential(t, storage, "anthropic"); got == nil || got.Key != "stored-key" {
		t.Errorf("store: %+v, pi keeps stored-key", got)
	}

	credentials.RemoveRuntimeApiKey("anthropic")
	if got := readCredential(t, credentials, "anthropic"); got == nil || got.Key != "stored-key" {
		t.Errorf("read after removal: %+v, pi stored-key", got)
	}
}

func TestRuntimeCredentialsListMergesOverridesWithoutExposingKeys(t *testing.T) {
	storage := storeWith(t, map[string]ai.Credential{"anthropic": {
		Type: ai.CredentialOAuth, Access: "access", Refresh: "refresh", Expires: time.Now().Add(time.Minute).UnixMilli(),
	}})
	credentials := NewRuntimeCredentials(storage)
	credentials.SetRuntimeApiKey("anthropic", "runtime-key")
	credentials.SetRuntimeApiKey("openai", "other-runtime-key")

	got, err := credentials.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ai.CredentialInfo{{ProviderID: "anthropic", Type: ai.CredentialAPIKey}, {ProviderID: "openai", Type: ai.CredentialAPIKey}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list: %+v, pi %+v", got, want)
	}
}

// contextStore records the context each CredentialStore call receives.
type contextStore struct{ received []context.Context }

func (s *contextStore) Read(ctx context.Context, _ string) (*ai.Credential, error) {
	s.received = append(s.received, ctx)
	return nil, nil
}

func (s *contextStore) List(ctx context.Context) ([]ai.CredentialInfo, error) {
	s.received = append(s.received, ctx)
	return nil, nil
}

func (s *contextStore) Modify(ctx context.Context, _ string, _ func(*ai.Credential) (*ai.Credential, error)) (*ai.Credential, error) {
	s.received = append(s.received, ctx)
	return nil, nil
}

func (s *contextStore) Delete(ctx context.Context, _ string) error {
	s.received = append(s.received, ctx)
	return nil
}

// pi forwards each operation's signal; the port forwards its context.
func TestRuntimeCredentialsForwardOperationContextsToTheStore(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "operation")
	storage := &contextStore{}
	credentials := NewRuntimeCredentials(storage)

	_, _ = credentials.Read(ctx, "anthropic")
	_, _ = credentials.List(ctx)
	_, _ = credentials.Modify(ctx, "anthropic", func(*ai.Credential) (*ai.Credential, error) { return nil, nil })
	_ = credentials.Delete(ctx, "anthropic")

	if len(storage.received) != 4 {
		t.Fatalf("the store saw %d calls, pi 4", len(storage.received))
	}
	for i, got := range storage.received {
		if got != ctx {
			t.Errorf("call %d got another context", i)
		}
	}
}

// failingDeleteStore fails Delete with err.
type failingDeleteStore struct {
	*ai.InMemoryCredentialStore
	err     error
	deletes int
}

func (s *failingDeleteStore) Delete(context.Context, string) error {
	s.deletes++
	return s.err
}

func TestRuntimeCredentialsKeepAnOverrideWhenTheStoreDeleteFails(t *testing.T) {
	cancelled := context.Canceled
	storage := &failingDeleteStore{
		InMemoryCredentialStore: storeWith(t, map[string]ai.Credential{"anthropic": {Type: ai.CredentialAPIKey, Key: "stored-key"}}),
		err:                     cancelled,
	}
	credentials := NewRuntimeCredentials(storage)
	credentials.SetRuntimeApiKey("anthropic", "runtime-key")

	if err := credentials.Delete(context.Background(), "anthropic"); !errors.Is(err, cancelled) {
		t.Fatalf("delete: %v, pi rejects with the store's error", err)
	}
	if storage.deletes != 1 {
		t.Errorf("the store saw %d deletes, pi 1", storage.deletes)
	}
	if got := readCredential(t, credentials, "anthropic"); got == nil || got.Key != "runtime-key" {
		t.Errorf("read after the failed delete: %+v, pi runtime-key", got)
	}
}

func TestRuntimeCredentialsDeleteClearsOverrideAndStore(t *testing.T) {
	storage := storeWith(t, map[string]ai.Credential{"anthropic": {Type: ai.CredentialAPIKey, Key: "stored-key"}})
	credentials := NewRuntimeCredentials(storage)
	credentials.SetRuntimeApiKey("anthropic", "runtime-key")

	if err := credentials.Delete(context.Background(), "anthropic"); err != nil {
		t.Fatal(err)
	}
	if got := readCredential(t, credentials, "anthropic"); got != nil {
		t.Errorf("read after delete: %+v, pi undefined", got)
	}
	if list, err := credentials.List(context.Background()); err != nil || len(list) != 0 {
		t.Errorf("list after delete: %+v %v, pi []", list, err)
	}
}

// Probed on npm 1.3.0.
func TestRuntimeCredentialsEmptyKeyAndListingOrder(t *testing.T) {
	storage := storeWith(t, map[string]ai.Credential{
		"anthropic": {Type: ai.CredentialAPIKey, Key: "stored-key"},
		"openai":    {Type: ai.CredentialAPIKey, Key: "o"},
	})
	credentials := NewRuntimeCredentials(storage)
	credentials.SetRuntimeApiKey("anthropic", "")
	credentials.SetRuntimeApiKey("zeta", "z")
	credentials.SetRuntimeApiKey("alpha", "a")

	if got := readCredential(t, credentials, "anthropic"); got == nil || got.Key != "stored-key" || !credentials.HasRuntimeApiKey("anthropic") {
		t.Errorf("empty override: read %+v has %v, pi reads stored-key and has it", got, credentials.HasRuntimeApiKey("anthropic"))
	}
	got, err := credentials.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ai.CredentialInfo{
		{ProviderID: "anthropic", Type: ai.CredentialAPIKey},
		{ProviderID: "openai", Type: ai.CredentialAPIKey},
		{ProviderID: "zeta", Type: ai.CredentialAPIKey},
		{ProviderID: "alpha", Type: ai.CredentialAPIKey},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list: %+v, pi %+v", got, want)
	}
}
