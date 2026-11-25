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
// reason next to finish_reason and an error message gains it; and (9b911b409,
// #12407) a stop without text or tool calls whose native reason is not a clean
// stop is a retryable empty-response error.
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
			name: "records a native reason hidden behind a successful finish_reason without changing a turn with text",
			chunks: []string{
				`{"id":"gen-1","choices":[{"index":0,"delta":{"content":"Done."},"finish_reason":null}]}`,
				`{"id":"gen-1","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopStop,
			wantRaw:    "stop.MALFORMED_RESPONSE",
		},
		{
			name: "retries a stop after reasoning only when the native reason is not a clean stop",
			chunks: []string{
				`{"id":"gen-8","choices":[{"index":0,"delta":{"reasoning":"Let me think."},"finish_reason":null}]}`,
				`{"id":"gen-8","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`,
			},
			openRouter:    true,
			wantStop:      ai.StopError,
			wantRaw:       "stop.MALFORMED_RESPONSE",
			wantErrMsg:    "OpenRouter returned an empty response (native_finish_reason: MALFORMED_RESPONSE)",
			wantRetryable: new(true),
		},
		{
			name: "retries an empty stop whose native reason OpenRouter counts as clean but its provider documents as a failure",
			chunks: []string{
				`{"id":"gen-9","choices":[{"index":0,"delta":{"content":"  "},"finish_reason":null}]}`,
				`{"id":"gen-9","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"OTHER"}]}`,
			},
			openRouter:    true,
			wantStop:      ai.StopError,
			wantRaw:       "stop.OTHER",
			wantErrMsg:    "OpenRouter returned an empty response (native_finish_reason: OTHER)",
			wantRetryable: new(true),
		},
		{
			name: "keeps an empty stop whose native reason is a clean stop",
			chunks: []string{
				`{"id":"gen-10","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"end_turn"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopStop,
			wantRaw:    "stop.end_turn",
		},
		{
			name: "matches clean stop reasons case-insensitively",
			chunks: []string{
				`{"id":"gen-11","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"End_Turn"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopStop,
			wantRaw:    "stop.End_Turn",
		},
		// JavaScript semantics of the empty-stop check: toLowerCase lowers
		// U+0130 to "i" + U+0307, so "COMPACT\u0130ON" is not "compaction"
		// (strings.ToLower would make it one), and trim takes U+FEFF, so text
		// of only that is not visible output (unicode.IsSpace keeps it).
		{
			name: "lowers the native reason as toLowerCase does",
			chunks: []string{
				`{"id":"gen-12","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"COMPACT\u0130ON"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopError,
			wantRaw:    "stop.COMPACT\u0130ON",
			wantErrMsg: "OpenRouter returned an empty response (native_finish_reason: COMPACT\u0130ON)",
		},
		{
			name: "trims text as String.prototype.trim does",
			chunks: []string{
				`{"id":"gen-13","choices":[{"index":0,"delta":{"content":"\ufeff"},"finish_reason":null}]}`,
				`{"id":"gen-13","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`,
			},
			openRouter: true,
			wantStop:   ai.StopError,
			wantRaw:    "stop.MALFORMED_RESPONSE",
			wantErrMsg: "OpenRouter returned an empty response (native_finish_reason: MALFORMED_RESPONSE)",
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
