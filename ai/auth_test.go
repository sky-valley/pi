package ai

import (
	"errors"
	"testing"
)

// fakeAuthContext is an injectable AuthContext over a fixed env map.
type fakeAuthContext struct {
	env   map[string]string
	files map[string]bool
}

func (f fakeAuthContext) Env(name string) string   { return f.env[name] }
func (f fakeAuthContext) FileExists(p string) bool { return f.files[p] }

func TestInMemoryCredentialStore(t *testing.T) {
	s := NewInMemoryCredentialStore()

	if c, err := s.Read("p"); err != nil || c != nil {
		t.Fatalf("empty read = (%v, %v), want (nil, nil)", c, err)
	}

	// Modify sets a credential and returns the post-write value.
	got, err := s.Modify("p", func(cur *Credential) (*Credential, error) {
		if cur != nil {
			t.Errorf("fn should see nil current, got %v", cur)
		}
		return &Credential{Type: CredentialAPIKey, Key: "k1"}, nil
	})
	if err != nil || got == nil || got.Key != "k1" {
		t.Fatalf("modify = (%v, %v), want key k1", got, err)
	}

	// Read returns a copy (mutating it must not affect the store).
	c, _ := s.Read("p")
	c.Key = "mutated"
	if again, _ := s.Read("p"); again.Key != "k1" {
		t.Errorf("store credential was aliased: got %q", again.Key)
	}

	// fn returning nil leaves the entry unchanged and returns current.
	got, _ = s.Modify("p", func(cur *Credential) (*Credential, error) { return nil, nil })
	if got == nil || got.Key != "k1" {
		t.Errorf("no-op modify should return current k1, got %v", got)
	}

	// fn error leaves the entry unchanged.
	if _, err := s.Modify("p", func(cur *Credential) (*Credential, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Error("modify should propagate fn error")
	}
	if after, _ := s.Read("p"); after == nil || after.Key != "k1" {
		t.Errorf("entry should survive a failed modify, got %v", after)
	}

	// Delete removes it.
	_ = s.Delete("p")
	if after, _ := s.Read("p"); after != nil {
		t.Errorf("delete should remove the entry, got %v", after)
	}
}

func TestEnvAPIKeyAuthResolve(t *testing.T) {
	auth := EnvAPIKeyAuth("Test key", "PRIMARY_KEY", "SECONDARY_KEY")
	model := &Model{Provider: "test"}

	// Stored credential key wins over env.
	ctx := fakeAuthContext{env: map[string]string{"PRIMARY_KEY": "from-env"}}
	res, err := auth.Resolve(model, ctx, &Credential{Type: CredentialAPIKey, Key: "stored"})
	if err != nil || res == nil || res.Auth.APIKey != "stored" || res.Source != "stored credential" {
		t.Fatalf("stored key should win: %+v (err %v)", res, err)
	}

	// No stored credential: first set env var in order resolves.
	res, _ = auth.Resolve(model, fakeAuthContext{env: map[string]string{"SECONDARY_KEY": "second"}}, nil)
	if res == nil || res.Auth.APIKey != "second" || res.Source != "SECONDARY_KEY" {
		t.Fatalf("env fallback wrong: %+v", res)
	}

	// Unconfigured -> nil.
	if res, _ := auth.Resolve(model, fakeAuthContext{}, nil); res != nil {
		t.Fatalf("unconfigured should be nil, got %+v", res)
	}
}

func TestResolveProviderAuthAPIKeyAmbient(t *testing.T) {
	auth := ProviderAuth{APIKey: EnvAPIKeyAuth("Test", "TEST_KEY")}
	model := &Model{Provider: "test"}
	store := NewInMemoryCredentialStore()
	ctx := fakeAuthContext{env: map[string]string{"TEST_KEY": "ambient"}}

	res, err := resolveProviderAuth("test", auth, model, store, ctx)
	if err != nil || res == nil || res.Auth.APIKey != "ambient" {
		t.Fatalf("ambient api-key resolution wrong: %+v (err %v)", res, err)
	}
}

func TestResolveProviderAuthOAuthRefreshUnderLock(t *testing.T) {
	model := &Model{Provider: "oauthp"}
	store := NewInMemoryCredentialStore()
	// Seed an EXPIRED oauth credential.
	_, _ = store.Modify("oauthp", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1}, nil
	})

	refreshCalls := 0
	auth := ProviderAuth{OAuth: &OAuthAuth{
		Name: "Test OAuth",
		Refresh: func(c OAuthCredentials) (OAuthCredentials, error) {
			refreshCalls++
			return OAuthCredentials{Refresh: "r1", Access: "new", Expires: nowMillis() + 3_600_000}, nil
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) {
			return ModelAuth{APIKey: c.Access}, nil
		},
	}}

	res, err := resolveProviderAuth("oauthp", auth, model, store, ctx())
	if err != nil || res == nil || res.Auth.APIKey != "new" || res.Source != "OAuth" {
		t.Fatalf("expired oauth should refresh then derive: %+v (err %v)", res, err)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected exactly one refresh, got %d", refreshCalls)
	}
	// The rotated credential must be persisted.
	if stored, _ := store.Read("oauthp"); stored == nil || stored.Access != "new" {
		t.Fatalf("rotated credential not persisted: %+v", stored)
	}

	// A still-valid credential is not refreshed.
	res, _ = resolveProviderAuth("oauthp", auth, model, store, ctx())
	if res == nil || res.Auth.APIKey != "new" || refreshCalls != 1 {
		t.Fatalf("valid token should not refresh again: %+v calls=%d", res, refreshCalls)
	}
}

func TestResolveProviderAuthOAuthRefreshFailure(t *testing.T) {
	model := &Model{Provider: "oauthp"}
	store := NewInMemoryCredentialStore()
	_, _ = store.Modify("oauthp", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, Refresh: "r0", Access: "old", Expires: 1}, nil
	})

	auth := ProviderAuth{OAuth: &OAuthAuth{
		Refresh: func(c OAuthCredentials) (OAuthCredentials, error) {
			return OAuthCredentials{}, errors.New("invalid_grant")
		},
		ToAuth: func(c OAuthCredentials) (ModelAuth, error) { return ModelAuth{}, nil },
	}}

	_, err := resolveProviderAuth("oauthp", auth, model, store, ctx())
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrOAuth {
		t.Fatalf("refresh failure should be ModelsError code oauth, got %v", err)
	}
	// The stored credential is preserved for retry.
	if stored, _ := store.Read("oauthp"); stored == nil || stored.Access != "old" {
		t.Fatalf("failed refresh must preserve the stored credential, got %+v", stored)
	}
}

func ctx() AuthContext { return fakeAuthContext{} }
