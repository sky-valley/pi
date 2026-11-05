package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Upstream 0f4d5e960 (pi test/openai-completions-prompt-cache.test.ts and
// test/openai-responses-prompt-cache.test.ts): compat.promptCacheKeyMode
// decides whether prompt_cache_key is sent. "enabled" sends it through a
// compatible proxy, "disabled" never sends it, and "auto" (or absent) keeps
// each API's earlier behaviour. prompt_cache_retention is unaffected.
func TestOpenAICompletionsPromptCacheKeyMode(t *testing.T) {
	const proxy = "https://proxy.example.com/v1"
	const native = "https://api.openai.com/v1"
	tests := []struct {
		name          string
		baseURL       string
		compat        string
		retention     ai.CacheRetention
		sessionID     string
		wantKey       any // nil = absent
		wantRetention any // nil = absent
	}{
		{"enabled proxy, default retention", proxy, `{"promptCacheKeyMode":"enabled","supportsLongCacheRetention":false}`, "", "session-proxy", "session-proxy", nil},
		{"enabled proxy, short", proxy, `{"promptCacheKeyMode":"enabled","supportsLongCacheRetention":false}`, ai.CacheShort, "session-proxy", "session-proxy", nil},
		{"enabled proxy, long without long support", proxy, `{"promptCacheKeyMode":"enabled","supportsLongCacheRetention":false}`, ai.CacheLong, "session-proxy", "session-proxy", nil},
		{"absent mode, proxy short", proxy, "", ai.CacheShort, "session-proxy", nil, nil},
		{"absent mode, proxy long", proxy, `{"supportsLongCacheRetention":true}`, ai.CacheLong, "session-proxy", "session-proxy", "24h"},
		{"auto mode, proxy short", proxy, `{"promptCacheKeyMode":"auto"}`, ai.CacheShort, "session-proxy", nil, nil},
		{"auto mode, proxy long", proxy, `{"promptCacheKeyMode":"auto","supportsLongCacheRetention":true}`, ai.CacheLong, "session-proxy", "session-proxy", "24h"},
		{"enabled, caching off", proxy, `{"promptCacheKeyMode":"enabled"}`, ai.CacheNone, "session-proxy", nil, nil},
		{"disabled native, short", native, `{"promptCacheKeyMode":"disabled"}`, ai.CacheShort, "session-disabled", nil, nil},
		{"disabled native, long", native, `{"promptCacheKeyMode":"disabled","supportsLongCacheRetention":true}`, ai.CacheLong, "session-disabled", nil, "24h"},
		{"disabled proxy keeps long retention", proxy, `{"promptCacheKeyMode":"disabled","supportsLongCacheRetention":true}`, ai.CacheLong, "session-proxy", nil, "24h"},
		{"enabled proxy, no session id", proxy, `{"promptCacheKeyMode":"enabled"}`, ai.CacheShort, "", nil, nil},
		{"enabled proxy clamps to 64 code points", proxy, `{"promptCacheKeyMode":"enabled"}`, ai.CacheShort, strings.Repeat("𐐀", 67), strings.Repeat("𐐀", 64), nil},
		// pi reads the key bare: a mode string it does not know is neither
		// "enabled" nor "auto", so no key goes.
		{"unknown mode string sends no key", native, `{"promptCacheKeyMode":"sometimes"}`, ai.CacheShort, "s", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: "test-model", Api: ai.APIOpenAICompletions, Provider: "test-proxy", BaseURL: tc.baseURL, MaxTokens: 4096}
			if tc.compat != "" {
				model.Compat = json.RawMessage(tc.compat)
			}
			body := mustBuildOpenAIParams(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hello", 1)}},
				&OpenAIOptions{StreamOptions: ai.StreamOptions{CacheRetention: tc.retention, SessionID: tc.sessionID}})
			if got := body["prompt_cache_key"]; got != tc.wantKey {
				t.Errorf("prompt_cache_key = %#v, want %#v", got, tc.wantKey)
			}
			if got := body["prompt_cache_retention"]; got != tc.wantRetention {
				t.Errorf("prompt_cache_retention = %#v, want %#v", got, tc.wantRetention)
			}
		})
	}
}

func TestOpenAIResponsesPromptCacheKeyMode(t *testing.T) {
	tests := []struct {
		name      string
		compat    string
		retention ai.CacheRetention
		wantKey   any
	}{
		{"absent mode sends the key", `{}`, ai.CacheShort, "session-1"},
		{"auto sends the key", `{"promptCacheKeyMode":"auto"}`, ai.CacheShort, "session-1"},
		{"enabled sends the key", `{"promptCacheKeyMode":"enabled"}`, ai.CacheLong, "session-1"},
		{"disabled sends none", `{"promptCacheKeyMode":"disabled"}`, ai.CacheShort, nil},
		{"disabled sends none with long retention", `{"promptCacheKeyMode":"disabled"}`, ai.CacheLong, nil},
		{"caching off sends none", `{"promptCacheKeyMode":"enabled"}`, ai.CacheNone, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: "test-model", Api: ai.APIOpenAIResponses, Provider: "test-proxy",
				BaseURL: "https://proxy.example.com/v1", MaxTokens: 4096, Compat: json.RawMessage(tc.compat)}
			body := mustBuildResponsesParams(t, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hello", 1)}},
				&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{CacheRetention: tc.retention, SessionID: "session-1"}})
			if got := body["prompt_cache_key"]; got != tc.wantKey {
				t.Errorf("prompt_cache_key = %#v, want %#v", got, tc.wantKey)
			}
		})
	}
}
