package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/sky-valley/pi/internal/jstext"
)

// maxProviderErrorBodyChars caps the surfaced HTTP error body, matching pi's
// MAX_PROVIDER_ERROR_BODY_CHARS (error-body.ts). Upstream 6fbeba51 added this
// cap so a verbose proxy/gateway error body cannot dominate the surfaced
// message (the string can land in a recorded error turn's session JSON).
const maxProviderErrorBodyChars = 4000

// providerStoppedPrefix is the prefix pi puts in front of a provider's own
// terminal stop reason (anthropic-messages.ts, google-generative-ai.ts). This
// package reproduces pi's user-facing strings byte-exactly, capitalization
// included.
const providerStoppedPrefix = "Provider stopped with: "

// truncateErrorText ports pi's truncateErrorText (error-body.ts). JS measures
// with String.length / String.slice, i.e. UTF-16 code units, so the cap and the
// "[truncated N chars]" count are UTF-16-unit based, not byte- or rune-based.
// The suffix string is matched byte-exactly.
func truncateErrorText(text string, maxChars int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= maxChars {
		return text
	}
	head := string(utf16.Decode(units[:maxChars]))
	return fmt.Sprintf("%s... [truncated %d chars]", head, len(units)-maxChars)
}

// formatProviderError builds a concise error from an HTTP error response,
// extracting the provider's structured error message when present (OpenAI,
// Anthropic, and Google all nest it under "error": {"message": ...}).
//
// Architecture note (upstream 6fbeba51): pi's normalizeProviderError recovers
// the HTTP status and raw body from the JS provider SDKs' opaque error objects
// (.statusCode/.error/.body/$response/$metadata). The Go port issues raw HTTP
// requests and already holds resp.StatusCode and the raw body here, so the
// field-probing half is N/A — the #5763 "opaque, no body" bug cannot occur. Its
// other half is observable, though: for the openai SDK, a parsed `error` object
// REPLACES the SDK's message in what the user sees. formatResponsesHTTPError
// and formatCompletionsHTTPError port that composition against a captured
// oracle, and anthropicSDKErrorMessage the anthropic one; this function keeps
// the port's own `<Label> API error <status>: <msg>` shape for the google call
// site (docs/UPSTREAM.md K18) and applies 6fbeba51's 4000-char cap to the
// body-derived message.
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
	msg = truncateErrorText(msg, maxProviderErrorBodyChars)
	return fmt.Errorf("%s API error %d: %s", label, status, msg)
}

// formatResponsesHTTPError ports the error message pi's OpenAI Responses
// provider surfaces for a non-2xx HTTP response — the openai-responses.ts
// catch: formatProviderError(normalizeProviderError(err), prefix), where err is
// the openai SDK's APIError (openaiStatusError) and the prefix names the
// provider, "OpenAI" for provider "openai" and the id verbatim otherwise
// (upstream 0c7bb7c5c, #9298: a Grok 403 used to read as an OpenAI error).
//
// Composition, byte-exact to the captured oracle in testdata/httperror: the
// SDK message is `${status} ${msg}`; when the APIError's `error` is a
// non-empty plain object, its JSON.stringify form, capped at
// maxProviderErrorBodyChars, replaces the message unless the message already
// contains it (error-body.ts messageCarriesBody). So
// `{"error":{"message":"blocked"}}` reads `xai API error (403): {"message":"blocked"}`
// while `{"error":"boom"}` reads `xai API error (403): 403 "boom"`.
func formatResponsesHTTPError(provider string, status int, body []byte) error {
	label := provider
	if provider == "openai" {
		label = "OpenAI"
	}
	e := newOpenAIStatusError(body)
	message := e.message(status)
	if errBody := e.body(); errBody != "" && !strings.Contains(message, errBody) {
		return fmt.Errorf("%s API error (%d): %s", label, status, errBody)
	}
	return fmt.Errorf("%s API error (%d): %s", label, status, message)
}

// formatCompletionsHTTPError ports the error message pi's openai-completions
// provider surfaces for a non-2xx HTTP response — its catch:
// formatProviderError(normalizeProviderError(err)) with no prefix, so
// `${status}: ${body}` where formatResponsesHTTPError has its label, and the
// SDK's message as it is otherwise — and then its append of the APIError's
// error.metadata.raw (some OpenRouter upstreams put detail there): on a line of
// its own, as String() writes it, when it is truthy and the message does not
// already contain it (upstream 6fbeba51's guard).
func formatCompletionsHTTPError(status int, body []byte) error {
	e := newOpenAIStatusError(body)
	message := e.message(status)
	if errBody := e.body(); errBody != "" && !strings.Contains(message, errBody) {
		message = fmt.Sprintf("%d: %s", status, errBody)
	}
	raw := rawOptional(rawOptional(e.errJSON)["metadata"])["raw"]
	if !rawTruthy(raw) {
		return errors.New(message)
	}
	// An object with its own toString member has no String() (see
	// rawToString): pi's catch block throws inside itself there and its
	// stream never ends (D84); the port leaves raw out.
	if text, err := rawToString(raw); err == nil && !strings.Contains(message, text) {
		message += "\n" + text
	}
	return errors.New(message)
}

// openaiStatusError is the openai client's APIError for a non-2xx response
// (client.makeStatusError over APIError.generate, openai 7.19.0): error is its
// `error` as JSON text, nil for undefined, and text the message makeMessage
// falls back to when that is falsy.
//
// The client reads the body as fetch's text() (one leading BOM dropped,
// stripBOM) and parses it as JSON: a value that does not parse, or parses to
// a falsy one, leaves the raw text as the message (`errJSON ? undefined :
// errText`). openai 7 then takes an object or array whose own `error` is null
// or absent for the error itself (`{error: body}`); any other value's `error`
// member is the error — undefined for a string, a number or true.
type openaiStatusError struct {
	errJSON json.RawMessage
	text    string
}

func newOpenAIStatusError(body []byte) openaiStatusError {
	body = stripBOM(body)
	if !json.Valid(body) || !rawTruthy(body) {
		return openaiStatusError{text: string(body)}
	}
	switch jsonValueKind(body) {
	case '{':
		if member := rawOptional(body)["error"]; !rawNullish(member) {
			return openaiStatusError{errJSON: member}
		}
		return openaiStatusError{errJSON: body}
	case '[':
		return openaiStatusError{errJSON: body}
	}
	return openaiStatusError{}
}

// message is APIError.makeMessage's text for this error at status: the
// error's detail (openaiSDKErrorDetail), else the raw text, as
// `${status} ${msg}`, or `${status} status code (no body)` when both are
// empty. Nothing here is capped — pi's cap applies to the body half only.
func (e openaiStatusError) message(status int) string {
	msg := openaiSDKErrorDetail(e.errJSON)
	if msg == "" {
		msg = e.text
	}
	if msg == "" {
		return fmt.Sprintf("%d status code (no body)", status)
	}
	return fmt.Sprintf("%d %s", status, msg)
}

// body is the body half of pi's normalizeProviderError for this error
// (error-body.ts pickBodyText/extractBody): the APIError's `error` when it is a
// non-empty plain object, stringified the way JSON.stringify does and capped
// at maxProviderErrorBodyChars. Every other value — a string, an array, an
// empty object, null or undefined — yields "" (pi: undefined), and the SDK
// message stands.
func (e openaiStatusError) body() string {
	if len(rawOptional(e.errJSON)) == 0 {
		return ""
	}
	text, ok := jsStringify(e.errJSON)
	if !ok {
		return ""
	}
	return truncateErrorText(text, maxProviderErrorBodyChars)
}

// openaiSDKErrorMessage is the openai SDK's APIError message for a non-2xx
// response with body: the message pi's retryProviderRequest quotes when it
// refuses a server's retry delay.
func openaiSDKErrorMessage(status int, body []byte) string {
	return newOpenAIStatusError(body).message(status)
}

// openaiSDKErrorDetail is the text openai SDK APIError.makeMessage builds from
// its `error` argument — @anthropic-ai/sdk's makeMessage is byte-identical —
// given as JSON (nil when absent): error.message when it
// is a truthy string, JSON.stringify(error.message) when it is truthy
// otherwise, else JSON.stringify(error) — or "" when error is falsy, where
// makeMessage falls back to its status and message arguments.
func openaiSDKErrorDetail(errorValue json.RawMessage) string {
	if !rawTruthy(errorValue) {
		return ""
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(errorValue, &members) == nil {
		if m, has := members["message"]; has && rawTruthy(m) {
			var str string
			if json.Unmarshal(m, &str) == nil {
				return str
			}
			if j, ok := jsStringify(m); ok {
				return j
			}
		}
	}
	j, _ := jsStringify(errorValue)
	return j
}

// stripBOM is the one piece of fetch's text() decode that the openai
// functions above see: one leading UTF-8 BOM is dropped before the client ever
// parses, on the terminal path and on the retry fail-fast quote alike. The rest
// of that decode — invalid UTF-8 becoming U+FFFD per maximal subpart — is
// ported for anthropic only (anthropicSDKErrorMessage); the openai paths pass
// the raw bytes through (docs/UPSTREAM.md K18).
func stripBOM(body []byte) []byte {
	return bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))
}

// rawTruthy reports JavaScript truthiness for a well-formed JSON value without
// decoding it: null, false, 0 and "" are falsy; everything else — objects and
// arrays even when empty, and any non-zero number including the ones JSON.parse
// overflows to Infinity, which encoding/json cannot decode — is truthy.
func rawTruthy(raw json.RawMessage) bool {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 {
		return false
	}
	switch v[0] {
	case 'n', 'f':
		return false
	case 't', '{', '[':
		return true
	case '"':
		return len(v) > 2
	}
	f, err := strconv.ParseFloat(string(v), 64)
	if err != nil {
		return errors.Is(err, strconv.ErrRange) // overflow: JS Infinity, truthy
	}
	return f != 0
}

// anthropicSDKErrorMessage is the Anthropic SDK's APIError message for a
// non-2xx response, which pi surfaces as is: on the terminal path as the
// stream's errorMessage, and quoted by retryProviderRequest's fail-fast
// refusal. Nothing caps it.
//
// The SDK reads the body as fetch's text() decodes it (one leading BOM dropped,
// invalid UTF-8 as U+FFFD per maximal subpart), parses it with safeJSON, and
// hands makeMessage the WHOLE parsed body as `error` — openai passes its
// `error` member — with the raw text as the fallback message when the parse
// failed or made a falsy value (`errJSON ? undefined : errText`). makeMessage
// is byte-identical in both SDKs (openaiSDKErrorDetail). So a conformant
// anthropic body — {"type":"error","error":{...}}, with no top-level `message`
// — reads `${status} {"type":"error",...}`, not the nested message.
func anthropicSDKErrorMessage(status int, body []byte) string {
	text := jstext.DecodeUTF8(stripBOM(body))
	var msg string
	if raw := json.RawMessage(text); json.Valid(raw) && rawTruthy(raw) {
		msg = openaiSDKErrorDetail(raw)
	} else {
		msg = text
	}
	if msg == "" {
		return fmt.Sprintf("%d status code (no body)", status)
	}
	return fmt.Sprintf("%d %s", status, msg)
}
