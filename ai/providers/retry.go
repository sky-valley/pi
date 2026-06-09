package providers

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sky-valley/pi/ai"
)

const (
	defaultMaxRetries      = 2
	defaultMaxRetryDelayMs = 60_000  // 60s cap on a server-requested delay
	defaultTimeoutMs       = 600_000 // 10 minutes (matches the OpenAI/Anthropic SDK default)
	retryBaseDelayMs       = 500
)

// retryConfig captures the retry/timeout knobs resolved from StreamOptions.
type retryConfig struct {
	maxRetries      int
	maxRetryDelayMs int
	timeoutMs       int
}

func retryFromOptions(o ai.StreamOptions) retryConfig {
	cfg := retryConfig{
		maxRetries:      o.MaxRetries,
		maxRetryDelayMs: defaultMaxRetryDelayMs,
		timeoutMs:       o.TimeoutMs,
	}
	if cfg.maxRetries <= 0 {
		cfg.maxRetries = defaultMaxRetries
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
	base, _ := http.DefaultTransport.(*http.Transport)
	tr := base.Clone()
	tr.ResponseHeaderTimeout = time.Duration(timeoutMs) * time.Millisecond
	c := &http.Client{Transport: tr}
	clientCache[timeoutMs] = c
	return c
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// retryDelay computes the wait before the next attempt. It honors a server
// Retry-After header when present, otherwise uses exponential backoff. It
// returns ok=false when a server-requested delay exceeds the cap, signaling the
// caller to surface the response rather than wait.
func retryDelay(resp *http.Response, attempt, maxRetryDelayMs int) (time.Duration, bool) {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				ms := secs * 1000
				if maxRetryDelayMs > 0 && ms > maxRetryDelayMs {
					return 0, false
				}
				return time.Duration(ms) * time.Millisecond, true
			}
			if t, err := http.ParseTime(ra); err == nil {
				ms := int(time.Until(t).Milliseconds())
				if ms < 0 {
					ms = 0
				}
				if maxRetryDelayMs > 0 && ms > maxRetryDelayMs {
					return 0, false
				}
				return time.Duration(ms) * time.Millisecond, true
			}
		}
	}
	ms := retryBaseDelayMs * int(math.Pow(2, float64(attempt)))
	if maxRetryDelayMs > 0 && ms > maxRetryDelayMs {
		ms = maxRetryDelayMs
	}
	return time.Duration(ms) * time.Millisecond, true
}

// sendWithRetry issues the request built by build, retrying transient network
// errors and retryable HTTP statuses with backoff. build must produce a fresh
// *http.Request on each call (request bodies are single-use).
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
			delay, _ := retryDelay(nil, attempt, cfg.maxRetryDelayMs)
			if !sleepCtx(ctx, delay) {
				return nil, ctx.Err()
			}
			continue
		}
		if isRetryableStatus(resp.StatusCode) && attempt < attempts-1 {
			delay, ok := retryDelay(resp, attempt, cfg.maxRetryDelayMs)
			if !ok {
				// Server asked to wait longer than the cap: surface the response so
				// the caller reports the rate limit instead of blocking.
				return resp, nil
			}
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
