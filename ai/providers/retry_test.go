package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// TestRetryDefaultIsZeroRetries locks pi's `maxRetries ?? 0`: with MaxRetries
// unset, exactly one attempt is made and the error surfaces immediately.
func TestRetryDefaultIsZeroRetries(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"unavailable"}`)
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	final := StreamOpenAICompletions(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("expected error with zero default retries, got %s", final.StopReason)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected exactly 1 attempt (default 0 retries), got %d", calls)
	}
}

func TestProviderRetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, openAISSE)
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	final := StreamOpenAICompletions(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: 2}}).Result()

	if final.StopReason == ai.StopError {
		t.Fatalf("expected success after retry, got error: %s", final.ErrorMessage)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts (1 retry), got %d", calls)
	}
}

func TestProviderStopsRetryingPastLimit(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("retry-after-ms", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"unavailable"}`)
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	final := StreamOpenAICompletions(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIOptions{StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: 1}}).Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("expected error after exhausting retries, got %s", final.StopReason)
	}
	// maxRetries=1 => 2 attempts total.
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

// TestRetry409Retried locks 409 Conflict into the retry matrix (openai SDK).
func TestRetry409Retried(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	build := func() (*http.Request, error) { return http.NewRequest("GET", server.URL, nil) }
	resp, err := sendWithRetry(context.Background(), build, retryConfig{maxRetries: 1, maxRetryDelayMs: defaultMaxRetryDelayMs, timeoutMs: defaultTimeoutMs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retrying 409, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

// TestRetryXShouldRetryTrueForcesRetry: x-should-retry: true makes an
// otherwise non-retryable status (404) retryable.
func TestRetryXShouldRetryTrueForcesRetry(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("x-should-retry", "true")
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	build := func() (*http.Request, error) { return http.NewRequest("GET", server.URL, nil) }
	resp, err := sendWithRetry(context.Background(), build, retryConfig{maxRetries: 1, maxRetryDelayMs: defaultMaxRetryDelayMs, timeoutMs: defaultTimeoutMs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected retry forced by x-should-retry:true, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

// TestRetryXShouldRetryFalseSuppressesRetry: x-should-retry: false makes an
// otherwise retryable status (429) non-retryable.
func TestRetryXShouldRetryFalseSuppressesRetry(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	build := func() (*http.Request, error) { return http.NewRequest("GET", server.URL, nil) }
	resp, err := sendWithRetry(context.Background(), build, retryConfig{maxRetries: 3, maxRetryDelayMs: defaultMaxRetryDelayMs, timeoutMs: defaultTimeoutMs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 surfaced, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 attempt (retry suppressed), got %d", calls)
	}
}

// TestRetryAfterMsPreferredOverRetryAfter: retry-after-ms wins when both
// headers are present.
func TestRetryAfterMsPreferredOverRetryAfter(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("retry-after-ms", "5")
	resp.Header.Set("Retry-After", "30") // 30s — would dominate if used

	d := retryDelay(resp, 0, defaultMaxRetryDelayMs)
	if d != 5*time.Millisecond {
		t.Fatalf("expected 5ms from retry-after-ms, got %v", d)
	}
}

// TestRetryAfterSecondsParsed: a plain-seconds Retry-After is honored.
func TestRetryAfterSecondsParsed(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "2")
	d := retryDelay(resp, 0, defaultMaxRetryDelayMs)
	if d != 2*time.Second {
		t.Fatalf("expected 2s from Retry-After seconds, got %v", d)
	}
}

// TestRetryAfterHTTPDateParsed: an HTTP-date Retry-After is honored.
func TestRetryAfterHTTPDateParsed(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
	d := retryDelay(resp, 0, defaultMaxRetryDelayMs)
	if d < 8*time.Second || d > 10*time.Second {
		t.Fatalf("expected ~10s from Retry-After http-date, got %v", d)
	}
}

// TestRetryAfterAbove60sIgnoredUsesBackoff: a server delay >= 60s is IGNORED
// (not fail-fast) and the computed backoff is used instead — the old surface-
// the-response behavior came from the excluded codex provider.
func TestRetryAfterAbove60sIgnoredUsesBackoff(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "3600") // 1 hour
	d := retryDelay(resp, 0, defaultMaxRetryDelayMs)
	// attempt 0 backoff: 500ms with up to 25% downward jitter.
	if d < 375*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("expected backoff in [375ms,500ms] when RA>60s is ignored, got %v", d)
	}
}

// TestRetryAfterAbove60sStillRetries: end-to-end — a huge Retry-After does not
// abort retrying and does not block; the request is retried after backoff.
func TestRetryAfterAbove60sStillRetries(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	start := time.Now()
	build := func() (*http.Request, error) { return http.NewRequest("GET", server.URL, nil) }
	resp, err := sendWithRetry(context.Background(), build, retryConfig{maxRetries: 1, maxRetryDelayMs: defaultMaxRetryDelayMs, timeoutMs: defaultTimeoutMs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected success after ignoring huge Retry-After, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry waited too long (%v); huge Retry-After should be ignored", elapsed)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

// TestRetryBackoffJitterBounds: backoff is min(0.5s * 2^attempt, 8s) scaled by
// a jitter factor in (0.75, 1.0].
func TestRetryBackoffJitterBounds(t *testing.T) {
	for attempt, baseMs := range []int{500, 1000, 2000} {
		lo := time.Duration(float64(baseMs)*0.75) * time.Millisecond
		hi := time.Duration(baseMs) * time.Millisecond
		for i := 0; i < 200; i++ {
			d := retryDelay(nil, attempt, defaultMaxRetryDelayMs)
			if d < lo || d > hi {
				t.Fatalf("attempt %d: delay %v outside jitter bounds [%v,%v]", attempt, d, lo, hi)
			}
		}
	}
}

// TestRetryBackoffCappedAt8s: the computed backoff never exceeds the openai
// SDK's 8s cap, jitter included.
func TestRetryBackoffCappedAt8s(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := retryDelay(nil, 10, defaultMaxRetryDelayMs) // 0.5s*2^10 = 512s uncapped
		if d > 8*time.Second {
			t.Fatalf("backoff exceeded 8s cap: %v", d)
		}
		if d < 6*time.Second { // 8s * 0.75 jitter floor
			t.Fatalf("capped backoff below jitter floor: %v", d)
		}
	}
}

// TestRetryMaxRetryDelayOptionCapsBackoff: an explicit MaxRetryDelayMs below
// 8s caps the computed backoff.
func TestRetryMaxRetryDelayOptionCapsBackoff(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := retryDelay(nil, 10, 1000)
		if d > time.Second {
			t.Fatalf("backoff exceeded maxRetryDelayMs override: %v", d)
		}
		if d < 750*time.Millisecond {
			t.Fatalf("capped backoff below jitter floor: %v", d)
		}
	}
}

// TestRetryFromOptionsDefaults locks the config resolution: MaxRetries
// zero-value stays 0, negatives clamp to 0, explicit values pass through.
func TestRetryFromOptionsDefaults(t *testing.T) {
	if got := retryFromOptions(ai.StreamOptions{}); got.maxRetries != 0 {
		t.Fatalf("default maxRetries = %d, want 0", got.maxRetries)
	}
	if got := retryFromOptions(ai.StreamOptions{MaxRetries: -3}); got.maxRetries != 0 {
		t.Fatalf("negative maxRetries = %d, want 0", got.maxRetries)
	}
	if got := retryFromOptions(ai.StreamOptions{MaxRetries: 4}); got.maxRetries != 4 {
		t.Fatalf("explicit maxRetries = %d, want 4", got.maxRetries)
	}
	cap := 1234
	if got := retryFromOptions(ai.StreamOptions{MaxRetryDelayMs: &cap}); got.maxRetryDelayMs != 1234 {
		t.Fatalf("maxRetryDelayMs override = %d, want 1234", got.maxRetryDelayMs)
	}
	if got := retryFromOptions(ai.StreamOptions{}); got.maxRetryDelayMs != defaultMaxRetryDelayMs {
		t.Fatalf("default maxRetryDelayMs = %d, want %d", got.maxRetryDelayMs, defaultMaxRetryDelayMs)
	}
}

// TestRetryAbortWinsDuringBackoff: context cancellation during the backoff
// sleep aborts immediately instead of retrying.
func TestRetryAbortWinsDuringBackoff(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	build := func() (*http.Request, error) { return http.NewRequestWithContext(ctx, "GET", server.URL, nil) }
	_, err := sendWithRetry(ctx, build, retryConfig{maxRetries: 5, maxRetryDelayMs: defaultMaxRetryDelayMs, timeoutMs: defaultTimeoutMs})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected context cancellation error, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 attempt before abort, got %d", calls)
	}
}

// TestSharedClientSurvivesReplacedDefaultTransport: if http.DefaultTransport
// is replaced with a non-*http.Transport, sharedClient must not nil-deref.
func TestSharedClientSurvivesReplacedDefaultTransport(t *testing.T) {
	orig := http.DefaultTransport
	http.DefaultTransport = http.RoundTripper(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, nil
	}))
	defer func() { http.DefaultTransport = orig }()

	c := sharedClient(987_654) // unique timeout so the cache misses
	if c == nil || c.Transport == nil {
		t.Fatal("sharedClient returned nil client/transport with replaced DefaultTransport")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport fallback, got %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 987_654*time.Millisecond {
		t.Fatalf("fallback transport timeout = %v", tr.ResponseHeaderTimeout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResponsesPromptCacheKey(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = parseJSONWithRepair(string(body), &gotBody)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"r","status":"completed"}}`+"\n\n")
	}))
	defer server.Close()

	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: server.URL, MaxTokens: 100}
	StreamOpenAIResponses(context.Background(), model,
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}},
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "sess-123", CacheRetention: ai.CacheShort}}).Result()

	if gotBody["prompt_cache_key"] != "sess-123" {
		t.Fatalf("prompt_cache_key not sent: %v", gotBody["prompt_cache_key"])
	}
}
