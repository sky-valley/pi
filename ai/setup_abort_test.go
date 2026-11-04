package ai

import (
	"context"
	"errors"
	"testing"
)

// A setup failure that happens after the request's context is done reads as
// an abort, not an error (upstream f3a64af07, #8409): pi's lazyStream checks
// the request's signal when its setup rejects, so a turn aborted while a tool
// ran — whose next request's auth setup then fails on the aborted signal —
// ends as "aborted". Every lazyStream that receives the signal does it:
// Models.stream / streamSimple / fetchDeferred, and createProvider's dispatch
// for a model whose api the provider has no implementation of.
func TestSetupFailureAfterAbortReadsAsAborted(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	ghost := &Model{Provider: "ghost", ID: "x", Api: "api"}
	m := CreateModels(nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:       "p",
		Models:   []*Model{{Provider: "p", ID: "m", Api: "api"}},
		APIByApi: map[Api]ProviderStreams{"api": *stubAPI()},
	}))
	noAPI := &Model{Provider: "p", ID: "m", Api: "other-api"}

	cases := []struct {
		name string
		run  func(ctx context.Context) *AssistantMessageEventStream
	}{
		{"Models.Stream", func(ctx context.Context) *AssistantMessageEventStream {
			return m.Stream(ctx, ghost, Context{}, nil)
		}},
		{"Models.StreamSimple", func(ctx context.Context) *AssistantMessageEventStream {
			return m.StreamSimple(ctx, ghost, Context{}, nil)
		}},
		{"Models.StreamDeferred", func(ctx context.Context) *AssistantMessageEventStream {
			return m.StreamDeferred(ctx, ghost, DeferredHandle{}, nil)
		}},
		{"provider without the model's api", func(ctx context.Context) *AssistantMessageEventStream {
			return m.GetProvider("p").StreamSimple(ctx, noAPI, TranscriptContext{}, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range []struct {
				ctx    context.Context
				reason StopReason
			}{{context.Background(), StopError}, {cancelled, StopAborted}} {
				stream := tc.run(want.ctx)
				var terminal *AssistantMessageEvent
				for event := range stream.Events() {
					if event.Type == EventError {
						e := event
						terminal = &e
					}
				}
				res := stream.Result()
				if terminal == nil || terminal.Reason != want.reason {
					t.Fatalf("ctx err %v: terminal event %+v, want reason %q", want.ctx.Err(), terminal, want.reason)
				}
				if res.StopReason != want.reason || res.ErrorMessage == "" {
					t.Fatalf("ctx err %v: result stopReason %q errorMessage %q, want %q with the setup's message",
						want.ctx.Err(), res.StopReason, res.ErrorMessage, want.reason)
				}
			}
		})
	}

	// ErrorStream has no request context, as a lazyStream given no signal
	// (azure's endpoint resolution) has none: it stays an error.
	if res := ErrorStream(ghost, errors.New("setup failed")).Result(); res.StopReason != StopError {
		t.Fatalf("ErrorStream stopReason %q, want error", res.StopReason)
	}
}
