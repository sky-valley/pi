package ai

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// promptRecorder answers each login prompt "x1", "x2", ... as the capture does.
type promptRecorder struct{ prompts []AuthPrompt }

func (r *promptRecorder) Prompt(prompt AuthPrompt) (string, error) {
	r.prompts = append(r.prompts, AuthPrompt{Type: prompt.Type, Message: prompt.Message})
	return "x" + string(rune('0'+len(r.prompts))), nil
}

func (r *promptRecorder) Notify(AuthEvent) {}

// builtinAuthDebt names the built-in providers whose api-key auth still
// differs from pi's (docs: F1 ledger notes). Each must still differ, so a fix
// removes it from here.
var builtinAuthDebt = map[string]string{
	// pi's AWS login selects a method first; the port's ambient resolver has
	// no login.
	"amazon-bedrock": "login",
	// pi's Vertex login selects a method first; no login here either.
	"google-vertex": "login",
	// pi gives openai-codex OAuth auth alone; the port, without its OAuth
	// login, keeps an ambient api-key resolver so the provider has auth.
	"openai-codex": "auth",
	// Classifier-only: BuiltinModels passes over it (Scope queue row 3).
	"typesafe": "provider",
}

// TestBuiltinAPIKeyAuthNamesMatchPi: each built-in provider's api-key auth
// carries pi's display name, and its login asks pi's prompts
// (testdata/builtin-auth, capture.mjs against npm 1.3.0).
func TestBuiltinAPIKeyAuthNamesMatchPi(t *testing.T) {
	data, err := os.ReadFile("testdata/builtin-auth/builtin-auth-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Providers []struct {
			ID         string       `json:"id"`
			APIKeyName *string      `json:"apiKeyName"`
			Prompts    []AuthPrompt `json:"prompts"`
			LoginError *string      `json:"loginError"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	models := BuiltinModels(nil)
	for _, want := range capture.Providers {
		var diffs []string
		provider := models.GetProvider(want.ID)
		if provider == nil {
			diffs = append(diffs, "no provider")
		} else {
			var name *string
			var prompts []AuthPrompt
			if apiKey := provider.Auth().APIKey; apiKey != nil {
				name = &apiKey.Name
				if apiKey.Login != nil {
					recorder := &promptRecorder{}
					_, _ = apiKey.Login(context.Background(), recorder)
					prompts = recorder.prompts
				}
			}
			if !reflect.DeepEqual(name, want.APIKeyName) {
				got, pi := "<none>", "<none>"
				if name != nil {
					got = *name
				}
				if want.APIKeyName != nil {
					pi = *want.APIKeyName
				}
				diffs = append(diffs, "name "+got+", pi "+pi)
			}
			if !reflect.DeepEqual(prompts, want.Prompts) {
				diffs = append(diffs, "prompts differ")
				t.Logf("%s prompts:\n go %+v\n pi %+v", want.ID, prompts, want.Prompts)
			}
		}
		_, debt := builtinAuthDebt[want.ID]
		switch {
		case debt && len(diffs) == 0:
			t.Errorf("%s now matches pi: remove it from builtinAuthDebt", want.ID)
		case !debt && len(diffs) > 0:
			t.Errorf("%s: %v", want.ID, diffs)
		}
	}
}
