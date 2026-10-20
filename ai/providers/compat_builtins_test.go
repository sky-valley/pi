package providers

import (
	"context"
	"encoding/json"
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

// TestCompatBuiltinsRecordedAgainWithoutArguments: pi's
// registerBuiltInApiProviders() takes no arguments and records, for every
// built-in api, whatever the registry holds now — so an override a host
// registered becomes the baseline, and compat routes an unkeyed Cloudflare
// model through the built-in Models again while the registry keeps the
// override. Probed on npm 1.3.0: "override" with the override registered,
// "Provider is not configured: cloudflare-workers-ai" after
// registerBuiltInApiProviders(), and the registry entry still answers
// "override".
func TestCompatBuiltinsRecordedAgainWithoutArguments(t *testing.T) {
	runCompatChild(t, "record-again")
	if os.Getenv("PI_COMPAT_CHILD") != "record-again" {
		return
	}
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIOpenAICompletions,
		Stream: func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.StreamOptions) *ai.AssistantMessageEventStream {
			return ai.ErrorStream(model, errors.New("override"))
		},
	}, "compat-record-again-test")
	if message, spans := compatCloudflareOutcome(t); message != "override" || len(spans) != 1 {
		t.Fatalf("override registered: got %q with spans %v, pi \"override\" with spans [caller]", message, spans)
	}
	ai.RegisterBuiltInApiProviders()
	const notConfigured = "Provider is not configured: cloudflare-workers-ai"
	if message, spans := compatCloudflareOutcome(t); message != notConfigured || len(spans) != 2 {
		t.Errorf("after RegisterBuiltInApiProviders(): got %q with spans %v, pi %q with spans [caller pi.ai.request]", message, spans, notConfigured)
	}
	registered, _ := ai.GetApiProvider(ai.APIOpenAICompletions)
	model := ai.GetModel("cloudflare-workers-ai", "@cf/deepseek-ai/deepseek-v4-flash-0731")
	if got := registered.Stream(context.Background(), model, ai.TranscriptContext{}, nil).Result().ErrorMessage; got != "override" {
		t.Errorf("registry entry answers %q, pi keeps the override (\"override\")", got)
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
			return ai.BuiltinModels(nil).Stream(context.Background(), model, ai.Context{}, &ai.ModelsStreamOptions{StreamOptions: options}).Result()
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

// unportedApisCapture is testdata/unported-apis: for one catalog model per
// built-in api, what pi says when its built-in provider has no implementation
// of the api (createProvider's dispatch) and what compat says once a host
// unregistered the api (resolveApiProvider).
type unportedApisCapture struct {
	Rows []struct {
		Api          ai.Api `json:"api"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Dispatch     string `json:"dispatch"`
		Unregistered string `json:"unregistered"`
	} `json:"rows"`
}

func loadUnportedApisCapture(t *testing.T) unportedApisCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/unported-apis/unported-apis-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture unportedApisCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) == 0 {
		t.Fatal("unported-apis capture has no rows")
	}
	return capture
}

// TestUnportedApisFailAsPisDispatch: a built-in model whose api the port has
// not ported (no implementation handed over, so none registered and none
// recorded) fails as pi's built-in provider does without that api's
// implementation — createProvider's dispatch error — through compat's Stream
// and StreamSimple. It is pi's compat
// algorithm applied to that state: no registry entry and no recorded
// built-in compare equal, so getBuiltinProviderForModel hands the request to
// the built-in provider. An api a host registered and then unregistered
// fails as pi's compat does, with resolveApiProvider's error (encoded in the
// stream: G3).
func TestUnportedApisFailAsPisDispatch(t *testing.T) {
	capture := loadUnportedApisCapture(t)
	if os.Getenv("PI_COMPAT_CHILD") == "" {
		unported := 0
		for _, row := range capture.Rows {
			if _, ported := ai.GetApiProvider(row.Api); ported {
				continue
			}
			unported++
			model := ai.GetModel(row.Provider, row.Model)
			if model == nil {
				t.Fatalf("%s/%s is not in the catalog", row.Provider, row.Model)
			}
			for path, stream := range map[string]func() *ai.AssistantMessage{
				"compat Stream": func() *ai.AssistantMessage {
					return ai.Stream(context.Background(), model, ai.Context{}, nil).Result()
				},
				"compat StreamSimple": func() *ai.AssistantMessage {
					return ai.StreamSimple(context.Background(), model, ai.Context{}, nil).Result()
				},
			} {
				if got := stream().ErrorMessage; got != row.Dispatch {
					t.Errorf("%s %s: %q, pi %q", row.Api, path, got, row.Dispatch)
				}
			}
		}
		// When every api is ported this half covers nothing: delete it.
		if unported == 0 {
			t.Fatal("every built-in api is ported: the dispatch half covers nothing")
		}
	}

	runCompatChild(t, "unregistered")
	if os.Getenv("PI_COMPAT_CHILD") != "unregistered" {
		return
	}
	for _, row := range capture.Rows {
		if _, ported := ai.GetApiProvider(row.Api); !ported {
			continue
		}
		model := ai.GetModel(row.Provider, row.Model)
		if model == nil {
			t.Fatalf("%s/%s is not in the catalog", row.Provider, row.Model)
		}
		ai.RegisterApiProvider(ai.ApiProvider{Api: row.Api}, "unported-apis-test")
		ai.UnregisterApiProviders("unported-apis-test")
		if got := ai.StreamSimple(context.Background(), model, ai.Context{}, nil).Result().ErrorMessage; got != row.Unregistered {
			t.Errorf("%s unregistered: %q, pi %q", row.Api, got, row.Unregistered)
		}
	}
}
