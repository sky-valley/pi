package coding

import (
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// TestImportingCodingRegistersBuiltinProviders pins the SDK contract from the
// Job Market regression: a consumer that only imports the coding package and
// follows ResolveModel → NewSession → Run must never hit
// "No API provider registered" for a built-in API. pi guarantees this via the
// module-load side effect of importing pi-ai; the Go port via init() in
// ai/providers plus coding's import of it.
func TestImportingCodingRegistersBuiltinProviders(t *testing.T) {
	for _, api := range []ai.Api{
		ai.APIAnthropicMessages,
		ai.APIOpenAICompletions,
		ai.APIOpenAIResponses,
		ai.APIGoogleGenerativeAI,
	} {
		if _, ok := ai.GetApiProvider(api); !ok {
			t.Fatalf("api %q not registered by importing the coding package", api)
		}
	}
}

// TestResolvedCatalogModelIsStreamable walks the consumer path end-to-end with
// a real catalog model and asserts the first Run failure is NOT the
// registration error (it must be a transport/auth-level failure instead — we
// point BaseURL at an unroutable address so no real request leaves the box).
func TestResolvedCatalogModelIsStreamable(t *testing.T) {
	model, err := ResolveModel("openai/gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	clone := *model
	clone.BaseURL = "http://127.0.0.1:1" // unroutable: fail fast at dial
	t.Setenv("OPENAI_API_KEY", "test-key")
	sess := newTestSession(t, SessionOptions{Model: &clone, Cwd: t.TempDir(), MaxRetries: 0})
	res, err := sess.Run(t.Context(), "hello")
	if err != nil {
		if strings.Contains(err.Error(), "No API provider registered") {
			t.Fatalf("registration error on the SDK path: %v", err)
		}
		return // any other (transport) error is the expected outcome
	}
	if strings.Contains(res.ErrorMessage, "No API provider registered") {
		t.Fatalf("registration error on the SDK path: %s", res.ErrorMessage)
	}
	if res.StopReason != ai.StopError {
		t.Fatalf("expected a transport-level error result, got stopReason %q", res.StopReason)
	}
}
