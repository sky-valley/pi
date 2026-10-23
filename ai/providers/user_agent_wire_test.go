package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// --- the identity's User-Agent is a DEFAULT, not an override ---
//
// Upstream used to FORCE pi's user agent over every other header source for
// two providers (kimi-coding on anthropic-messages, 9d2ec7ffa; xai on both
// openai adapters, 70e878d4c). 87af49dec spread it FIRST into each adapter's
// header merge instead, and adc5fa9f8 replaced the fixed string with the
// request identity's (ai.FormatUserAgent; pi/<version> (<platform>; <runtime>;
// <arch>) by default), still first: every later source wins. These pin the
// slot and spelling mechanics around it; TestIdentityHeadersMatchPi pins the
// values against pi's own adapters.
//
// Request headers are outside the bodies-only differential harness, so this
// surface is pinned here on the wire instead.

// captureOpenAIResponsesHeaders runs one openai-responses request against a
// local server and returns the headers that reached the wire.
func captureOpenAIResponsesHeaders(t *testing.T, model *ai.Model, opts ai.StreamOptions) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, responsesSSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

		&OpenAIResponsesOptions{StreamOptions: opts}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return got
}

// captureGoogleHeaders runs one google-generative-ai request against a local
// server and returns the headers that reached the wire.
func captureGoogleHeaders(t *testing.T, model *ai.Model, opts ai.StreamOptions) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

		&GoogleOptions{StreamOptions: opts}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return got
}

// captureAnthropicHeaders runs one anthropic-messages request and returns the
// headers that reached the wire.
func captureAnthropicHeaders(t *testing.T, model *ai.Model, opts *AnthropicOptions) http.Header {
	t.Helper()
	headers, _ := anthropicCapture(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts, anthropicSSE)
	return headers
}

// wantUserAgent asserts that exactly one user-agent value reached the wire and
// that it is the expected one. Exactly-once matters on its own: pi collapses
// case-variant header names to a single wire header, and net/http canonicalizes
// to a single key, so a second value would mean the port had grown a header pi
// cannot send.
func wantUserAgent(t *testing.T, h http.Header, want string) {
	t.Helper()
	if got := h.Values("User-Agent"); len(got) != 1 || got[0] != want {
		t.Fatalf("user-agent = %v, want exactly [%q]", got, want)
	}
}

func anthropicUAModel() *ai.Model {
	return &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Input: []string{"text"}, MaxTokens: 4096}
}

func anthropicUAOptions(opts ai.ProviderRequestOptions) *AnthropicOptions {
	return &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: opts}}
}

// Every ported adapter now sends pi's runtime user agent when nothing else
// supplies one. Before 87af49dec only kimi-coding and xai requests carried it;
// plain anthropic, openai-completions, openai-responses and google sent none.
func TestPiUserAgentIsTheDefaultOnEveryAdapter(t *testing.T) {
	t.Run("anthropic api key", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{APIKey: "k"}))
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	// The ANTHROPIC_AUTH_TOKEN bearer branch (upstream 24e5cc04) sits ahead of
	// the other three and merges the same client headers.
	t.Run("anthropic auth token bearer", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			Env: map[string]string{ai.AnthropicAuthTokenEnv: "my-auth-token"},
		}))
		if got := h.Get("authorization"); got != "Bearer my-auth-token" {
			t.Fatalf("authorization = %q, want the bearer branch", got)
		}
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	// The copilot branch, on a model that carries no catalog user agent of its
	// own (the shipped ones do — see TestModelHeadersOverridePiUserAgent).
	t.Run("anthropic github-copilot", func(t *testing.T) {
		model := anthropicUAModel()
		model.Provider = "github-copilot"
		h := captureAnthropicHeaders(t, model, anthropicUAOptions(ai.ProviderRequestOptions{APIKey: "gh-token"}))
		if got := h.Get("authorization"); got != "Bearer gh-token" {
			t.Fatalf("authorization = %q, want the copilot branch", got)
		}
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	t.Run("openai-completions", func(t *testing.T) {
		h := captureOpenAIHeaders(t, openAITestModel(), ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
		})
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	t.Run("openai-responses", func(t *testing.T) {
		model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureOpenAIResponsesHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
		})
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	t.Run("google", func(t *testing.T) {
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureGoogleHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key"},
		})
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	// xai is the interesting one on the openai adapters: it used to force pi's
	// agent last; now it merely gets the same default as everyone else.
	t.Run("xai completions", func(t *testing.T) {
		model := &ai.Model{ID: "grok-custom", Api: ai.APIOpenAICompletions, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureOpenAIHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "xai-test-token"},
		})
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
	t.Run("xai responses", func(t *testing.T) {
		model := &ai.Model{ID: "grok-4.5", Api: ai.APIOpenAIResponses, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureOpenAIResponsesHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "xai-test-token"},
		})
		wantUserAgent(t, h, ai.FormatUserAgent(nil))
	})
}

// THE REVERSAL. A user agent on model.Headers now beats pi's default, where
// before 87af49dec pi's agent was re-set after the whole merge for exactly
// these two providers. Both are asserted by name because both changed meaning.
func TestModelHeadersOverridePiUserAgent(t *testing.T) {
	t.Run("kimi-coding anthropic", func(t *testing.T) {
		model := &ai.Model{ID: "kimi-for-coding", Api: ai.APIAnthropicMessages, Provider: "kimi-coding",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("KimiCLI/1.5")}}
		h := captureAnthropicHeaders(t, model, anthropicUAOptions(ai.ProviderRequestOptions{APIKey: "kimi-key"}))
		wantUserAgent(t, h, "KimiCLI/1.5")
	})
	t.Run("xai completions", func(t *testing.T) {
		model := &ai.Model{ID: "grok-custom", Api: ai.APIOpenAICompletions, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("model-agent")}}
		h := captureOpenAIHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "xai-test-token"},
		})
		wantUserAgent(t, h, "model-agent")
	})
	t.Run("xai responses", func(t *testing.T) {
		model := &ai.Model{ID: "grok-4.5", Api: ai.APIOpenAIResponses, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("model-agent")}}
		h := captureOpenAIResponsesHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "xai-test-token"},
		})
		wantUserAgent(t, h, "model-agent")
	})
	t.Run("google", func(t *testing.T) {
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("model-agent")}}
		h := captureGoogleHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key"},
		})
		wantUserAgent(t, h, "model-agent")
	})
	// The live catalog case: shipped github-copilot models carry
	// User-Agent: GitHubCopilotChat/… , which the reversal now lets through.
	t.Run("github-copilot catalog model", func(t *testing.T) {
		model := ai.GetModel("github-copilot", "claude-haiku-4.5")
		if model == nil {
			t.Skip("github-copilot/claude-haiku-4.5 not in catalog")
		}
		want := model.Headers["User-Agent"]
		if want == nil || *want == "" {
			t.Fatalf("catalog model lost its User-Agent header: %v", model.Headers)
		}
		// The clone exists so anthropicCapture's BaseURL write cannot land on
		// the shared catalog entry ai.GetModel hands back; Headers still alias
		// the catalog map and must stay read-only here.
		clone := *model
		h := captureAnthropicHeaders(t, &clone, anthropicUAOptions(ai.ProviderRequestOptions{APIKey: "gh-token"}))
		wantUserAgent(t, h, *want)
	})
}

// The anthropic OAuth branch sends Claude Code's `user-agent: claude-cli/<v>`
// and no client identity at all (upstream adc5fa9f8).
func TestAnthropicOAuthUserAgentBeatsPiDefault(t *testing.T) {
	h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
		APIKey: "sk-ant-oat-token",
	}))
	wantUserAgent(t, h, "claude-cli/"+claudeCodeVersion)
}

// options.Headers are spread last in every adapter, so a consumer user agent
// beats both pi's default and model.Headers.
func TestOptionsHeadersOverridePiUserAgentAndModelHeaders(t *testing.T) {
	t.Run("anthropic over kimi model header", func(t *testing.T) {
		model := &ai.Model{ID: "kimi-for-coding", Api: ai.APIAnthropicMessages, Provider: "kimi-coding",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("KimiCLI/1.5")}}
		h := captureAnthropicHeaders(t, model, anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "kimi-key",
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("custom-client")},
		}))
		wantUserAgent(t, h, "custom-client")
	})
	// The OAuth branch has no seeded "User-Agent" slot since upstream
	// adc5fa9f8, so a caller's agent in any spelling takes a slot after
	// Claude Code's and wins (TestIdentityHeadersMatchPi's "anthropic OAuth:
	// request …" rows are pi's own answers).
	t.Run("anthropic oauth identity loses to the User-Agent spelling", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "sk-ant-oat-token",
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("custom-client")},
		}))
		wantUserAgent(t, h, "custom-client")
	})
	t.Run("anthropic oauth identity loses to another spelling", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "sk-ant-oat-token",
			Headers: ai.ProviderHeaders{"user-agent": strPtr("custom-client")},
		}))
		wantUserAgent(t, h, "custom-client")
	})
	// A third spelling takes a slot of its own, after the identity's, so it
	// wins there too — the slot, not the letter case, is what decides.
	t.Run("anthropic oauth identity loses to a third spelling", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "sk-ant-oat-token",
			Headers: ai.ProviderHeaders{"USER-AGENT": strPtr("custom-client")},
		}))
		wantUserAgent(t, h, "custom-client")
	})
	t.Run("xai completions", func(t *testing.T) {
		model := &ai.Model{ID: "grok-custom", Api: ai.APIOpenAICompletions, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("model-agent")}}
		h := captureOpenAIHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "xai-test-token",
				Headers: ai.ProviderHeaders{"User-Agent": strPtr("custom-agent")},
			},
		})
		wantUserAgent(t, h, "custom-agent")
	})
	t.Run("xai responses", func(t *testing.T) {
		model := &ai.Model{ID: "grok-4.5", Api: ai.APIOpenAIResponses, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("model-agent")}}
		h := captureOpenAIResponsesHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "xai-test-token",
				Headers: ai.ProviderHeaders{"User-Agent": strPtr("custom-agent")},
			},
		})
		wantUserAgent(t, h, "custom-agent")
	})
	t.Run("google", func(t *testing.T) {
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("model-agent")}}
		h := captureGoogleHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "g-key",
				Headers: ai.ProviderHeaders{"User-Agent": strPtr("custom-agent")},
			},
		})
		wantUserAgent(t, h, "custom-agent")
	})
	// A lowercase consumer spelling must win too: net/http canonicalizes, and
	// so does the SDK pi hands its merged object to.
	t.Run("lowercase spelling on anthropic", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "k",
			Headers: ai.ProviderHeaders{"user-agent": strPtr("custom-client")},
		}))
		wantUserAgent(t, h, "custom-client")
	})
}

// A deletion marker still wins over the new default. Adapters that hand the
// merged headers to an SDK as `defaultHeaders` let a null DELETE the header;
// google converts through providerHeadersToRecord, which since upstream
// a328aa89a folds names case-insensitively, so a null there removes pi's
// default whatever its spelling.
func TestUserAgentMarkerBeatsPiDefault(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "k",
			Headers: ai.ProviderHeaders{"user-agent": nil},
		}))
		// net/http supplies a transport agent once ours is gone, exactly as
		// fetch does for pi; what must not survive is pi's default.
		if got := h.Get("User-Agent"); got == ai.FormatUserAgent(nil) {
			t.Fatalf("a marker must suppress pi's default user agent, got %q", got)
		}
	})
	// With no seeded slot on the OAuth branch, a marker in any spelling
	// deletes Claude Code's agent itself; what reaches the wire then is the
	// transport's default (D12).
	t.Run("anthropic oauth marker removes Claude Code's agent", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "sk-ant-oat-token",
			Headers: ai.ProviderHeaders{"User-Agent": nil},
		}))
		if got := h.Get("User-Agent"); got == "claude-cli/"+claudeCodeVersion || got == ai.FormatUserAgent(nil) {
			t.Fatalf("a marker must suppress every source's user agent, got %q", got)
		}
	})
	t.Run("openai-completions", func(t *testing.T) {
		model := &ai.Model{ID: "grok-custom", Api: ai.APIOpenAICompletions, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureOpenAIHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "xai-test-token",
				Headers: ai.ProviderHeaders{"User-Agent": nil},
			},
		})
		if got := h.Get("User-Agent"); got == ai.FormatUserAgent(nil) {
			t.Fatalf("a marker must suppress pi's default user agent, got %q", got)
		}
	})
	t.Run("openai-responses", func(t *testing.T) {
		model := &ai.Model{ID: "grok-4.5", Api: ai.APIOpenAIResponses, Provider: "xai",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureOpenAIResponsesHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "xai-test-token",
				Headers: ai.ProviderHeaders{"User-Agent": nil},
			},
		})
		if got := h.Get("User-Agent"); got == ai.FormatUserAgent(nil) {
			t.Fatalf("a marker must suppress pi's default user agent, got %q", got)
		}
	})
	// google, name-colliding marker: pi's object becomes {"User-Agent": null},
	// which providerHeadersToRecord drops, so no user agent is sent.
	t.Run("google colliding marker drops the default", func(t *testing.T) {
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureGoogleHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "g-key",
				Headers: ai.ProviderHeaders{"User-Agent": nil},
			},
		})
		if got := h.Get("User-Agent"); got == ai.FormatUserAgent(nil) {
			t.Fatalf("a marker on the same name must drop pi's default, got %q", got)
		}
	})
	// google, differently-spelled marker: pi's object holds both keys, and the
	// record folds them — the null sits in the last slot for "user-agent", so
	// it deletes pi's default instead of being skipped beside it (upstream
	// a328aa89a; before it pi's default survived here). Measured against
	// @google/genai 2.21.0: the record is then empty and the wire carries
	// genai's own `google-genai-sdk/2.21.0 gl-node/<node>`. The port sends
	// net/http's agent instead — the D12 substitution class, which is why only
	// pi's own agent is ruled out here.
	t.Run("google case-variant marker drops the default", func(t *testing.T) {
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
			Input: []string{"text"}, MaxTokens: 4096}
		h := captureGoogleHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "g-key",
				Headers: ai.ProviderHeaders{"user-agent": nil},
			},
		})
		if got := h.Get("User-Agent"); got == ai.FormatUserAgent(nil) {
			t.Fatalf("a case-variant marker must drop pi's default, got %q", got)
		}
	})
}

// A marker in a later source deletes a differently-spelled value an earlier
// source put in the record, not only its own spelling. pi's record for
// model.headers {"X-Trace": "model"} and options.headers {"x-trace": null} is
// {"User-Agent": <pi>}: measured by executing a328aa89a's
// providerHeadersToRecord over google's merged object under node, and on the
// wire through @google/genai 2.21.0 (no x-trace header).
func TestGoogleCaseVariantMarkerDeletesEarlierSource(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		Input: []string{"text"}, MaxTokens: 4096,
		Headers: ai.ProviderHeaders{"X-Trace": strPtr("model")}}
	h := captureGoogleHeaders(t, model, ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:  "g-key",
			Headers: ai.ProviderHeaders{"x-trace": nil},
		},
	})
	if values, present := h["X-Trace"]; present {
		t.Fatalf("X-Trace = %q, want it absent: the later marker deletes the model's spelling", values)
	}
	wantUserAgent(t, h, ai.FormatUserAgent(nil))
}

// The record path must not depend on Go map iteration order either. Two
// spellings inside ONE source have no order to reproduce, so sorted name order
// is the tie-break (see headerObject.merge): "user-agent" sorts after
// "User-Agent", so it takes the last slot and the fold keeps it alone.
func TestHeaderObjectRecordCaseCollisionIsDeterministic(t *testing.T) {
	source := ai.ProviderHeaders{
		"User-Agent": strPtr(ai.FormatUserAgent(nil)),
		"user-agent": strPtr("from-model"),
		"X-Keep":     strPtr("keep"),
		// A marker with no other spelling leaves nothing to write.
		"X-Trace": nil,
	}
	// One run cannot tell a sorted implementation from a lucky one: Go
	// randomizes map iteration per range statement, so repeat enough that an
	// order-dependent implementation is overwhelmingly likely to disagree with
	// itself at least once.
	for i := range 200 {
		h := http.Header{}
		o := &headerObject{}
		o.merge(source)
		if err := o.applyAsRecord(h); err != nil {
			t.Fatal(err)
		}
		if got := h.Values("User-Agent"); len(got) != 1 || got[0] != "from-model" {
			t.Fatalf("run %d: user-agent = %v, want exactly [%q] — the name sorting last must win", i, got, "from-model")
		}
		if got := h.Get("X-Keep"); got != "keep" {
			t.Fatalf("run %d: X-Keep = %q, want keep", i, got)
		}
		if _, present := h["X-Trace"]; present {
			t.Fatalf("run %d: X-Trace = %q, want it never written", i, h.Get("X-Trace"))
		}
	}
}

// The same collision reaching google through the real merge, where the order IS
// known: the seed takes slot 0 and a differently-spelled user agent from a
// later source takes a later slot, so the later source wins whatever its
// spelling sorts like. "USER-AGENT" sorts BEFORE "User-Agent" and used to lose
// here for that reason alone.
//
// What this pins is pi's record, which since upstream a328aa89a holds the
// later spelling alone: {"user-agent": "model-agent"}. pi's wire value is not
// reachable on this adapter: @google/genai seeds its own "User-Agent" default
// and appends the record to it, so pi sends
// `google-genai-sdk/2.21.0 gl-node/<node>, model-agent` (measured against
// @google/genai 2.21.0, the version a328aa89a's package-lock names) where the
// port sends the record's value alone — docs/UPSTREAM.md D10/D12, not asserted
// here.
func TestGoogleCaseCollidingModelUserAgentWins(t *testing.T) {
	for _, name := range []string{"user-agent", "USER-AGENT", "User-agent"} {
		t.Run(name, func(t *testing.T) {
			for i := range 20 {
				model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
					Input: []string{"text"}, MaxTokens: 4096,
					Headers: ai.ProviderHeaders{name: strPtr("model-agent")}}
				h := captureGoogleHeaders(t, model, ai.StreamOptions{
					ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key"},
				})
				if got := h.Values("User-Agent"); len(got) != 1 || got[0] != "model-agent" {
					t.Fatalf("run %d: user-agent = %v, want exactly [%q]", i, got, "model-agent")
				}
			}
		})
	}
}

// On google each source folds into the record on its own (upstream adc5fa9f8:
// providerHeadersToRecord(identity, model.headers, options.headers)), so a
// consumer header wins over the catalog's in any spelling: it deletes every
// earlier spelling of its name and takes the last slot.
func TestOptionsHeadersWinAcrossSpellingsOnGoogle(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		Input: []string{"text"}, MaxTokens: 4096,
		Headers: ai.ProviderHeaders{"user-agent": strPtr("model-agent")}}
	h := captureGoogleHeaders(t, model, ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:  "g-key",
			Headers: ai.ProviderHeaders{"USER-AGENT": strPtr("opts-agent")},
		},
	})
	wantUserAgent(t, h, "opts-agent")
}

// An empty-string user agent is a DIVERGENCE (docs/UPSTREAM.md D11), pinned
// under that row's name so it cannot drift silently. net/http omits the
// User-Agent header entirely when its value is empty (net/http.Request.write
// and the HTTP/2 encoder special-case exactly this one header), so the port
// sends no user agent at all. pi sends it present and empty: upstream
// 8676a0dcd's adapters under node v26.4.0 (@anthropic-ai/sdk 0.124.0,
// @google/genai 2.21.0, the versions its package-lock.json locks) put
// `User-Agent: ` on the wire. Not fixable through http.Header.
//
// A whitespace-only value normalizes to "" first, as fetch's Headers
// normalizes it (TestHeaderValuesAreTrimmedLikeFetch, "all whitespace"), and
// then meets the same drop. That is not pinned here: over HTTP/1.1, which these
// captures use, sending the raw value would put pi's bytes on the wire, and a
// row asserting the header absent would fail exactly then.
func TestEmptyUserAgentIsDroppedEntirely(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "k",
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("")},
		}))
		if values, present := h["User-Agent"]; present {
			t.Fatalf("User-Agent = %q, want the header absent — net/http drops an empty one", values)
		}
	})
	t.Run("google", func(t *testing.T) {
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
			Input: []string{"text"}, MaxTokens: 4096,
			Headers: ai.ProviderHeaders{"User-Agent": strPtr("")}}
		h := captureGoogleHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key"},
		})
		if values, present := h["User-Agent"]; present {
			t.Fatalf("User-Agent = %q, want the header absent — net/http drops an empty one", values)
		}
	})
	// Any other header keeps an empty value, so the drop is the transport's
	// User-Agent special case and not this package losing empty strings.
	t.Run("other headers keep an empty value", func(t *testing.T) {
		h := captureAnthropicHeaders(t, anthropicUAModel(), anthropicUAOptions(ai.ProviderRequestOptions{
			APIKey:  "k",
			Headers: ai.ProviderHeaders{"X-Empty": strPtr("")},
		}))
		if values, present := h["X-Empty"]; !present || len(values) != 1 || values[0] != "" {
			t.Fatalf("X-Empty = %q (present=%v), want exactly one empty value", values, present)
		}
	})
}
