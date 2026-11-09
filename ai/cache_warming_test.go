package ai

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// cacheWarmingCaptureFile is what testdata/cache-warming/capture.mjs recorded
// from pi-ai's cache-warming.ts at src ea1cfcc4c (upstream c9552c7d8 added the
// module; no published build has it yet).
const cacheWarmingCaptureFile = "testdata/cache-warming/cache-warming-ea1cfcc4c.json"

// pi-ai's prompt cache warming policy (upstream c9552c7d8) matches pi's
// output for every captured case: the warm-or-stop decision bit for bit,
// including a pricing tier on both sides of its threshold and models without
// cache-write or any prices; the refresh delay; the cache lifetime per
// retention, PI_CACHE_RETENTION and model tier; whether a request can be
// replayed; the refresh options; and the constants.
func TestCacheWarmingMatchesPi(t *testing.T) {
	// The capture ran with PI_CACHE_RETENTION unset; rows that want it set it
	// in their options' Env.
	t.Setenv("PI_CACHE_RETENTION", "")
	data, err := os.ReadFile(cacheWarmingCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Constants struct {
			MaxAgeMs                    int     `json:"maxAgeMs"`
			MaxIdleAgeMs                int     `json:"maxIdleAgeMs"`
			MinimumExpectedSavings      float64 `json:"minimumExpectedSavings"`
			IdleContinuationProbability float64 `json:"idleContinuationProbability"`
		} `json:"constants"`
		Models    map[string]*Model `json:"models"`
		Decisions []struct {
			Model        string            `json:"model"`
			PromptTokens int               `json:"promptTokens"`
			Phase        CacheWarmingPhase `json:"phase"`
			Decision     json.RawMessage   `json:"decision"`
		} `json:"decisions"`
		Delays []struct {
			TTLMs   float64  `json:"ttlMs"`
			DelayMs *float64 `json:"delayMs"`
		} `json:"delays"`
		TTLs []struct {
			Model          string            `json:"model"`
			Label          string            `json:"label"`
			CacheRetention CacheRetention    `json:"cacheRetention"`
			Env            map[string]string `json:"env"`
			TTLMs          *float64          `json:"ttlMs"`
		} `json:"ttls"`
		Replayable []struct {
			Model      string         `json:"model"`
			Reasoning  *ThinkingLevel `json:"reasoning"`
			Replayable bool           `json:"replayable"`
		} `json:"replayable"`
		RequestOptions struct {
			MaxTokens           int            `json:"maxTokens"`
			MaxRetries          int            `json:"maxRetries"`
			DisableContinuation bool           `json:"disableContinuation"`
			CacheRetention      CacheRetention `json:"cacheRetention"`
			SessionID           string         `json:"sessionId"`
		} `json:"requestOptions"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", cacheWarmingCaptureFile, err)
	}
	if len(capture.Decisions) == 0 || len(capture.Delays) == 0 || len(capture.TTLs) == 0 || len(capture.Replayable) == 0 {
		t.Fatalf("%s holds an empty table; rerun capture.mjs", cacheWarmingCaptureFile)
	}
	model := func(name string) *Model {
		t.Helper()
		m := capture.Models[name]
		if m == nil {
			t.Fatalf("no captured model %q", name)
		}
		return m
	}

	c := capture.Constants
	if c.MaxAgeMs != CacheWarmingMaxAgeMs || c.MaxIdleAgeMs != CacheWarmingMaxIdleAgeMs ||
		c.MinimumExpectedSavings != CacheWarmingMinimumExpectedSavings ||
		c.IdleContinuationProbability != CacheWarmingIdleContinuationProbability {
		t.Errorf("constants differ from pi's %+v", c)
	}

	for _, row := range capture.Decisions {
		got := EvaluateCacheWarming(model(row.Model), row.PromptTokens, row.Phase)
		if !sameCacheWarmingDecision(t, got, row.Decision) {
			t.Errorf("%s at %d tokens, %s:\n go %+v\n pi %s", row.Model, row.PromptTokens, row.Phase, got, row.Decision)
		}
	}

	for _, row := range capture.Delays {
		delay, ok := CacheWarmingDelayMs(row.TTLMs)
		if ok != (row.DelayMs != nil) || (ok && delay != *row.DelayMs) {
			t.Errorf("delay for a %vms TTL: %v %v, pi %v", row.TTLMs, delay, ok, row.DelayMs)
		}
	}

	for _, row := range capture.TTLs {
		var opts *StreamOptions
		if row.Label != "none" {
			opts = &StreamOptions{CacheRetention: row.CacheRetention}
			opts.Env = row.Env
		}
		ttl, ok := PromptCacheTTLMs(model(row.Model), opts)
		if ok != (row.TTLMs != nil) || (ok && ttl != *row.TTLMs) {
			t.Errorf("%s, %s: TTL %v %v, pi %v", row.Model, row.Label, ttl, ok, row.TTLMs)
		}
	}

	for _, row := range capture.Replayable {
		var opts *SimpleStreamOptions
		if row.Reasoning != nil {
			opts = &SimpleStreamOptions{Reasoning: *row.Reasoning}
		}
		if got := CacheWarmingReplayable(model(row.Model), opts); got != row.Replayable {
			t.Errorf("%s with reasoning %v: replayable %v, pi %v", row.Model, row.Reasoning, got, row.Replayable)
		}
	}

	maxTokens := 4096
	in := StreamOptions{MaxTokens: &maxTokens, CacheRetention: CacheLong, SessionID: "s"}
	in.MaxRetries = 5
	out := CacheWarmingRequestOptions(in)
	want := capture.RequestOptions
	if out.MaxTokens == nil || *out.MaxTokens != want.MaxTokens || out.MaxRetries != want.MaxRetries ||
		out.DisableContinuation != want.DisableContinuation || out.CacheRetention != want.CacheRetention ||
		out.SessionID != want.SessionID {
		t.Errorf("refresh options %+v, pi %+v", out, want)
	}
	if *in.MaxTokens != 4096 || in.MaxRetries != 5 || in.DisableContinuation {
		t.Errorf("CacheWarmingRequestOptions changed the request's own options: %+v", in)
	}
}

// pi reads compat.forceAdaptiveThinking with `=== true`: only that exact key
// holding the JSON value true counts.
func TestCacheWarmingReplayableReadsCompatExactly(t *testing.T) {
	opts := &SimpleStreamOptions{Reasoning: ThinkingHigh}
	for _, tc := range []struct {
		compat string
		want   bool
	}{
		{``, false},
		{`{"forceAdaptiveThinking":true}`, true},
		{`{"forceAdaptiveThinking":"true"}`, false},
		{`{"forceAdaptiveThinking":1}`, false},
		{`{"forceadaptivethinking":true}`, false},
		{`{"forceAdaptiveThinking":true,"forceAdaptiveThinking":false}`, false},
		{`{"forceAdaptiveThinking":false,"forceAdaptiveThinking":true}`, true},
		{`[true]`, false},
	} {
		m := &Model{Api: APIAnthropicMessages, Compat: json.RawMessage(tc.compat)}
		if got := CacheWarmingReplayable(m, opts); got != tc.want {
			t.Errorf("compat %s: %v, want %v", tc.compat, got, tc.want)
		}
	}
}

// sameCacheWarmingDecision compares a Go decision with pi's as decoded JSON
// values: the same keys, and numbers with the same float64 bits (JSON.stringify
// prints the shortest text that round-trips a double).
func sameCacheWarmingDecision(t *testing.T, got CacheWarmingDecision, pi json.RawMessage) bool {
	t.Helper()
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g, p map[string]any
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(pi, &p); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, p)
}
