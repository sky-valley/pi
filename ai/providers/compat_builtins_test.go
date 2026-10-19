package providers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/telemetry"
)

// compatCloudflareOutcome streams a built-in Cloudflare model through
// compat's Stream with no key and no Cloudflare environment, under a caller
// span, and reports the result's error and the names of the spans recorded.
// pi's compat sends such a request through compatModels — failing setup with
// "Provider is not configured" under a pi.ai.request span — only while the
// registry holds the built-in implementation of the model's api; otherwise it
// goes straight to the registered provider, unobserved.
func compatCloudflareOutcome(t *testing.T) (string, []string) {
	t.Helper()
	setCloudflareEnv(t, nil)
	model := ai.GetModel("cloudflare-workers-ai", "@cf/deepseek-ai/deepseek-v4-flash-0731")
	if model == nil {
		t.Fatal("cloudflare-workers-ai/@cf/deepseek-ai/deepseek-v4-flash-0731 is not in the catalog")
	}
	recorder := telemetry.NewInMemoryContext()
	var result *ai.AssistantMessage
	_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		options := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{Telemetry: span}}
		result = ai.Stream(context.Background(), model, ai.Context{}, &options).Result()
		return nil
	})
	var names []string
	for _, span := range recorder.Spans() {
		names = append(names, span.Name)
	}
	return result.ErrorMessage, names
}

// runCompatChild runs scenario in a fresh test process, so the compat state it
// sees is the one this package's import leaves, as pi's is at compat load.
func runCompatChild(t *testing.T, scenario string) {
	t.Helper()
	if os.Getenv("PI_COMPAT_CHILD") != "" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "PI_COMPAT_CHILD="+scenario)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("child process ran no test:\n%s", out)
	}
}

// TestCompatOverrideRegisteredBeforeFirstUseStreamsDirect: pi records the
// built-in api implementations when compat loads, so an implementation a
// host registers afterwards — before or after its first Cloudflare request —
// is an override, and compat streams a Cloudflare model on its api straight
// to it (compat.ts getBuiltinProviderForModel).
func TestCompatOverrideRegisteredBeforeFirstUseStreamsDirect(t *testing.T) {
	runCompatChild(t, "override")
	if os.Getenv("PI_COMPAT_CHILD") != "override" {
		return
	}
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIOpenAICompletions,
		Stream: func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.StreamOptions) *ai.AssistantMessageEventStream {
			return ai.ErrorStream(model, errors.New("override"))
		},
	}, "compat-override-test")
	message, spans := compatCloudflareOutcome(t)
	if message != "override" || len(spans) != 1 {
		t.Errorf("got %q with spans %v, pi \"override\" with spans [caller]", message, spans)
	}
}

// TestCompatBuiltinsReregisteredKeepTheirRouting: running RegisterBuiltins
// again, as pi's registerBuiltInApiProviders may run again, leaves the
// built-in implementations in place and records them anew, so compat keeps
// routing an unkeyed Cloudflare model through the built-in Models.
func TestCompatBuiltinsReregisteredKeepTheirRouting(t *testing.T) {
	runCompatChild(t, "reregister")
	if os.Getenv("PI_COMPAT_CHILD") != "reregister" {
		return
	}
	const notConfigured = "Provider is not configured: cloudflare-workers-ai"
	if message, spans := compatCloudflareOutcome(t); message != notConfigured || len(spans) != 2 {
		t.Fatalf("before: got %q with spans %v, pi %q with spans [caller pi.ai.request]", message, spans, notConfigured)
	}
	RegisterBuiltins()
	if message, spans := compatCloudflareOutcome(t); message != notConfigured || len(spans) != 2 {
		t.Errorf("after RegisterBuiltins: got %q with spans %v, pi %q with spans [caller pi.ai.request]", message, spans, notConfigured)
	}
}

// TestBuiltinModelsStreamThroughTheBuiltins: pi's builtinModels() streams
// through its own api implementations, whatever the registry holds, so an
// implementation a host registers changes neither BuiltinModels nor the
// compat Models an unkeyed Cloudflare model routes through once the
// built-ins are recorded again (probed on npm 1.3.0: both reach
// https://api.cloudflare.com/.../accounts/a/ai/v1/chat/completions with the
// override registered).
func TestBuiltinModelsStreamThroughTheBuiltins(t *testing.T) {
	t.Cleanup(func() {
		ai.UnregisterApiProviders("builtin-models-override-test")
		RegisterBuiltins()
	})
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIOpenAICompletions,
		Stream: func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.StreamOptions) *ai.AssistantMessageEventStream {
			return ai.ErrorStream(model, errors.New("override"))
		},
	}, "builtin-models-override-test")
	RegisterBuiltins()
	setCloudflareEnv(t, map[string]string{"CLOUDFLARE_API_KEY": "k", "CLOUDFLARE_ACCOUNT_ID": "a"})
	model := ai.GetModel("cloudflare-workers-ai", "@cf/deepseek-ai/deepseek-v4-flash-0731")
	const want = "https://api.cloudflare.com/client/v4/accounts/a/ai/v1/chat/completions"
	for name, stream := range map[string]func(ai.StreamOptions) *ai.AssistantMessage{
		"BuiltinModels": func(options ai.StreamOptions) *ai.AssistantMessage {
			return ai.BuiltinModels().Stream(context.Background(), model, ai.Context{}, &ai.ModelsStreamOptions{StreamOptions: options}).Result()
		},
		"compat": func(options ai.StreamOptions) *ai.AssistantMessage {
			return ai.Stream(context.Background(), model, ai.Context{}, &options).Result()
		},
	} {
		recorder := &cloudflareRecorder{}
		result := stream(ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{HTTPClient: recorder}})
		if len(recorder.requests) != 1 || recorder.requests[0].URL != want {
			t.Errorf("%s: requests %v (%q), pi one to %s", name, recorder.requests, result.ErrorMessage, want)
		}
	}
}
