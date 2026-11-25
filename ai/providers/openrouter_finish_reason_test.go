package providers

import (
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// openRouterNativeSSE frames each chunk as an SSE data event and ends the
// stream with [DONE].
func openRouterNativeSSE(chunks ...string) string {
	var b strings.Builder
	for _, chunk := range chunks {
		b.WriteString("data: " + chunk + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func asOpenRouterModel(model *ai.Model) {
	model.ID = "google/gemini-4-neon"
	model.Provider = "openrouter"
}

// TestOpenRouterNativeFinishReason mirrors pi's
// openai-completions-raw-stop-reason.test.ts "OpenRouter native_finish_reason"
// (ddf5868d2, #11931): an OpenRouter model's rawStopReason records the native
// reason next to finish_reason, an error message gains it, and the stop reason
// is unchanged.
func TestOpenRouterNativeFinishReason(t *testing.T) {
	tests := []struct {
		name       string
		chunks     []string
		openRouter bool
		wantStop   ai.StopReason
		wantRaw    string
		wantErrMsg string
		// wantRetryable is asserted where pi's case asserts it.
		wantRetryable *bool
	}{
		{
			name: "records a native reason hidden behind a successful finish_reason without changing the stop reason",
			chunks: []string{
				`{"id":"gen-1","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopStop,
			wantRaw:    "stop.MALFORMED_RESPONSE",
		},
		{
			name: "records the native reason of a tool call",
			chunks: []string{
				`{"id":"gen-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}`,
				// Gemini reports STOP for tool calls; OpenRouter's finish_reason carries the tool call.
				`{"id":"gen-2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls","native_finish_reason":"STOP"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopToolUse,
			wantRaw:    "tool_calls.STOP",
		},
		{
			name: "adds the native reason to an error message and keeps it retryable",
			chunks: []string{
				`{"id":"gen-3","choices":[{"index":0,"delta":{},"finish_reason":"error","native_finish_reason":"MALFORMED_FUNCTION_CALL"}]}`,
			},
			openRouter:    true,
			wantStop:      ai.StopError,
			wantRaw:       "error.MALFORMED_FUNCTION_CALL",
			wantErrMsg:    "Provider finish_reason: error (native_finish_reason: MALFORMED_FUNCTION_CALL)",
			wantRetryable: new(true),
		},
		{
			name: "keeps the native reason when a trailing chunk repeats finish_reason without it",
			chunks: []string{
				`{"id":"gen-4","choices":[{"index":0,"delta":{},"finish_reason":"error","native_finish_reason":"MALFORMED_FUNCTION_CALL"}]}`,
				`{"id":"gen-4","choices":[{"index":0,"delta":{},"finish_reason":"error"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopError,
			wantRaw:    "error.MALFORMED_FUNCTION_CALL",
			wantErrMsg: "Provider finish_reason: error (native_finish_reason: MALFORMED_FUNCTION_CALL)",
		},
		{
			name: "does not repeat a native reason equal to finish_reason",
			chunks: []string{
				`{"id":"gen-5","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"stop"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopStop,
			wantRaw:    "stop",
		},
		{
			name: "ignores native_finish_reason from providers other than OpenRouter",
			chunks: []string{
				`{"id":"chatcmpl-6","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`,
			},
			wantStop: ai.StopStop,
			wantRaw:  "stop",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate func(*ai.Model)
			if tt.openRouter {
				mutate = asOpenRouterModel
			}
			final := runOpenAIStream(t, openRouterNativeSSE(tt.chunks...), mutate)
			if final.StopReason != tt.wantStop {
				t.Errorf("stopReason = %s (%q), want %s", final.StopReason, final.ErrorMessage, tt.wantStop)
			}
			if final.RawStopReason != tt.wantRaw {
				t.Errorf("rawStopReason = %q, want %q", final.RawStopReason, tt.wantRaw)
			}
			if final.ErrorMessage != tt.wantErrMsg {
				t.Errorf("errorMessage = %q, want %q", final.ErrorMessage, tt.wantErrMsg)
			}
			if tt.wantRetryable != nil {
				if got := ai.IsRetryableAssistantError(*final); got != *tt.wantRetryable {
					t.Errorf("IsRetryableAssistantError = %v, want %v", got, *tt.wantRetryable)
				}
			}
		})
	}
}

// TestOpenRouterNativeFinishReasonReading pins readOpenRouterNativeFinishReason's
// guard: only a non-empty string is a native reason.
func TestOpenRouterNativeFinishReasonReading(t *testing.T) {
	for _, tc := range []struct {
		choice string
		want   string
		ok     bool
	}{
		{`{"native_finish_reason":"STOP"}`, "STOP", true},
		{`{"native_finish_reason":""}`, "", false},
		{`{"native_finish_reason":null}`, "", false},
		{`{"native_finish_reason":1}`, "", false},
		{`{}`, "", false},
	} {
		choice, err := ai.DecodeOrderedValue([]byte(tc.choice))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := readOpenRouterNativeFinishReason(choice)
		if got != tc.want || ok != tc.ok {
			t.Errorf("readOpenRouterNativeFinishReason(%s) = %q, %v; want %q, %v", tc.choice, got, ok, tc.want, tc.ok)
		}
	}
}
