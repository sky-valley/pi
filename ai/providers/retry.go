package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

const (
	// defaultMaxRetryDelayMs is the ceiling on a server-requested Retry-After
	// delay. Exceeding it fails the request outright (pi's
	// validateServerRetryDelayMs); set MaxRetryDelayMs to 0 to disable.
	defaultMaxRetryDelayMs = 60_000
	defaultTimeoutMs       = 600_000 // 10 minutes (matches the OpenAI/Anthropic SDK default)
	retryBaseDelayMs       = 500     // openai SDK initialRetryDelay = 0.5s
	retryBackoffCapMs      = 8_000   // openai SDK maxRetryDelay = 8s
)

// retryConfig captures the retry/timeout knobs resolved from StreamOptions.
type retryConfig struct {
	maxRetries      int
	maxRetryDelayMs int
	timeoutMs       int
	// providerError renders the SDK APIError message that pi interpolates into
	// the fail-fast error, and selects whether fail-fast applies at all. It is
	// set for the providers whose SDK error carries the response headers
	// (anthropic-messages, openai-completions, openai-responses): pi's
	// retryProviderRequest reads a server-requested delay from them and fails
	// fast on one above maxRetryDelayMs. It is nil for google: pi runs google
	// through retryProviderRequest too (retryGoogleRequest), but
	// @google/genai's ApiError carries no headers, so pi never reads a server
	// delay there and never fails fast.
	providerError func(status int, body []byte) string
	// httpClient overrides the shared client (pi StreamOptions.fetch). Nil keeps
	// sharedClient, whose transport carries the timeoutMs response-header cap.
	httpClient ai.HTTPDoer
}

// retryFromOptions mirrors pi's `maxRetries: options?.maxRetries ?? 0` passed
// to the SDKs: an unset/zero MaxRetries means ZERO retries (single attempt).
// providerError is the caller's SDK-message renderer; see retryConfig.
func retryFromOptions(o ai.StreamOptions, providerError func(status int, body []byte) string) retryConfig {
	cfg := retryConfig{
		maxRetries:      o.MaxRetries,
		maxRetryDelayMs: defaultMaxRetryDelayMs,
		timeoutMs:       o.TimeoutMs,
		providerError:   providerError,
	}
	if c, ok := customHTTPClient(o.HTTPClient); ok {
		cfg.httpClient = c
	}
	if cfg.maxRetries < 0 {
		cfg.maxRetries = 0
	}
	if o.MaxRetryDelayMs != nil {
		cfg.maxRetryDelayMs = *o.MaxRetryDelayMs
	}
	if cfg.timeoutMs <= 0 {
		cfg.timeoutMs = defaultTimeoutMs
	}
	return cfg
}

// customHTTPClient reports whether opts carries an HTTP client that actually
// overrides the provider default, and returns it. pi blesses
// `fetch === globalThis.fetch` as equivalent to unset, so http.DefaultClient —
// the Go stand-in for that default — must mean "unset" everywhere, not just in
// the google adapter's guard.
func customHTTPClient(c ai.HTTPDoer) (ai.HTTPDoer, bool) {
	if c == nil || c == ai.HTTPDoer(http.DefaultClient) {
		return nil, false
	}
	return c, true
}

// clientCache memoizes http.Clients keyed by response-header timeout so we reuse
// connection pools across requests.
var (
	clientMu    sync.Mutex
	clientCache = map[int]*http.Client{}
)

// sharedClient returns an http.Client whose transport caps the time to first
// response byte at timeoutMs (ResponseHeaderTimeout). It deliberately leaves the
// streaming body read uncapped so long SSE responses are not severed.
func sharedClient(timeoutMs int) *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c, ok := clientCache[timeoutMs]; ok {
		return c
	}
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		// http.DefaultTransport was replaced with a non-*http.Transport (e.g.
		// by instrumentation); fall back to a fresh transport mirroring Go's
		// defaults instead of dereferencing a nil from the failed assertion.
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	tr.ResponseHeaderTimeout = time.Duration(timeoutMs) * time.Millisecond
	c := &http.Client{Transport: tr}
	clientCache[timeoutMs] = c
	return c
}

// shouldRetryResponse implements the openai SDK retry matrix (which pi
// delegates to): only non-2xx responses are considered; an explicit
// `x-should-retry` header overrides the status logic; otherwise 408, 409,
// 429, and all >=500 statuses are retryable.
func shouldRetryResponse(resp *http.Response) bool {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false
	}
	switch resp.Header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, // 408
		http.StatusConflict,        // 409
		http.StatusTooManyRequests: // 429
		return true
	}
	return resp.StatusCode >= 500
}

// parseFloatPrefix mirrors JavaScript's Number.parseFloat, which pi uses to read
// the Retry-After headers: leading whitespace is skipped, the longest valid
// numeric prefix is consumed, and trailing junk is ignored (so "3600s" parses as
// 3600). ok=false stands for JS NaN — no numeric prefix at all. A prefix that
// overflows float64 yields ±Inf, matching JS ("1e400" is Infinity, not NaN);
// the caller clamps it before building a Duration. The "Infinity" literal is
// accepted too, because parseFloat does accept it — case-sensitively and as a
// prefix, so "Infinityx" is Infinity while "Inf" and "infinity" are NaN.
func parseFloatPrefix(s string) (float64, bool) {
	// parseFloat skips StrWhiteSpaceChar, the set String.prototype.trim
	// removes: U+FEFF is in it and U+0085 is not, unlike unicode.IsSpace.
	s = jstext.TrimStart(s)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	// parseFloat accepts the Infinity literal, exact-case, as a prefix.
	if strings.HasPrefix(s[i:], "Infinity") {
		if s[0] == '-' {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	digits := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			digits++
		}
	}
	if digits == 0 {
		return 0, false
	}
	end := i
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k > j {
			end = k
		}
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	// An out-of-range prefix is ±Inf in JS, not NaN, so only a genuine syntax
	// error counts as "no numeric prefix".
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// serverRetryDelayMs extracts a server-requested retry delay in milliseconds,
// mirroring pi's getRetryDelayMs header handling. `retry-after-ms` wins when it
// reads as a finite number; otherwise `Retry-After` is read as seconds, falling
// back to an HTTP date. A header that yields no finite delay — unparseable,
// Infinity, or a value that overflows — dictates nothing, so the caller falls
// back to the computed backoff (pi's Number.isFinite guards, upstream
// 2bbfcca43). ok=false means no header dictated the delay.
func serverRetryDelayMs(resp *http.Response) (float64, bool) {
	if resp == nil {
		return 0, false
	}
	if v := resp.Header.Get("retry-after-ms"); v != "" {
		if ms, ok := parseFloatPrefix(jstext.IsomorphicDecode(v)); ok && isFinite(ms) {
			return ms, true
		}
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, ok := parseFloatPrefix(jstext.IsomorphicDecode(ra)); ok {
			// seconds * 1000 can overflow a finite reading to Infinity.
			if ms := secs * 1000; isFinite(ms) {
				return ms, true
			}
		} else if t, err := http.ParseTime(ra); err == nil {
			return float64(time.Until(t).Milliseconds()), true
		}
	}
	return 0, false
}

func isFinite(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }

// validateServerRetryDelay ports pi's validateServerRetryDelayMs: a
// server-requested delay above maxRetryDelayMs fails the request immediately
// instead of being clamped or ignored, so the visible agent-level retry policy
// handles it. maxRetryDelayMs <= 0 disables the limit.
//
// providerMsg is the already-rendered SDK APIError message, matching the
// `providerErrorMessage: string` parameter pi passes.
//
// errServerRetryDelayTooLong wraps the result so the agent-level retry policy
// this defers to can recognize the condition; pi throws a plain Error, but
// string-matching is not an API.
func validateServerRetryDelay(delayMs float64, maxRetryDelayMs int, providerMsg string) error {
	if maxRetryDelayMs <= 0 || delayMs <= float64(maxRetryDelayMs) {
		return nil
	}
	// The message is pi's, byte-for-byte, capitalization included. Do not
	// "fix" it — tests byte-compare it against the TS template literal.
	return &serverRetryDelayError{msg: fmt.Sprintf(
		"Server requested %ss retry delay (max: %ss). %s",
		ceilSeconds(delayMs),
		ceilSeconds(float64(maxRetryDelayMs)),
		providerMsg)}
}

// errServerRetryDelayTooLong marks a request abandoned because the server asked
// to be retried later than maxRetryDelayMs allows. pi throws a plain Error here;
// a sentinel lets the agent-level retry policy this defers to recognize the
// condition without string-matching, while Error() stays byte-identical to pi.
var errServerRetryDelayTooLong = errors.New("server retry delay exceeds limit")

type serverRetryDelayError struct{ msg string }

func (e *serverRetryDelayError) Error() string { return e.msg }

func (e *serverRetryDelayError) Is(target error) bool { return target == errServerRetryDelayTooLong }

// ceilSeconds renders the numeric part of pi's `${Math.ceil(ms / 1000)}s`,
// including JS's "Infinity" spelling for a header value that overflowed float64.
func ceilSeconds(ms float64) string {
	secs := math.Ceil(ms / 1000)
	if math.IsInf(secs, 1) {
		return "Infinity"
	}
	return strconv.FormatFloat(secs, 'f', -1, 64)
}

// maxServerDelayMs is the largest millisecond delay representable as a Duration.
const maxServerDelayMs = float64(math.MaxInt64 / int64(time.Millisecond))

// serverDelayDuration converts a validated server-requested delay. Negative
// values (a Retry-After date in the past) retry immediately, matching pi's
// `Math.max(0, ms)`, and the upper clamp keeps an absurd delay from wrapping
// int64 nanoseconds into a negative Duration.
func serverDelayDuration(ms float64) time.Duration {
	switch {
	case ms < 0:
		ms = 0
	case ms > maxServerDelayMs:
		ms = maxServerDelayMs
	}
	// JS setTimeout truncates its delay to an integer millisecond count.
	return time.Duration(ms) * time.Millisecond
}

// backoffDelay is pi's computed fallback: min(0.5s * 2^attempt, 8s) with up to
// 25% downward jitter. maxRetryDelayMs bounds only a server-requested delay,
// never this.
func backoffDelay(attempt int) time.Duration {
	backoff := math.Min(float64(retryBaseDelayMs)*math.Pow(2, float64(attempt)), retryBackoffCapMs)
	jitter := 1 - rand.Float64()*0.25
	return time.Duration(backoff*jitter) * time.Millisecond
}

// retryDelay computes the wait before the next attempt, mirroring pi's
// getRetryDelayMs: a server-dictated delay wins once it passes
// validateServerRetryDelay, otherwise the computed backoff applies.
func retryDelay(resp *http.Response, attempt int, cfg retryConfig, providerMsg string) (time.Duration, error) {
	if ms, ok := serverRetryDelayMs(resp); ok && cfg.providerError != nil {
		if err := validateServerRetryDelay(ms, cfg.maxRetryDelayMs, providerMsg); err != nil {
			return 0, err
		}
		return serverDelayDuration(ms), nil
	} else if ok {
		// Google's branch, the one caller without a providerError. pi wraps
		// google in retryProviderRequest too (retryGoogleRequest), but
		// @google/genai's ApiError carries no headers, so pi never reads the
		// server's delay and never fails on an oversized one. The port has the
		// headers and honors what fits, falling through to the backoff
		// otherwise (ledger D8).
		if ms >= 0 && (cfg.maxRetryDelayMs <= 0 || ms <= float64(cfg.maxRetryDelayMs)) {
			return serverDelayDuration(ms), nil
		}
	}
	return backoffDelay(attempt), nil
}

// errRequestAborted is pi's createAbortError() (utils/provider-retry.ts):
// retryProviderRequest throws Error("Request aborted") for a request whose
// signal has aborted by the time it fails, and for an abort during its retry
// wait (abortableSleep).
var errRequestAborted = errors.New("Request aborted")

// errRequestWasAborted is the Error("Request was aborted") pi's adapters
// throw from their own checks of the signal — anthropic's before each body
// read, and each SDK adapter's once its stream has ended — and the error
// message faux gives an aborted stream. It is not errRequestAborted, the
// retry loop's.
var errRequestWasAborted = errors.New("Request was aborted")

// readSDKErrorBody reads the body of the non-2xx response anthropic or an
// openai loop fails with, as their SDKs do (sdkErrorText). The SDKs throw
// their error only once they have read that body, and pi's
// retryProviderRequest catches it and checks the signal first thing, so a
// request aborted by then — during the read included — ends
// errRequestAborted, whatever the body said.
func readSDKErrorBody(ctx context.Context, body io.Reader) ([]byte, error) {
	data := sdkErrorText(body)
	if ctx != nil && ctx.Err() != nil {
		return nil, errRequestAborted
	}
	return data, nil
}

// sdkErrorText is the text openai and @anthropic-ai/sdk parse a non-2xx
// response's body from: `await response.text().catch((err) =>
// castToError(err).message)`. A read that fails leaves its error's message —
// undici's "terminated" when the connection drops mid-body (fetchBody) — in
// place of what had arrived.
func sdkErrorText(body io.Reader) []byte {
	data, err := io.ReadAll(body)
	if err != nil {
		// context.Canceled from a custom client's body stands for the
		// AbortError a custom fetch's body throws, whose message is undici's.
		if errors.Is(err, context.Canceled) {
			err = errOperationAborted
		}
		return []byte(err.Error())
	}
	return data
}

// readFetchErrorBody is readSDKErrorBody for @google/genai, which reads a
// non-2xx response's body with no catch: a read that fails rejects, and its
// error is the stream's.
func readFetchErrorBody(ctx context.Context, body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(body)
	if ctx != nil && ctx.Err() != nil {
		return nil, errRequestAborted
	}
	return data, err
}

// undiciFetchError is what fetch rejects with when a request sent through
// the port's own client gets no response: undici's TypeError "fetch failed",
// whatever the transport failure was — a refused, dropped or unreachable
// connection, a failed TLS handshake, a malformed response, a header value
// its client refuses, or undici's own connect and headers timeouts. client.Do
// reports every such failure as a *url.Error whose Op is the method; any
// other error is returned as it is.
func undiciFetchError(err error) error {
	var sent *url.Error
	if errors.As(err, &sent) && sent.Op == "Post" {
		return errors.New("fetch failed")
	}
	return err
}

// undiciHeadersTimeoutMs bounds the wait for a response's headers on the
// port's own client, which stands for undici's fetch: undici's headersTimeout,
// 300 seconds, which fetch applies whatever the caller's options say. pi's CLI
// installs the same value as its httpIdleTimeoutMs default. Where pi's request
// is a bare fetch (google through @google/genai, pi-messages) its expiry is one
// more "fetch failed" (undiciFetchError); an SDK adapter's own timeoutMs timer
// can only cut the wait shorter, and wins a tie, having started first
// (sendWithRetry). A variable only so a test can shorten it.
var undiciHeadersTimeoutMs = 300_000

// fetchRejection is a send that got no response: the error the port's own
// client, or a custom HTTPClient (pi's custom fetch), rejected the request
// with, or undici's refusal of it (fetchRefusal). sendWithRetry keeps it
// apart from an error building the request, which pi's SDKs throw as it is,
// where they wrap a rejected fetch in an error of their own (sdkFetchError).
type fetchRejection struct {
	err    error
	custom bool
	// headersTimeout is set when the port's own client gave up waiting for
	// the headers of a request it had written at undiciHeadersTimeoutMs,
	// before any timeoutMs of the SDK's: undici's UND_ERR_HEADERS_TIMEOUT.
	headersTimeout bool
}

func (e *fetchRejection) Error() string { return e.err.Error() }

func (e *fetchRejection) Unwrap() error { return e.err }

// errAPIConnection and errAPIConnectionTimeout are the Anthropic and OpenAI
// SDKs' APIConnectionError and APIConnectionTimeoutError, which they throw
// when their fetch rejects.
var (
	errAPIConnection        = errors.New("Connection error.")
	errAPIConnectionTimeout = errors.New("Request timed out.")
)

// sdkTimedOut is the SDKs' test of a rejection's text.
var sdkTimedOut = regexp.MustCompile(`(?i)timed? ?out`)

// errOpenAIHeadersTimeout is openai 7.19.0's APIConnectionTimeoutError for a
// fetch that failed on undici's headersTimeout (UND_ERR_HEADERS_TIMEOUT).
var errOpenAIHeadersTimeout = errors.New("Request timed out. Node.js fetch timed out waiting for response headers; configure a matching undici fetch and fetchOptions.dispatcher with an Agent whose headersTimeout is at least the SDK timeout.")

// openaiFetchError is sdkFetchError for the two openai loops: openai 7.19.0
// words a rejection whose cause is undici's headers timeout apart
// (errOpenAIHeadersTimeout). @anthropic-ai/sdk 0.124.0 does not, so
// anthropic's is "Request timed out." like any other timeout.
func openaiFetchError(err error) error {
	var rejected *fetchRejection
	if errors.As(err, &rejected) && rejected.headersTimeout {
		return errOpenAIHeadersTimeout
	}
	return sdkFetchError(err)
}

// sdkFetchError is the error an SDK adapter (anthropic, both openai loops)
// fails with for err from sendWithRetry. The SDKs throw a rejected fetch as
// their APIConnectionError, or as their APIConnectionTimeoutError when it
// looks like a timeout: an AbortError the caller's signal did not raise (the
// SDK's own timeoutMs timer raises one; in Go, a context error), or an error
// whose text says it timed out. retryProviderRequest retries either and
// throws the last. The port's
// own client's transport failures are classed by net/http's Timeout, which
// holds for what undici calls a timeout — its connect and headers timeouts,
// an OS ETIMEDOUT — and for timeoutMs's ResponseHeaderTimeout. The text of
// undici's refusal is undici's own, and a custom client's error is its own,
// so for those the SDKs' test reads the text. Any other error — an abort, a
// request that could not be built, a server's refused retry delay — is pi's
// and passes through.
func sdkFetchError(err error) error {
	var rejected *fetchRejection
	if !errors.As(err, &rejected) {
		return err
	}
	if rejected.timedOut() {
		return errAPIConnectionTimeout
	}
	return errAPIConnection
}

func (e *fetchRejection) timedOut() bool {
	var refused *fetchCredentialsError
	if !e.custom && !errors.As(e.err, &refused) {
		var sent *url.Error
		return errors.As(e.err, &sent) && sent.Timeout()
	}
	return errors.Is(e.err, context.Canceled) || errors.Is(e.err, context.DeadlineExceeded) ||
		sdkTimedOut.MatchString(e.err.Error())
}

// errTerminated is the message of the TypeError undici's fetch errors a
// response body's stream with when its connection fails mid-body — dropped,
// reset, or closed short of the length or chunks the head promised.
var errTerminated = errors.New("terminated")

// fetchBody is a response body read through the port's own client, the
// stand-in for undici's fetch, which errors a body's stream when its signal
// aborts: a read that starts once ctx is done, or that fails once it is,
// rejects with undici's AbortError (errOperationAborted) — a chunk that had
// already arrived is never read — and any other read that fails, however
// net/http words the failure, fails as undici's does, errTerminated. The
// body's end is a read of its own, as a stream reader's done is: undici
// reaches it only when a read asks, so an abort before that read rejects it
// even when the whole body has arrived. A read that returns the last bytes
// and io.EOF together — net/http's does, when the end is already buffered —
// returns the bytes alone; the next read, which io.Reader's contract has
// return 0 and io.EOF, reports the end. An adapter wraps only its own
// client's bodies: a custom HTTPClient is pi's custom fetch, whose body is
// read as it is, abort or no abort.
type fetchBody struct {
	ctx context.Context
	r   io.Reader
}

func (b fetchBody) Read(p []byte) (int, error) {
	aborted := func() bool { return b.ctx != nil && b.ctx.Err() != nil }
	if aborted() {
		return 0, errOperationAborted
	}
	n, err := b.r.Read(p)
	switch {
	case err == io.EOF && n > 0:
		err = nil
	case err != nil && err != io.EOF:
		err = errTerminated
		if aborted() {
			err = errOperationAborted
		}
	}
	return n, err
}

// errOperationAborted is the message of undici's AbortError, which a fetch
// rejects with when its signal aborts before the response arrives, and which
// a body read that the abort cuts short, or that starts after it, rejects
// with (fetch errors the body's stream on abort).
var errOperationAborted = errors.New("This operation was aborted")

// nullBodyStatus reports whether a response with this status reaches pi with
// a null body whatever the server sent: 204 and 205, the null body statuses
// (101, 103, 204, 205, 304) a 2xx response can have. undici neither reads nor
// decodes such a body, and the Response constructor refuses one, so a custom
// fetch's is null too. net/http reads a 205's body like any other.
func nullBodyStatus(status int) bool {
	return status == http.StatusNoContent || status == http.StatusResetContent
}

// nullResponseBody reports whether pi finds resp's body null: at a status
// fetch gives a null body (nullBodyStatus), and for a custom HTTPClient's nil
// Body, which stands for a custom fetch's null body at any status. It makes a
// nil Body http.NoBody, so an error response's reads as a null one does, as
// "". http.NoBody itself is not null: net/http gives it to a 200 whose
// Content-Length is 0, where fetch's body is empty.
func nullResponseBody(resp *http.Response) bool {
	if resp.Body == nil {
		resp.Body = http.NoBody
		return true
	}
	return nullBodyStatus(resp.StatusCode)
}

// sendWithRetry issues the request built by build, retrying transient network
// errors (like 5xx) and retryable HTTP statuses with backoff. build must
// produce a fresh *http.Request on each call (request bodies are single-use).
// With cfg.maxRetries == 0 (pi's default) exactly one attempt is made.
//
// It is pi's retryProviderRequest, which every adapter sending through it
// wraps its SDK request in: once ctx is done, a request that fails — no
// response, or a non-2xx one, which the SDKs throw — and a retry wait both
// end with errRequestAborted. A send that gets no response otherwise fails
// with a *fetchRejection holding its client's error.
//
// For providers whose SDK error carries the response headers
// (cfg.providerError non-nil) a server-requested delay above
// cfg.maxRetryDelayMs terminates the loop with the fail-fast error from
// validateServerRetryDelay.
//
// The port's own client waits for a response's headers until the first of
// cfg.timeoutMs (an SDK's own timer, which starts first and so wins a tie) and
// undici's headersTimeout (undiciHeadersTimeoutMs); a rejection marks which
// one it was (fetchRejection.headersTimeout). Only a wait once the request is
// written is the headers timeout: a connection or TLS handshake that times out
// is undici's connect timeout.
func sendWithRetry(ctx context.Context, build func() (*http.Request, error), cfg retryConfig) (*http.Response, error) {
	client := cfg.httpClient
	undiciTimesOutFirst := false
	if client == nil {
		client = sharedClient(min(cfg.timeoutMs, undiciHeadersTimeoutMs))
		undiciTimesOutFirst = undiciHeadersTimeoutMs < cfg.timeoutMs
	}
	attempts := cfg.maxRetries + 1
	var lastErr error
	aborted := func() bool { return ctx != nil && ctx.Err() != nil }

	for attempt := 0; attempt < attempts; attempt++ {
		if aborted() {
			return nil, errRequestAborted
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		// The port's own client stands for undici's fetch, which refuses some
		// requests before sending anything; the SDK sees that as any other
		// rejected fetch.
		var resp *http.Response
		var wrote atomic.Bool
		if cfg.httpClient == nil {
			err = fetchRefusal(req)
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				// net/http can retry a request on a fresh connection when the
				// write on a reused one fails, so each attempt starts unwritten.
				GetConn:      func(string) { wrote.Store(false) },
				WroteRequest: func(info httptrace.WroteRequestInfo) { wrote.Store(info.Err == nil) },
			}))
		}
		if err == nil {
			resp, err = client.Do(req)
		}
		if err != nil {
			if aborted() {
				return nil, errRequestAborted
			}
			var sent *url.Error
			headersTimeout := undiciTimesOutFirst && wrote.Load() && errors.As(err, &sent) && sent.Timeout()
			err = &fetchRejection{err: err, custom: cfg.httpClient != nil, headersTimeout: headersTimeout}
			lastErr = err
			if attempt == attempts-1 {
				return nil, err
			}
			// No response, so no server-requested delay: pure backoff.
			if !sleepCtx(ctx, backoffDelay(attempt)) {
				return nil, errRequestAborted
			}
			continue
		}
		if (resp.StatusCode < 200 || resp.StatusCode >= 300) && aborted() {
			readAndCloseBody(resp, sdkResponseBody(ctx, resp, cfg.httpClient))
			return nil, errRequestAborted
		}
		if shouldRetryResponse(resp) && attempt < attempts-1 {
			// The body is only needed to quote the provider in a fail-fast
			// error, so render it lazily.
			var providerMsg string
			body := readAndCloseBody(resp, sdkResponseBody(ctx, resp, cfg.httpClient))
			// The SDK throws once it has read the body, and pi's catch
			// checks the signal before it reads a retry delay.
			if aborted() {
				return nil, errRequestAborted
			}
			if cfg.providerError != nil {
				providerMsg = cfg.providerError(resp.StatusCode, body)
			}
			delay, err := retryDelay(resp, attempt, cfg, providerMsg)
			if err != nil {
				return nil, err
			}
			if !sleepCtx(ctx, delay) {
				return nil, errRequestAborted
			}
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("request failed after %d attempts", attempts)
}

// readAndCloseBody consumes and closes a retryable response's body, read
// through body (sdkResponseBody), returning the text its SDK parses
// (sdkErrorText) so a fail-fast error can quote the provider's message. The
// 1 MiB cap matches the volume the previous discard-only drain read, so
// connection reuse is unchanged.
func readAndCloseBody(resp *http.Response, body io.Reader) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	text := sdkErrorText(io.LimitReader(body, 1<<20))
	_ = resp.Body.Close()
	return text
}

// sdkResponseBody is resp's body as an SDK adapter reads it. The SDKs fetch
// with undici unless the caller hands pi its own fetch, so a body the port's
// own client fetched fails as undici's does (fetchBody): "terminated" for a
// connection that drops mid-body.
func sdkResponseBody(ctx context.Context, resp *http.Response, httpClient ai.HTTPDoer) io.Reader {
	if _, custom := customHTTPClient(httpClient); custom {
		return resp.Body
	}
	return fetchBody{ctx, resp.Body}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
