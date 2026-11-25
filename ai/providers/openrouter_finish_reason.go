package providers

import (
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// OpenRouter's native_finish_reason, ported from pi
// utils/openrouter-finish-reason.ts (upstream ddf5868d2, #11931): the upstream
// provider's own finish reason, which OpenRouter sends next to the
// OpenAI-style finish_reason it normalizes it into. Recorded for diagnostics,
// and used to catch upstream failures that OpenRouter reports as an empty
// stop.

// readOpenRouterNativeFinishReason is pi's readOpenRouterNativeFinishReason:
// the choice's native_finish_reason when it is a non-empty string, an
// OpenRouter extension the OpenAI SDK types do not declare.
func readOpenRouterNativeFinishReason(choice any) (string, bool) {
	native, isString := jsGet(choice, "native_finish_reason").(string)
	return native, isString && native != ""
}

// applyOpenRouterNativeFinishReason is pi's applyOpenRouterNativeFinishReason.
// It records a native reason that differs from finishReason on a message whose
// stop reason was already mapped from finishReason: RawStopReason becomes e.g.
// "stop.MALFORMED_RESPONSE" (the format of openai-responses'
// "incomplete.max_output_tokens"), and an error message gets the native reason
// appended. The stop reason itself is unchanged.
//
// finishReason is the wire value, of whatever type: pi's === never equals a
// non-string to the native reason, and its template literal writes String().
func applyOpenRouterNativeFinishReason(message *ai.AssistantMessage, finishReason any, nativeReason string) {
	if text, isString := finishReason.(string); isString && text == nativeReason {
		return
	}
	message.RawStopReason = jsStringField(finishReason) + "." + nativeReason
	if message.StopReason == ai.StopError && message.ErrorMessage != "" {
		// Appended, not replaced: retry matching relies on the finish_reason message prefix.
		message.ErrorMessage += " (native_finish_reason: " + nativeReason + ")"
	}
}

// cleanStopNativeReasons are the native reasons OpenRouter treats as a clean
// stop. OpenRouter maps any other value to stop as well, so a failure that
// already streamed output (e.g. Gemini reasoning followed by
// MALFORMED_RESPONSE) arrives as stop. Two of OpenRouter's entries are left
// out because their providers document them as failures: Gemini's "OTHER"
// ("stopped for a reason not otherwise specified"; mapGoogleStopReason maps
// it to an error) and DeepSeek's "insufficient_system_resource" ("interrupted
// due to insufficient resource").
//
// OpenRouter does not publish this list, so it can drift. It is an allowlist
// on purpose: a missing clean reason costs a few retries of an empty turn,
// while a missing failure reason would end the turn silently. Lowercase,
// compared case-insensitively, because upstreams differ in case ("STOP",
// "stop").
var cleanStopNativeReasons = map[string]bool{
	"stop":          true,
	"end_turn":      true,
	"stop_sequence": true,
	"eos":           true,
	"complete":      true,
	"completed":     true,
	"pause_turn":    true,
	"compaction":    true,
	"end_of_stream": true,
}

// flagOpenRouterEmptyStop is pi's flagOpenRouterEmptyStop (upstream
// 9b911b409, #12407). It turns a finished stop without visible output (text or
// tool calls; reasoning does not count) into an error when the native reason
// is not a clean stop. The message matches the retry pattern for OpenRouter's
// own "Provider returned an empty response", so the agent retries it. The
// fold is toLowerCase and the emptiness test trim, with JavaScript's semantics
// (jstext): strings.ToLower makes "COMPACTİON" a clean "compaction", and
// strings.TrimSpace keeps U+FEFF as visible text.
func flagOpenRouterEmptyStop(message *ai.AssistantMessage, nativeReason string) {
	if message.StopReason != ai.StopStop || cleanStopNativeReasons[jstext.ToLower(nativeReason)] {
		return
	}
	for _, block := range message.Content {
		switch block := block.(type) {
		case ai.ToolCall:
			return
		case ai.TextContent:
			if jstext.Trim(block.Text) != "" {
				return
			}
		}
	}
	message.StopReason = ai.StopError
	message.ErrorMessage = "OpenRouter returned an empty response (native_finish_reason: " + nativeReason + ")"
}
