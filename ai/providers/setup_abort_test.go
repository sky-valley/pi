package providers

import (
	"context"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// A provider's streamSimple that throws before its stream exists — a missing
// key, a thinking level Google cannot map — reaches the caller through
// lazyApi, which since upstream f3a64af07 hands lazyStream the request's
// signal: once the request has aborted, the throw reads as an abort. azure's
// endpoint resolution runs in a lazyStream given no signal, so it stays an
// error.
func TestSimpleSetupThrowAfterAbortReadsAsAborted(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	noKey := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{Env: map[string]string{}}}}
	cases := []struct {
		name   string
		stream func(ctx context.Context) *ai.AssistantMessageEventStream
		want   ai.StopReason
	}{
		{"anthropic key", func(ctx context.Context) *ai.AssistantMessageEventStream {
			m := &ai.Model{Api: "anthropic-messages", Provider: "anthropic", ID: "claude", BaseURL: "https://api.anthropic.com", MaxTokens: 1000}
			return StreamSimpleAnthropic(ctx, m, ai.TranscriptContext{}, noKey)
		}, ai.StopAborted},
		{"openai completions key", func(ctx context.Context) *ai.AssistantMessageEventStream {
			m := &ai.Model{Api: "openai-completions", Provider: "openai", ID: "gpt", BaseURL: "https://api.openai.com/v1", MaxTokens: 1000}
			return StreamSimpleOpenAICompletions(ctx, m, ai.TranscriptContext{}, noKey)
		}, ai.StopAborted},
		{"openai responses key", func(ctx context.Context) *ai.AssistantMessageEventStream {
			m := &ai.Model{Api: "openai-responses", Provider: "openai", ID: "gpt", BaseURL: "https://api.openai.com/v1", MaxTokens: 1000}
			return StreamSimpleOpenAIResponses(ctx, m, ai.TranscriptContext{}, noKey)
		}, ai.StopAborted},
		{"google key", func(ctx context.Context) *ai.AssistantMessageEventStream {
			m := &ai.Model{Api: "google-generative-ai", Provider: "google", ID: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta", MaxTokens: 1000}
			return StreamSimpleGoogle(ctx, m, ai.TranscriptContext{}, noKey)
		}, ai.StopAborted},
		{"azure endpoint", func(ctx context.Context) *ai.AssistantMessageEventStream {
			m := &ai.Model{Api: "openai-completions", Provider: "azure", ID: "gpt", MaxTokens: 1000}
			return StreamSimpleOpenAICompletions(ctx, m, ai.TranscriptContext{}, noKey)
		}, ai.StopError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if res := tc.stream(context.Background()).Result(); res.StopReason != ai.StopError || res.ErrorMessage == "" {
				t.Fatalf("live ctx: stopReason %q errorMessage %q, want an error", res.StopReason, res.ErrorMessage)
			}
			stream := tc.stream(cancelled)
			var reason ai.StopReason
			for event := range stream.Events() {
				if event.Type == ai.EventError {
					reason = event.Reason
				}
			}
			if res := stream.Result(); res.StopReason != tc.want || reason != tc.want {
				t.Fatalf("cancelled ctx: stopReason %q, event reason %q, want %q (%s)", res.StopReason, reason, tc.want, res.ErrorMessage)
			}
		})
	}
}
