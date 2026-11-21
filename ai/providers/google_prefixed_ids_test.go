package providers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Prefixed Gemini IDs and compat.supportsStrictMode (pi fe8a6f5dc, #12321).

func googleStrictTestModel(id string, compat string) *ai.Model {
	m := &ai.Model{ID: id, Name: id, Api: ai.APIGoogleGenerativeAI, Provider: "test-google",
		BaseURL: "https://example.invalid/v1beta", Reasoning: true, Input: []string{"text"},
		ContextWindow: 128000, MaxTokens: 4096}
	if compat != "" {
		m.Compat = json.RawMessage(compat)
	}
	return m
}

// pi google-shared-convert-tools.test.ts 'detects prefixed Gemini 3+ IDs and
// lets compat.supportsStrictMode override the detection'.
func TestGoogleStrictToolSamplingPrefixedIDsAndCompat(t *testing.T) {
	for _, tc := range []struct {
		id, compat string
		want       bool
	}{
		{"gemini-3.1-pro-preview", "", true},
		{"gemini-2.5-pro", "", false},
		{"randomprefix-gemini-4-argon", "", true},
		{"vendor-gemini-2.5-pro", "", false},
		{"argon-latest", "", false},
		{"argon-latest", `{"supportsStrictMode":true}`, true},
		{"gemini-3.1-pro-preview", `{"supportsStrictMode":false}`, false},
		// pi's `??` falls through on null, leaving the ID detection standing.
		{"gemini-3.1-pro-preview", `{"supportsStrictMode":null}`, true},
		// A mistyped value costs only itself (applyCompat): pi's compat schema
		// rejects it at load, so the detection stands.
		{"argon-latest", `{"supportsStrictMode":"yes"}`, false},
	} {
		if got := supportsGoogleStrictToolSampling(googleStrictTestModel(tc.id, tc.compat)); got != tc.want {
			t.Errorf("supportsGoogleStrictToolSampling(%q, compat %s) = %v, want %v", tc.id, tc.compat, got, tc.want)
		}
	}
}

// pi google-shared-convert-tools.test.ts 'sends VALIDATED for prefixed Gemini
// 3+ IDs and for compat.supportsStrictMode', through streamSimple's onPayload.
// The Go port has no google-vertex adapter, so only google-generative-ai runs.
func TestGoogleSendsValidatedForPrefixedIDsAndCompat(t *testing.T) {
	capturedMode := func(model *ai.Model) any {
		t.Helper()
		tool := ai.Tool{Name: "test_tool", Description: "A test tool", Parameters: ai.Object()}
		tool.ConstrainedSampling = &ai.ConstrainedSamplingConfig{
			Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer,
		}
		req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hi", 0)}, Tools: []ai.Tool{tool}})
		var payload map[string]any
		opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey: "test",
			OnPayload: func(p any, _ *ai.Model) (any, error) {
				payload = roundtripBody(t, p.(map[string]any))
				return nil, errors.New("payload captured")
			},
		}}}
		final := StreamSimpleGoogle(context.Background(), model, req, opts).Result()
		if !strings.Contains(final.ErrorMessage, "payload captured") {
			t.Fatalf("error = %q, want the capture", final.ErrorMessage)
		}
		cfg, _ := payload["toolConfig"].(map[string]any)
		fcc, _ := cfg["functionCallingConfig"].(map[string]any)
		return fcc["mode"]
	}
	if got := capturedMode(googleStrictTestModel("randomprefix-gemini-4-argon", "")); got != "VALIDATED" {
		t.Errorf("randomprefix-gemini-4-argon mode = %v, want VALIDATED", got)
	}
	if got := capturedMode(googleStrictTestModel("argon-latest", "")); got != nil {
		t.Errorf("argon-latest mode = %v, want none", got)
	}
	if got := capturedMode(googleStrictTestModel("argon-latest", `{"supportsStrictMode":true}`)); got != "VALIDATED" {
		t.Errorf("argon-latest with compat mode = %v, want VALIDATED", got)
	}
}

// pi google-shared-image-tool-result-routing.test.ts 'nests image tool results
// for prefixed Gemini 3+ IDs'.
func TestGoogleToolResultImagePrefixedGemini4Nested(t *testing.T) {
	const id = "randomprefix-gemini-4-argon"
	model := &ai.Model{ID: id, Api: ai.APIGoogleGenerativeAI, Provider: "google", Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{Provider: "google", Model: id, Api: ai.APIGoogleGenerativeAI,
			Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "shot", Arguments: map[string]any{}}}},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "shot", Content: ai.ContentList{
			ai.ImageContent{MimeType: "image/png", Data: "AAAA"},
		}},
	}}
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("expected 3 turns for a nested image tool result, got %d: %v", len(contents), contents)
	}
	fr := firstFunctionResponse(contents)
	imgs, ok := fr["parts"].([]any)
	if !ok || len(imgs) != 1 {
		t.Fatalf("prefixed Gemini 4 must nest images in functionResponse.parts: %v", fr)
	}
	if _, ok := imgs[0].(map[string]any)["inlineData"]; !ok {
		t.Fatalf("nested part missing inlineData: %v", imgs)
	}
}

// The other side of the unanchored version read: a prefixed Gemini 2 ID was a
// non-Gemini ID before, and nested its images; it now gets the separate user
// image turn as gemini-2.5 does (Go-side lock: upstream's nested case above
// passes either way, since unversioned IDs default to nesting).
func TestGoogleToolResultImagePrefixedGemini2SeparateTurn(t *testing.T) {
	const id = "vendor-gemini-2.5-flash"
	model := &ai.Model{ID: id, Api: ai.APIGoogleGenerativeAI, Provider: "google", Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{Provider: "google", Model: id, Api: ai.APIGoogleGenerativeAI,
			Content: ai.ContentList{ai.ToolCall{ID: "c1", Name: "shot", Arguments: map[string]any{}}}},
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "shot", Content: ai.ContentList{
			ai.ImageContent{MimeType: "image/png", Data: "AAAA"},
		}},
	}}
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	if len(contents) != 4 {
		t.Fatalf("expected 4 turns (separate image turn) for a prefixed Gemini 2 ID, got %d: %v", len(contents), contents)
	}
}

// usesGoogleThinkingLevel's Gemini 4+ test now reads getGeminiMajorVersion
// (pi fe8a6f5dc) instead of /gemini-(?:[4-9]|[1-9]\d)/, which also takes the
// gemini-live- form and a zero-padded major (parseInt("04") is 4).
func TestGoogleThinkingLevelGemini4ViaMajorVersion(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"gemini-4-argon", true},
		{"randomprefix-gemini-4-argon", true},
		{"gemini-live-4-flash", true},
		{"gemini-04-argon", true},
		{"gemini-12-pro", true},
		{"gemini-2.5-pro", false},
		{"vendor-gemini-2.5-pro", false},
	} {
		if got := usesGoogleThinkingLevel(&ai.Model{ID: tc.id}); got != tc.want {
			t.Errorf("usesGoogleThinkingLevel(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
