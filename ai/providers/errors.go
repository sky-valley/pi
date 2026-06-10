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
	return fmt.Errorf("%s API error %d: %s", label, status, msg)
}

// formatResponsesHTTPError ports the error message pi's OpenAI Responses
// provider surfaces for a non-2xx HTTP response: formatOpenAIResponsesError
// (openai-responses.ts:55-69) wraps the openai SDK's APIError, whose own
// message is `${status} ${msg}` (openai@6 core/error.ts makeMessage), giving
// e.g. `OpenAI API error (429): 429 slow down`.
func formatResponsesHTTPError(status int, body []byte) error {
	return fmt.Errorf("OpenAI API error (%d): %s", status, openaiSDKErrorMessage(status, body))
}

// openaiSDKErrorMessage replicates openai SDK APIError.makeMessage plus the
// client's body handling: the body is parsed as JSON (any JSON value); the
// message comes from errJSON.error.message (stringified when non-string),
// else JSON.stringify(errJSON.error) when error is truthy, else the raw body
// text when the body wasn't JSON.
func openaiSDKErrorMessage(status int, body []byte) string {
	errText := string(body)
	var errJSON any
	jsonOK := strings.TrimSpace(errText) != "" && json.Unmarshal(body, &errJSON) == nil

	var msg string
	if jsonOK {
		if obj, ok := errJSON.(map[string]any); ok {
			if errVal, has := obj["error"]; has && jsTruthy(errVal) {
				if em, ok := errVal.(map[string]any); ok {
					if m, has := em["message"]; has && jsTruthy(m) {
						if s, ok := m.(string); ok {
							msg = s
						} else if j, err := json.Marshal(m); err == nil {
							msg = string(j)
						}
					}
				}
				if msg == "" {
					if j, err := json.Marshal(errVal); err == nil {
						msg = string(j)
					}
				}
			}
		}
	} else {
		msg = errText
	}
	if msg == "" {
		return fmt.Sprintf("%d status code (no body)", status)
	}
	return fmt.Sprintf("%d %s", status, msg)
}

// jsTruthy reports JavaScript truthiness for a JSON-decoded value.
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return true // objects and arrays are always truthy
	}
}
