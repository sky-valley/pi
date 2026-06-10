package providers

import "testing"

// D7a: openaiSDKErrorMessage replicates the openai SDK's APIError.makeMessage
// (status-prefixed, error.message extraction with JSON.stringify fallbacks).
func TestOpenAISDKErrorMessage(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{429, `{"error":{"message":"slow down"}}`, "429 slow down"},
		{400, `{"error":{"message":{"k":1}}}`, `400 {"k":1}`},   // non-string message → JSON.stringify(message)
		{400, `{"error":{"code":"bad"}}`, `400 {"code":"bad"}`}, // no message → JSON.stringify(error)
		{400, `{"error":"boom"}`, `400 "boom"`},                 // string error → JSON.stringify(error)
		{400, `{"error":""}`, "400 status code (no body)"},      // falsy error, JSON body → no message
		{400, `{"detail":"x"}`, "400 status code (no body)"},    // JSON body without error field
		{500, "plain text", "500 plain text"},                   // non-JSON body → raw text
		{503, "", "503 status code (no body)"},                  // empty body
		{503, "   ", "503    "},                                 // whitespace body isn't valid JSON → raw text
	}
	for _, c := range cases {
		if got := openaiSDKErrorMessage(c.status, []byte(c.body)); got != c.want {
			t.Errorf("openaiSDKErrorMessage(%d, %q) = %q want %q", c.status, c.body, got, c.want)
		}
	}
	if got := formatResponsesHTTPError(429, []byte(`{"error":{"message":"slow down"}}`)).Error(); got != "OpenAI API error (429): 429 slow down" {
		t.Errorf("formatResponsesHTTPError = %q", got)
	}
}
