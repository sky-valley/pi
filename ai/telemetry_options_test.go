package ai

import (
	"context"
	"testing"

	"github.com/sky-valley/pi/telemetry"
)

// Ported from pi packages/ai/test/telemetry-options.test.ts (upstream
// 04d6447f7, retargeted by 6b461b75b). The images half is unported along with
// the images surface, and pi's buildBaseOptions/`satisfies` inheritance case
// is structural here: StreamOptions and the deferred options embed
// ProviderRequestOptions, and each provider's StreamSimple copies
// StreamOptions whole (see simple_options.go).

// tracedContext delegates to the shared noop context but is a pointer type,
// so identity assertions below mean "the caller's own context arrived", the
// way pi's `toBe(telemetryContext)` does.
type tracedContext struct{}

func (*tracedContext) StartSpan(options telemetry.SpanOptions, callback func(telemetry.Span) error) error {
	return telemetry.NoopContext.StartSpan(options, callback)
}

func (*tracedContext) Metrics() telemetry.Metrics         { return telemetry.NoopMetrics }
func (*tracedContext) Propagation() telemetry.Propagation { return telemetry.NoopPropagation }

// TestTelemetryContextSurvivesDispatch mirrors pi "survives provider and
// Models stream/deferred dispatch": a TelemetryContext set on the caller's
// request options must reach the provider api on all eight dispatch paths —
// provider-direct and Models Stream/StreamSimple/FetchDeferred/CancelDeferred.
// The Models paths copy the options whole (applyAuth's `ro = *opts`), so like
// TestModelsFetchDeferredKeepsTheCallersOwnOptions this guards against a
// future rebuild that hand-copies fields and silently drops one.
func TestTelemetryContextSurvivesDispatch(t *testing.T) {
	telemetryContext := &tracedContext{}
	var observed []telemetry.Context
	done := func(model *Model) *AssistantMessageEventStream {
		s := NewAssistantMessageEventStream()
		s.Push(AssistantMessageEvent{
			Type: EventDone, Reason: StopStop,
			Message: &AssistantMessage{Api: model.Api, Provider: model.Provider, Model: model.ID, StopReason: StopStop},
		})
		s.End()
		return s
	}
	p := CreateProvider(CreateProviderOptions{
		ID:     "telemetry-provider",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("telemetry-provider", "K")},
		Models: []*Model{{Provider: "telemetry-provider", ID: "model", Api: "telemetry-test"}},
		API: ptrStreams(ProviderStreams{
			Stream: func(_ context.Context, model *Model, _ TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
				observed = append(observed, opts.TelemetryContext)
				return done(model)
			},
			StreamSimple: func(_ context.Context, model *Model, _ TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
				observed = append(observed, opts.TelemetryContext)
				return done(model)
			},
			FetchDeferred: func(_ context.Context, model *Model, _ DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
				observed = append(observed, opts.TelemetryContext)
				return done(model)
			},
			CancelDeferred: func(_ context.Context, _ *Model, _ DeferredHandle, opts *DeferredCancelOptions) error {
				observed = append(observed, opts.TelemetryContext)
				return nil
			},
		}),
	})

	ctx := context.Background()
	model := &Model{Provider: "telemetry-provider", ID: "model", Api: "telemetry-test"}
	handle := DeferredHandle{Provider: model.Provider, ModelID: model.ID, Api: model.Api, ID: "response"}
	request := ProviderRequestOptions{TelemetryContext: telemetryContext}

	p.Stream(ctx, model, TranscriptContext{}, &StreamOptions{ProviderRequestOptions: request}).Result()
	p.StreamSimple(ctx, model, TranscriptContext{}, &SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: request}}).Result()
	p.(DeferredFetcher).FetchDeferred(ctx, model, handle, &DeferredFetchOptions{ProviderRequestOptions: request}).Result()
	cancelOptions := DeferredCancelOptions(request)
	if err := p.(DeferredCanceller).CancelDeferred(ctx, model, handle, &cancelOptions); err != nil {
		t.Fatalf("provider CancelDeferred: %v", err)
	}

	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(p)
	m.Stream(ctx, model, Context{}, &ModelsStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: request}}).Result()
	m.StreamSimple(ctx, model, Context{}, &ModelsSimpleStreamOptions{SimpleStreamOptions: SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: request}}}).Result()
	m.FetchDeferred(ctx, model, handle, &ModelsDeferredFetchOptions{DeferredFetchOptions: DeferredFetchOptions{ProviderRequestOptions: request}})
	if err := m.CancelDeferred(ctx, model, handle, &ModelsDeferredCancelOptions{DeferredCancelOptions: request}); err != nil {
		t.Fatalf("models CancelDeferred: %v", err)
	}

	if len(observed) != 8 {
		t.Fatalf("observed %d dispatches, want all 8 to reach the provider api", len(observed))
	}
	for i, got := range observed {
		if got != telemetry.Context(telemetryContext) {
			t.Errorf("dispatch %d saw telemetry context %#v, want the caller's own", i, got)
		}
	}
}
