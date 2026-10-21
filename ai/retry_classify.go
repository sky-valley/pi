package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// providerErrorPattern is pi's buildProviderErrorPattern result,
// new RegExp(patterns.join("|"), "i"), matched with that RegExp's semantics
// rather than RE2's. A JavaScript RegExp without the "u" flag differs from RE2
// in three ways these patterns can observe:
//
//   - "i" compares the toUpperCase of each character but never lets a
//     non-ASCII character canonicalize to an ASCII one, so against all-ASCII
//     patterns it folds ASCII letters only. RE2's (?i) folds by Unicode, under
//     which U+212A KELVIN SIGN matches "k" and U+017F LATIN SMALL LETTER LONG S
//     matches "s". The alternatives are therefore compiled lowercased, without
//     (?i), and matched against the input with only A-Z lowercased.
//   - "." matches one UTF-16 code unit other than a line terminator (\n, \r,
//     U+2028, U+2029), where RE2's matches one code point other than \n. Every
//     "." in these lists sits between ASCII literals. A surrogate always has
//     its partner on one side, so it never sits between two ASCII characters,
//     and the code units such a "." can match are exactly the BMP code points:
//     an astral character is excluded, not counted as two. Go strings cannot
//     hold a lone surrogate; an invalid UTF-8 byte decodes as one U+FFFD,
//     which "." matches in both.
//   - \S matches one UTF-16 code unit that is not JavaScript whitespace (which
//     takes in \v and the Unicode space separators), where RE2's matches any
//     code point but the ASCII \t\n\f\r and space. The only \S here is \S+
//     between spaces, and a run of non-whitespace code units that holds one
//     surrogate of a pair holds its partner, so it is a run of code points
//     outside JavaScript's whitespace.
type providerErrorPattern struct {
	source string // RegExp.prototype.source: the alternatives joined with "|", "/" escaped
	re     *regexp.Regexp
}

// jsDot is "." of a JavaScript RegExp without the "u" flag, as the RE2 class
// matching the same text where every "." is flanked by ASCII literals.
const jsDot = `[^\n\r\x{2028}\x{2029}\x{10000}-\x{10FFFF}]`

// jsNonSpace is \S of a JavaScript RegExp: everything but the WhiteSpace and
// LineTerminator code points.
const jsNonSpace = `[^\t\n\v\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]`

func buildProviderErrorPattern(patterns []string) providerErrorPattern {
	alternatives := make([]string, len(patterns))
	for i, pattern := range patterns {
		alternatives[i] = translateProviderErrorPattern(pattern)
	}
	return providerErrorPattern{
		// EscapeRegExpPattern writes "/" as "\/" so the source reads as a literal.
		source: strings.ReplaceAll(strings.Join(patterns, "|"), "/", `\/`),
		re:     regexp.MustCompile(strings.Join(alternatives, "|")),
	}
}

// translateProviderErrorPattern rewrites one alternative into RE2 syntax with
// the JavaScript semantics providerErrorPattern describes. It knows literals,
// ".", "?", "+", non-capturing groups and \S+, and panics on anything else:
// lowercasing would corrupt other escapes and classes, and RE2 and JavaScript
// disagree on several of them.
func translateProviderErrorPattern(pattern string) string {
	unsupported := func(what string) {
		panic(fmt.Sprintf("ai: provider error pattern %q uses %s; "+
			"teach translateProviderErrorPattern its JavaScript semantics before adding it", pattern, what))
	}
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '.':
			b.WriteString(jsDot)
		case '\\':
			if !strings.HasPrefix(pattern[i:], `\S+`) {
				unsupported("an escape other than \\S+")
			}
			b.WriteString(jsNonSpace)
			i++ // the "+" follows as a quantifier
		case '(':
			if !strings.HasPrefix(pattern[i:], "(?:") {
				unsupported("a group other than (?:...)")
			}
			b.WriteString("(?:")
			i += 2
		case ')', '?', '+':
			b.WriteByte(c)
		case '^', '$', '|', '*', '[', ']', '{', '}':
			unsupported(fmt.Sprintf("%q", c))
		default:
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// MatchString is RegExp.prototype.test on s.
func (p providerErrorPattern) MatchString(s string) bool {
	return p.re.MatchString(asciiLower(s))
}

// asciiLower lowercases A-Z and leaves every other byte as it is, so UTF-8
// sequences pass through untouched.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// nonRetryableProviderLimitErrorPattern matches provider error text that
// indicates a subscription/account/billing limit rather than a transient
// failure. Matches here suppress retries.
var nonRetryableProviderLimitErrorPattern = buildProviderErrorPattern([]string{
	// OpenCode Go/free-tier limits returned as 429 JSON error types by OpenCode's
	// Zen API. These are subscription/account limits, not transient throttles.
	"GoUsageLimitError",
	"FreeUsageLimitError",

	// OpenCode Go subscription-limit text asks users to enable available-balance
	// usage after rolling/weekly/monthly limits are reached.
	"Monthly usage limit reached",
	"available balance",

	// Generic quota/budget/billing exhaustion. `insufficient_quota` is OpenAI's
	// quota/billing error code; the other strings cover common gateway wording.
	"insufficient_quota",
	"out of budget",
	"quota exceeded",
	"billing",

	// Sign in with ChatGPT: the subscription's shared usage limit, which resets
	// after hours rather than seconds.
	"subscription_sharing_usage_limit_exceeded",
	// Sign in with ChatGPT on a ChatGPT Business plan: a usage limit set by the
	// workspace admin. Only the admin can raise it.
	"subscription_sharing_workplace_individual_usage_limit_exceeded",
})

// retryableProviderErrorPattern matches provider/transport error text that
// looks like a transient failure worth retrying.
var retryableProviderErrorPattern = buildProviderErrorPattern([]string{
	// Generic provider load, HTTP status, and server-side transient failures.
	"overloaded",
	"server_busy",
	"servers are currently busy",
	// Azure OpenAI's peak-load capacity refusal (#9669): a transient capacity
	// message, not an account limit.
	"currently experiencing high demand",
	"model is at capacity",
	"rate.?limit",
	"too many requests",
	"429",
	"500",
	"502",
	"503",
	"504",
	"520", // Cloudflare unknown-error status (#9627)
	"524", // Cloudflare origin-timeout status (#6239)
	// Also matches "The service is temporarily unavailable" (#10965).
	"service.?(?:is.?)?(?:temporarily.?)?unavailable",
	"server.?error",
	"internal.?error",

	// Wrapper/provider text for transient upstream failures, including OpenRouter
	// "Provider returned error" responses (#2264) and gateway wording such as
	// "The upstream provider returned an error" (#10842).
	"provider.?returned.?(?:an.?)?error",
	"exceeded request buffer limit while retrying upstream",
	// Gateway failure relaying the upstream response stream, sent as an OpenAI
	// Responses `response.failed` event without an error code (#10920).
	"unable to forward upstream",
	// Other gateway upstream failures relayed as `response.failed` error codes (#10920).
	"upstream.?request.?failed",
	"upstream_stream_read_error",
	"stream was interrupted",
	"http/?2 stream failed",

	// Network, proxy, and fetch transport failures. This includes OpenAI Codex
	// raw-fetch failures such as "upstream connect", "connection refused", and
	// "reset before headers" (#733), plus OpenRouter connection drops (#3317).
	"network.?error",
	"connection.?error",
	"connection.?refused",
	"connection.?lost",
	"other side closed",
	"fetch failed",
	// node-fetch system errors, e.g. the Google ADC OAuth token refresh used by
	// google-vertex. The reason is often empty (#10996).
	`request to \S+ failed, reason:`,
	// DNS resolution failures surface as node/libuv transport errors (#6904).
	"getaddrinfo",
	"ENOTFOUND",
	"EAI_AGAIN",
	"upstream.?connect",
	"reset before headers",
	"socket hang up",
	"socket connection was closed",
	"timed? out",
	"timeout",
	"terminated",

	// WebSocket transports can report close/error text instead of HTTP/fetch text.
	"websocket.?closed",
	"websocket.?error",

	// Premature stream endings from SDKs and transports. Anthropic can throw
	// "stream ended without ..." and "Anthropic stream ended before message_stop"
	// (#4433); Bedrock/Smithy can throw an HTTP/2 no-response error (#3594).
	"ended without",
	"stream ended before message_stop",
	"stream ended before a terminal response event",
	"http2 request did not get a response",
	// Node ERR_HTTP2_STREAM_CANCEL: the HTTP/2 session died before the request was
	// sent, e.g. after the Bedrock SDK's 5-minute session timeout (#10379).
	"pending stream has been canceled",

	// Provider-requested retry delay cap failures should flow through the outer
	// retry policy so callers can surface/abort the backoff (#1123).
	"retry delay",

	// Explicit retry guidance emitted mid-stream by OpenAI Responses and Bedrock
	// stream exceptions (#6019).
	"you can retry your request",
	"try your request again",
	"please retry your request",

	// gRPC based providers (e.g. NVIDIA NIM) surface transient throttles as a
	// ResourceExhausted status (#6449).
	"ResourceExhausted",

	// Sign in with ChatGPT: usage or user data temporarily unavailable. Usage
	// failures can arrive mid-stream without an HTTP 503 in the message.
	"subscription_sharing_usage_unavailable",
	"subscription_sharing_user_unavailable",
})

// IsRetryableAssistantError classifies whether a failed assistant message looks
// like a transient provider or transport error, so callers can decide if the
// last assistant turn should be restarted.
//
// This does not implement retry policy. Callers should first handle context
// overflow separately, then apply their own retry budget, backoff, and reporting
// before restarting the assistant turn.
func IsRetryableAssistantError(m AssistantMessage) bool {
	if m.StopReason != StopError || m.ErrorMessage == "" {
		return false
	}
	if nonRetryableProviderLimitErrorPattern.MatchString(m.ErrorMessage) {
		return false
	}
	return retryableProviderErrorPattern.MatchString(m.ErrorMessage)
}
