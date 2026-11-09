package providers

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicStrictFallbackCaptureFile is what testdata/anthropic-strict-fallback/
// capture.mjs recorded from pi's anthropic-messages stream at src ea1cfcc4c,
// replaying upstream's anthropic-strict-tool-fallback.test.ts. The session rows
// are upstream 45344bd3a's; at its parent, and in the pi-ai 1.5.0 build, every
// other row reproduces the file this one supersedes (strict-fallback-1.3.0.json).
const anthropicStrictFallbackCaptureFile = "testdata/anthropic-strict-fallback/strict-fallback-ea1cfcc4c.json"

// When Anthropic rejects strict tools for its own limits — a 400
// invalid_request_error saying the compiled grammar is too large, or there are
// too many strict tools, or too many union-typed parameters — and the request
// sent a strict:"prefer" tool strict, pi rebuilds the params with every prefer
// tool non-strict (require stays strict, inline definitions included), hands
// them to onPayload again, retries once, and records an
// anthropic_strict_tools_fallback diagnostic carrying the SDK's error message
// (upstream d4d53612b). Any other rejection, a second one, an abort, or an
// error event after the response started is surfaced as before. Since
// upstream 45344bd3a the rejection is remembered per session: that session's
// later requests send their prefer tools non-strict from the start, and never
// fall back again, until ai.CleanupSessionResources forgets it.
func TestAnthropicStrictToolFallbackMatchesPi(t *testing.T) {
	data, err := os.ReadFile(anthropicStrictFallbackCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		Events       []string        `json:"events"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage *string         `json:"errorMessage"`
		Diagnostics  json.RawMessage `json:"diagnostics"`
	}
	var capture struct {
		Rows []struct {
			Name      string            `json:"name"`
			Model     ai.Model          `json:"model"`
			Messages  []json.RawMessage `json:"messages"`
			Responses []struct {
				Status      int    `json:"status"`
				ContentType string `json:"contentType"`
				Body        string `json:"body"`
			} `json:"responses"`
			Options struct {
				APIKey         string            `json:"apiKey"`
				CacheRetention string            `json:"cacheRetention"`
				OnPayload      string            `json:"onPayload"`
				AbortOnRequest int               `json:"abortOnRequest"`
				Streams        int               `json:"streams"`
				SessionIDs     []string          `json:"sessionIds"`
				CleanupBefore  map[string]string `json:"cleanupBefore"`
			} `json:"options"`
			Payloads        []json.RawMessage `json:"payloads"`
			OnPayloadCalls  int               `json:"onPayloadCalls"`
			OnResponseCalls int               `json:"onResponseCalls"`
			Results         []result          `json:"results"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", anthropicStrictFallbackCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", anthropicStrictFallbackCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			messages := decodeCapturedMessages(t, row.Name, row.Messages)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			doer := &scriptedDoer{onRequest: func(n int) {
				if n == row.Options.AbortOnRequest {
					cancel()
				}
			}}
			for _, r := range row.Responses {
				doer.responses = append(doer.responses, wireResponse{status: r.Status, body: r.Body, contentType: r.ContentType})
			}
			onPayloadCalls, onResponseCalls := 0, 0
			apiKey := row.Options.APIKey
			if apiKey == "" {
				apiKey = "test-key"
			}
			opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{
				CacheRetention: ai.CacheRetention(row.Options.CacheRetention),
				ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey:     apiKey,
					HTTPClient: doer,
					OnResponse: func(ai.ProviderResponse, *ai.Model) error {
						onResponseCalls++
						return nil
					},
					OnPayload: func(payload any, _ *ai.Model) (any, error) {
						onPayloadCalls++
						if row.Options.OnPayload != "inferenceGeo" {
							return nil, nil
						}
						next := maps.Clone(payload.(map[string]any))
						next["inference_geo"], next["stream"] = "us", false
						return next, nil
					},
				},
			}}
			model := row.Model
			var results []result
			// Each row starts and ends with no remembered session, as the
			// capture's cleanupSessionResources() after every row does.
			if err := ai.CleanupSessionResources(""); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ai.CleanupSessionResources("") })
			for i := range max(row.Options.Streams, 1) {
				if sessionID, ok := row.Options.CleanupBefore[strconv.Itoa(i)]; ok {
					if err := ai.CleanupSessionResources(sessionID); err != nil {
						t.Fatal(err)
					}
				}
				opts.SessionID = ""
				if i < len(row.Options.SessionIDs) {
					opts.SessionID = row.Options.SessionIDs[i]
				}
				stream := StreamAnthropic(ctx, &model, ai.NormalizeContext(ai.Context{Messages: messages}), opts)
				var events []string
				for ev := range stream.Events() {
					events = append(events, string(ev.Type))
				}
				final := stream.Result()
				r := result{Events: events, StopReason: string(final.StopReason)}
				if final.ErrorMessage != "" {
					r.ErrorMessage = &final.ErrorMessage
				}
				r.Diagnostics = []byte("null")
				if len(final.Diagnostics) > 0 {
					diagnostics := slices.Clone(final.Diagnostics)
					for i := range diagnostics {
						diagnostics[i].Timestamp = 0
					}
					r.Diagnostics, _ = json.Marshal(diagnostics)
				}
				results = append(results, r)
			}
			if len(doer.requests) != len(row.Payloads) {
				t.Fatalf("%d requests, pi sent %d", len(doer.requests), len(row.Payloads))
			}
			for i, sent := range doer.requests {
				if !sameJSON(t, sent.body, row.Payloads[i]) {
					t.Errorf("request %d\n%s\npi\n%s", i+1, sent.body, compactJSON(row.Payloads[i]))
				}
			}
			if onPayloadCalls != row.OnPayloadCalls || onResponseCalls != row.OnResponseCalls {
				t.Errorf("onPayload ran %d times and onResponse %d, pi %d and %d",
					onPayloadCalls, onResponseCalls, row.OnPayloadCalls, row.OnResponseCalls)
			}
			got, _ := json.Marshal(results)
			want, _ := json.Marshal(row.Results)
			if !sameJSON(t, got, want) {
				t.Errorf("results\n%s\npi\n%s", got, want)
			}
		})
	}
}
