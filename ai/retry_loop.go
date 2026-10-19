package ai

import (
	"context"
	"math"
	"math/bits"
	"time"
)

// Policy-driven assistant-call retry, ported from pi
// packages/ai/src/utils/retry.ts (upstream 65dd2e0e). Kept alongside
// IsRetryableAssistantError so the classifier and the retry loop live together
// and stay reusable by the SDK and other callers. pi's consumer is the
// coding agent's compaction (core/compaction/compaction.ts wraps each summary
// call in retryAssistantCall), under the policy AgentSession reads from
// settings.retry (getRetrySettings).

// RetryPolicy is bounded retry with exponential backoff
// (baseDelayMs * 2^(attempt-1)), capped at MaxAgentDelayMs. Mirrors pi's
// RetryPolicy / settings.retry.
type RetryPolicy struct {
	Enabled bool
	// MaxRetries is the max retry attempts (0 = no retries). The initial call
	// never counts as a retry.
	MaxRetries int
	// BaseDelayMs is the base backoff; the per-attempt delay is
	// BaseDelayMs * 2^(attempt-1), before MaxAgentDelayMs caps it.
	BaseDelayMs int
	// MaxAgentDelayMs caps each computed backoff. Nil takes
	// DefaultMaxAgentRetryDelayMs; a zero value is a real cap of zero, not
	// "disabled" — pi's `?? DEFAULT_MAX_AGENT_RETRY_DELAY_MS` falls back on
	// undefined/null only, and upstream pins retryDelayMs({baseDelayMs: 2000,
	// maxAgentDelayMs: 0}, 5) === 0. That is the OPPOSITE of
	// ProviderRequestOptions.MaxRetryDelayMs, whose zero DISABLES its cap: the
	// two are different mechanisms that happen to share a 60s default. This one
	// clamps a locally computed backoff; that one rejects an over-long
	// server-requested Retry-After.
	MaxAgentDelayMs *int
}

// DefaultMaxAgentRetryDelayMs is pi's DEFAULT_MAX_AGENT_RETRY_DELAY_MS: the cap
// applied to an agent-level retry backoff when the policy leaves it unset.
const DefaultMaxAgentRetryDelayMs = 60_000

// RetryDelayMs is pi's retryDelayMs: the backoff for one attempt, capped.
//
// pi computes baseDelayMs * 2^max(0, attempt-1), saturates a result that is not
// a safe integer to Number.MAX_SAFE_INTEGER, then takes the minimum against the
// cap. The floor on the exponent matters here for a second reason: Go panics on
// a negative shift count where JS would compute a fraction, and this is an
// exported helper whose callers choose their own attempt numbering.
//
// The saturation step is pi's guard against an unrepresentable delay, and Go
// needs it for the same reason in a different arithmetic: an int shift wraps
// silently, and a wrapped-negative delay makes time.NewTimer fire immediately —
// turning the backoff into a hot loop. Saturating to math.MaxInt where pi
// saturates to 2^53-1 is indistinguishable through the cap that follows.
func RetryDelayMs(policy RetryPolicy, attempt int) int {
	maxDelayMs := DefaultMaxAgentRetryDelayMs
	if policy.MaxAgentDelayMs != nil {
		maxDelayMs = *policy.MaxAgentDelayMs
	}
	delayMs := shiftSaturating(policy.BaseDelayMs, attempt-1)
	if delayMs > maxDelayMs {
		return maxDelayMs
	}
	return delayMs
}

// shiftSaturating returns base * 2^shift, saturating to math.MaxInt on overflow
// rather than wrapping. A negative shift is pi's Math.max(0, attempt-1).
func shiftSaturating(base, shift int) int {
	if shift <= 0 || base == 0 {
		return base
	}
	if shift >= bits.UintSize {
		return math.MaxInt
	}
	shifted := base << uint(shift)
	if shifted>>uint(shift) != base {
		return math.MaxInt
	}
	return shifted
}

// RetryCallbacks are the optional hooks pi's retryAssistantCall emits around
// each retry. Any field may be nil, and the whole *RetryCallbacks may be nil.
type RetryCallbacks struct {
	// OnRetryScheduled fires before the backoff sleep of each retry attempt
	// (1-indexed).
	OnRetryScheduled func(attempt, maxAttempts, delayMs int, errorMessage string)
	// OnRetryAttemptStart fires after the backoff sleep, immediately before the
	// retried call starts.
	OnRetryAttemptStart func()
	// OnRetryFinished fires once when the loop ends: success is true if a later
	// call completed normally.
	OnRetryFinished func(success bool, attempt int, finalError string)
}

// The following nil-safe dispatchers let RetryAssistantCall emit callbacks
// unconditionally: a nil *RetryCallbacks or an unset hook is a no-op.

func (c *RetryCallbacks) scheduled(attempt, maxAttempts, delayMs int, errorMessage string) {
	if c != nil && c.OnRetryScheduled != nil {
		c.OnRetryScheduled(attempt, maxAttempts, delayMs, errorMessage)
	}
}

func (c *RetryCallbacks) attemptStart() {
	if c != nil && c.OnRetryAttemptStart != nil {
		c.OnRetryAttemptStart()
	}
}

func (c *RetryCallbacks) finished(success bool, attempt int, finalError string) {
	if c != nil && c.OnRetryFinished != nil {
		c.OnRetryFinished(success, attempt, finalError)
	}
}

// RetryAssistantCall runs a single assistant-producing call with bounded retry
// on transient errors (pi retryAssistantCall).
//
// Behavior:
//   - A successful response returns immediately. Aborts are terminal and never
//     retried, but reported as unsuccessful if they happen after a retry was
//     scheduled. Aborts during the backoff sleep are normalized to an aborted
//     AssistantMessage too, so callers need not care when cancellation happened.
//   - A non-retryable error (per IsRetryableAssistantError, including quota/
//     billing exhaustion) returns immediately so deterministic errors fail fast.
//   - Otherwise retries up to policy.MaxRetries times with exponential backoff,
//     emitting OnRetryScheduled before each sleep, OnRetryAttemptStart after each
//     sleep before the retried call starts, and OnRetryFinished once at the end
//     (whether the loop ends in success, exhausted retries, or an aborted sleep).
//
// When policy is nil or disabled, the first response is returned unchanged
// (equivalent to calling produce() directly). produce must return a non-nil
// *AssistantMessage (matching pi's Promise<AssistantMessage> contract).
func RetryAssistantCall(ctx context.Context, produce func() *AssistantMessage, policy *RetryPolicy, callbacks *RetryCallbacks) *AssistantMessage {
	maxAttempts := 0
	if policy != nil && policy.Enabled {
		maxAttempts = policy.MaxRetries
	}

	attempt := 0
	lastRetryScheduled := false
	lastRetryAttempt := 0
	for {
		response := produce()

		// Abort: terminal but not successful. Never retry an aborted message.
		if response.StopReason == StopAborted {
			if lastRetryScheduled {
				callbacks.finished(false, lastRetryAttempt, "")
			}
			return response
		}

		// Success: non-error, non-abort responses return as-is.
		if response.StopReason != StopError {
			if lastRetryScheduled {
				callbacks.finished(true, lastRetryAttempt, "")
			}
			return response
		}

		// Non-retryable, or budget exhausted: return the final error message.
		if attempt >= maxAttempts || !IsRetryableAssistantError(*response) {
			if lastRetryScheduled {
				callbacks.finished(false, lastRetryAttempt, response.ErrorMessage)
			}
			return response
		}

		attempt++
		lastRetryScheduled = true
		lastRetryAttempt = attempt
		errorMessage := response.ErrorMessage
		if errorMessage == "" {
			errorMessage = "Unknown error"
		}
		delayMs := RetryDelayMs(*policy, attempt)
		callbacks.scheduled(attempt, maxAttempts, delayMs, errorMessage)

		// Normalize aborts during retry backoff to the same AssistantMessage shape
		// as provider stream aborts, so callers need not care when cancellation
		// happened.
		timer := time.NewTimer(time.Duration(delayMs) * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			callbacks.finished(false, attempt, errorMessage)
			aborted := *response
			aborted.StopReason = StopAborted
			aborted.ErrorMessage = ""
			return &aborted
		}
		callbacks.attemptStart()
	}
}
