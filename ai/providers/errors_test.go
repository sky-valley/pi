package providers

import (
	"strings"
	"testing"
)

// TestTruncateErrorText locks pi's truncateErrorText (error-body.ts,
// MAX_PROVIDER_ERROR_BODY_CHARS): bodies at/under the cap pass through; over the
// cap they are sliced to maxChars with the byte-exact suffix
// "... [truncated <N> chars]" where N is counted in UTF-16 code units.
func TestTruncateErrorText(t *testing.T) {
	if got := truncateErrorText("short", 4000); got != "short" {
		t.Errorf("under cap mutated: %q", got)
	}
	if got := truncateErrorText(strings.Repeat("a", 4000), 4000); got != strings.Repeat("a", 4000) {
		t.Errorf("exactly-at-cap mutated: len=%d", len(got))
	}
	// 4002 ASCII chars → kept 4000 + suffix counting the dropped 2.
	over := strings.Repeat("a", 4002)
	want := strings.Repeat("a", 4000) + "... [truncated 2 chars]"
	if got := truncateErrorText(over, 4000); got != want {
		t.Errorf("over cap = %q want suffix %q", got[len(got)-30:], "... [truncated 2 chars]")
	}
	// UTF-16 counting: an astral char is 2 code units. "😀😀😀" = 6 units; cap 4
	// keeps the first two emoji (4 units) and reports 2 dropped units.
	if got := truncateErrorText("😀😀😀", 4); got != "😀😀... [truncated 2 chars]" {
		t.Errorf("utf16 truncate = %q", got)
	}
}

// TestFormatProviderErrorTruncation locks that formatProviderError caps the
// body-derived message at 4000 UTF-16 units (the new upstream 6fbeba51 cap).
// Before the change the full message would be surfaced uncapped.
func TestFormatProviderErrorTruncation(t *testing.T) {
	long := strings.Repeat("x", 5000)
	body := []byte(`{"error":{"message":"` + long + `"}}`)
	got := formatProviderError("OpenAI", 500, body).Error()
	wantSuffix := "... [truncated 1000 chars]"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("formatProviderError not truncated; suffix=%q", got[len(got)-40:])
	}
	wantPrefix := "OpenAI API error 500: " + strings.Repeat("x", 4000) + "..."
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("formatProviderError prefix mismatch")
	}
}

// TestOpenAISDKErrorMessageTruncation locks the cap on the openai-responses
// path: the body-derived msg is capped before the "<status> " prefix is added.
func TestOpenAISDKErrorMessageTruncation(t *testing.T) {
	long := strings.Repeat("y", 5000)
	got := openaiSDKErrorMessage(429, []byte(`{"error":{"message":"`+long+`"}}`))
	if !strings.HasPrefix(got, "429 "+strings.Repeat("y", 4000)+"...") {
		t.Errorf("openaiSDKErrorMessage not capped at body: prefix=%q", got[:30])
	}
	if !strings.HasSuffix(got, "... [truncated 1000 chars]") {
		t.Errorf("openaiSDKErrorMessage suffix=%q", got[len(got)-40:])
	}
}

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
