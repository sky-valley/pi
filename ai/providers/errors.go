package providers

import (
	"encoding/json"
	"fmt"
	"strings"
)

// formatProviderError builds a concise error from an HTTP error response,
// extracting the provider's structured error message when present (OpenAI,
// Anthropic, and Google all nest it under "error": {"message": ...}).
func formatProviderError(label string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
		if parsed.Error.Code != "" {
			msg = fmt.Sprintf("%s (%s)", msg, parsed.Error.Code)
		}
	}
	if len(msg) > 600 {
		msg = msg[:600] + "…"
	}
	return fmt.Errorf("%s API error %d: %s", label, status, msg)
}
