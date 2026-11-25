package providers

import "github.com/sky-valley/pi/ai"

// OpenRouter's native_finish_reason, ported from pi
// utils/openrouter-finish-reason.ts (upstream ddf5868d2, #11931): the upstream
// provider's own finish reason, which OpenRouter sends next to the
// OpenAI-style finish_reason it normalizes it into. Recorded for diagnostics
// only.

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
