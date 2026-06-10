package providers

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sky-valley/pi/ai"
)

const (
	// defaultMaxRetryDelayMs is the threshold beyond which a server-requested
	// Retry-After delay is ignored in favor of the computed backoff (matches
	// the openai SDK's 60s validity window).
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
}

// retryFromOptions mirrors pi's `maxRetries: options?.maxRetries ?? 0` passed
// to the SDKs: an unset/zero MaxRetries means ZERO retries (single attempt).
func retryFromOptions(o ai.StreamOptions) retryConfig {
	cfg := retryConfig{
		maxRetries:      o.MaxRetries,
		maxRetryDelayMs: defaultMaxRetryDelayMs,
		timeoutMs:       o.TimeoutMs,
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

// serverRetryDelayMs extracts a server-requested retry delay in milliseconds.
// `retry-after-ms` is preferred; otherwise `Retry-After` is parsed as seconds
// or as an HTTP date. Returns ok=false when no parseable header is present.
func serverRetryDelayMs(resp *http.Response) (int, bool) {
	if resp == nil {
		return 0, false
	}
	if v := resp.Header.Get("retry-after-ms"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return int(f), true
		}
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.ParseFloat(ra, 64); err == nil {
			return int(secs * 1000), true
		}
		if t, err := http.ParseTime(ra); err == nil {
			return int(time.Until(t).Milliseconds()), true
		}
	}
	return 0, false
}

// retryDelay computes the wait before the next attempt, mirroring the openai
// SDK: a server-requested delay is honored only when it is positive and below
// maxRetryDelayMs (default 60s) — anything larger is IGNORED (not fail-fast)
// and the computed backoff is used instead. The computed backoff is
// min(0.5s * 2^attempt, 8s) with up to 25% downward jitter; an explicit
// maxRetryDelayMs option below 8s also caps the backoff.
func retryDelay(resp *http.Response, attempt, maxRetryDelayMs int) time.Duration {
	if ms, ok := serverRetryDelayMs(resp); ok && ms > 0 && ms < maxRetryDelayMs {
		return time.Duration(ms) * time.Millisecond
	}
	backoff := math.Min(float64(retryBaseDelayMs)*math.Pow(2, float64(attempt)), retryBackoffCapMs)
	if maxRetryDelayMs > 0 && float64(maxRetryDelayMs) < backoff {
		backoff = float64(maxRetryDelayMs)
	}
	jitter := 1 - rand.Float64()*0.25
	return time.Duration(backoff*jitter) * time.Millisecond
}

// sendWithRetry issues the request built by build, retrying transient network
// errors (like 5xx) and retryable HTTP statuses with backoff. build must
// produce a fresh *http.Request on each call (request bodies are single-use).
// With cfg.maxRetries == 0 (pi's default) exactly one attempt is made.
func sendWithRetry(ctx context.Context, build func() (*http.Request, error), cfg retryConfig) (*http.Response, error) {
	client := sharedClient(cfg.timeoutMs)
	attempts := cfg.maxRetries + 1
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt == attempts-1 {
				return nil, err
			}
			if !sleepCtx(ctx, retryDelay(nil, attempt, cfg.maxRetryDelayMs)) {
				return nil, ctx.Err()
			}
			continue
		}
		if shouldRetryResponse(resp) && attempt < attempts-1 {
			delay := retryDelay(resp, attempt, cfg.maxRetryDelayMs)
			drain(resp)
			if !sleepCtx(ctx, delay) {
				return nil, ctx.Err()
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

func drain(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}
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
