package providers

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Sign in with ChatGPT (upstream 02eed88fd). These transliterate
// openai-responses-chatgpt-sign-in.test.ts and
// openai-responses-usage-limit.test.ts. Upstream asserts the usage-limit
// messages with toContain; the exact strings here are what pi's stream
// produced for the same responses at 3dd803d7e.

func chatGPTSignInModel() *ai.Model {
	return &ai.Model{
		ID: "gpt-5-mini", Name: "GPT-5 Mini", Api: ai.APIOpenAIResponses, Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Reasoning: true, Input: []string{"text"},
		ContextWindow: 400000, MaxTokens: 128000,
	}
}

var chatGPTSignInContext = ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 0)}}

// captureChatGPTSignInPayload returns the body pi's capturePayload sees, with
// maxTokens 1000, temperature 0.5 and cacheRetention "long". OnPayload stops
// the stream before the request is sent.
func captureChatGPTSignInPayload(t *testing.T, apiKey string, model *ai.Model) map[string]any {
	t.Helper()
	errStop := errors.New("payload captured")
	maxTokens, temperature := 1000, 0.5
	var payload map[string]any
	opts := &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey: apiKey,
			OnPayload: func(p any, _ *ai.Model) (any, error) {
				payload, _ = p.(map[string]any)
				return nil, errStop
			},
		},
		MaxTokens:      &maxTokens,
		Temperature:    &temperature,
		CacheRetention: ai.CacheLong,
	}}
	StreamOpenAIResponses(t.Context(), model, ai.NormalizeContext(chatGPTSignInContext), opts).Result()
	if payload == nil {
		t.Fatal("request payload was not captured")
	}
	return payload
}

func TestResponsesChatGPTSignInOmitsRejectedFields(t *testing.T) {
	payload := captureChatGPTSignInPayload(t, "chatgpt-access-token", chatGPTSignInModel())
	for _, key := range []string{"max_output_tokens", "temperature", "prompt_cache_retention"} {
		if v, ok := payload[key]; ok {
			t.Errorf("%s = %v, want it omitted for a ChatGPT access token", key, v)
		}
	}
}

func TestResponsesChatGPTSignInOmitsPromptCacheOptions(t *testing.T) {
	model := chatGPTSignInModel()
	model.Compat = []byte(`{"supportsExplicitPromptCacheMode":true}`)

	signIn := captureChatGPTSignInPayload(t, "chatgpt-access-token", model)
	apiKey := captureChatGPTSignInPayload(t, "sk-proj-test", model)

	if v, ok := signIn["prompt_cache_options"]; ok {
		t.Errorf("sign-in prompt_cache_options = %v, want it omitted", v)
	}
	if want := map[string]any{"ttl": "30m"}; !reflect.DeepEqual(apiKey["prompt_cache_options"], want) {
		t.Errorf("API-key prompt_cache_options = %v, want %v", apiKey["prompt_cache_options"], want)
	}
}

func TestResponsesChatGPTSignInKeepsFieldsOtherwise(t *testing.T) {
	gateway := chatGPTSignInModel()
	gateway.BaseURL = "https://gateway.example.com/v1"
	// Not an upstream case: the check names the openai provider, so another
	// provider sending its key to OpenAI's URL keeps the fields.
	custom := chatGPTSignInModel()
	custom.Provider = "my-openai"
	for _, tc := range []struct {
		name, apiKey string
		model        *ai.Model
	}{
		{"OpenAI API keys", "sk-proj-test", chatGPTSignInModel()},
		{"other OpenAI-compatible endpoints", "gateway-key", gateway},
		{"other providers at OpenAI's URL", "proj-key", custom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := captureChatGPTSignInPayload(t, tc.apiKey, tc.model)
			if got := payload["max_output_tokens"]; got != 1000 {
				t.Errorf("max_output_tokens = %v, want 1000", got)
			}
			if got := payload["temperature"]; got != 0.5 {
				t.Errorf("temperature = %v, want 0.5", got)
			}
			if got := payload["prompt_cache_retention"]; got != "24h" {
				t.Errorf("prompt_cache_retention = %v, want 24h", got)
			}
		})
	}
}

const chatGPTUsageHint = "\nCheck your ChatGPT usage: https://chatgpt.com/settings/usage"

// chatGPTUsageLimitError is the error message a canned response produces.
func chatGPTUsageLimitError(t *testing.T, doer cannedDoer) string {
	t.Helper()
	opts := &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test", HTTPClient: doer},
	}}
	final := StreamOpenAIResponses(t.Context(), chatGPTSignInModel(), ai.NormalizeContext(chatGPTSignInContext), opts).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
	return final.ErrorMessage
}

func TestResponsesChatGPTUsageLimitLinksToUsage(t *testing.T) {
	t.Run("the request is rejected", func(t *testing.T) {
		msg := chatGPTUsageLimitError(t, cannedDoer{
			status: http.StatusTooManyRequests,
			body:   `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"Usage limit reached.","type":"rate_limit_error"}}`,
		})
		want := `OpenAI API error (429): {"code":"subscription_sharing_usage_limit_exceeded","message":"Usage limit reached.","type":"rate_limit_error"}` + chatGPTUsageHint
		if msg != want {
			t.Errorf("errorMessage = %q, want %q", msg, want)
		}
	})
	t.Run("the stream fails", func(t *testing.T) {
		msg := chatGPTUsageLimitError(t, cannedDoer{
			status: http.StatusOK,
			body: "event: response.failed\ndata: " +
				`{"type":"response.failed","sequence_number":0,"response":{"id":"resp_failed","status":"failed","error":{"code":"subscription_sharing_usage_limit_exceeded","message":"Usage limit reached."}}}` +
				"\n\n",
		})
		if want := "subscription_sharing_usage_limit_exceeded: Usage limit reached." + chatGPTUsageHint; msg != want {
			t.Errorf("errorMessage = %q, want %q", msg, want)
		}
	})
}

// pi's CHATGPT_WORKSPACE_LIMIT_HINT (upstream cedefc532).
const chatGPTWorkspaceHint = "\nYou've reached your ChatGPT workspace's usage limit. Ask your workspace admin to increase it at admin.openai.com → Workspace → Usage limits."

func TestResponsesChatGPTWorkspaceLimitAsksTheAdmin(t *testing.T) {
	t.Run("the request is rejected", func(t *testing.T) {
		msg := chatGPTUsageLimitError(t, cannedDoer{
			status: http.StatusTooManyRequests,
			body:   `{"error":{"code":"subscription_sharing_workplace_individual_usage_limit_exceeded","message":"Usage limit reached.","type":"rate_limit_error"}}`,
		})
		want := `OpenAI API error (429): {"code":"subscription_sharing_workplace_individual_usage_limit_exceeded","message":"Usage limit reached.","type":"rate_limit_error"}` + chatGPTWorkspaceHint
		if msg != want {
			t.Errorf("errorMessage = %q, want %q", msg, want)
		}
	})
	t.Run("the stream fails", func(t *testing.T) {
		msg := chatGPTUsageLimitError(t, cannedDoer{
			status: http.StatusOK,
			body: "event: response.failed\ndata: " +
				`{"type":"response.failed","sequence_number":0,"response":{"id":"resp_failed","status":"failed","error":{"code":"subscription_sharing_workplace_individual_usage_limit_exceeded","message":"Usage limit reached."}}}` +
				"\n\n",
		})
		if want := "subscription_sharing_workplace_individual_usage_limit_exceeded: Usage limit reached." + chatGPTWorkspaceHint; msg != want {
			t.Errorf("errorMessage = %q, want %q", msg, want)
		}
	})
}
