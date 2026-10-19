package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// hasExplicitAPIKey is compat.ts hasExplicitApiKey: a key that is blank as
// JavaScript trims is no key.
func hasExplicitAPIKey(key string) bool {
	return jstext.Trim(key) != ""
}

// anthropicAuthTokenActive reports whether the anthropic bearer auth token is
// set for this request. When it is, the compat path leaves APIKey unresolved so
// the provider emits it as an Authorization header rather than x-api-key.
func anthropicAuthTokenActive(model *Model, env map[string]string) bool {
	return model.Provider == "anthropic" && ProviderEnvValue(AnthropicAuthTokenEnv, env) != ""
}

// scopedEnv returns the per-stream provider env overrides, or nil. Nil-safe so
// callers can pass options that may be nil. These overrides are consulted ahead
// of the OS environment when resolving the API key (pi 8eeaa2bc).
func scopedEnv(opts *StreamOptions) map[string]string {
	if opts == nil {
		return nil
	}
	return opts.Env
}

func withEnvAPIKey(model *Model, opts *StreamOptions) *StreamOptions {
	if opts != nil && hasExplicitAPIKey(opts.APIKey) {
		return opts
	}
	// ANTHROPIC_AUTH_TOKEN is a bearer credential the provider applies as an
	// Authorization header, ahead of the env api keys (pi 24e5cc04). Leave APIKey
	// empty so it never resolves to x-api-key; StreamAnthropic reads the token and
	// emits the bearer. An explicit key (handled above) still wins over it.
	if anthropicAuthTokenActive(model, scopedEnv(opts)) {
		return opts
	}
	key := GetEnvApiKey(model.Provider, scopedEnv(opts))
	// The ambient-auth marker means "authenticated without an explicit key";
	// never inject it as a real API key (pi 850c210b).
	if key == "" || key == ambientAuthMarker {
		return opts
	}
	if opts == nil {
		return &StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: key}}
	}
	clone := *opts
	clone.APIKey = key
	return &clone
}

func withEnvAPIKeySimple(model *Model, opts *SimpleStreamOptions) *SimpleStreamOptions {
	if opts != nil && hasExplicitAPIKey(opts.APIKey) {
		return opts
	}
	var simpleEnv map[string]string
	if opts != nil {
		simpleEnv = opts.Env
	}
	if anthropicAuthTokenActive(model, simpleEnv) {
		return opts
	}
	key := GetEnvApiKey(model.Provider, simpleEnv)
	if key == "" || key == ambientAuthMarker {
		return opts
	}
	if opts == nil {
		return &SimpleStreamOptions{
			StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: key}},
		}
	}
	clone := *opts
	clone.APIKey = key
	return &clone
}

func resolveProvider(api Api) (ApiProvider, error) {
	p, ok := GetApiProvider(api)
	if !ok {
		return ApiProvider{}, fmt.Errorf("No API provider registered for api: %s", api)
	}
	return p, nil
}

// routesThroughModels is compat.ts's Cloudflare branch: a built-in
// cloudflare-* model without a resolved key (an explicit API key or a
// cf-aig-authorization header) streams through compatModels, whose
// Cloudflare auth resolves the key and the account and gateway ids — and
// which observes the request under its telemetry, as Models does. Every
// other model streams straight from its api provider.
func routesThroughModels(model *Model, opts *StreamOptions) bool {
	if !strings.HasPrefix(model.Provider, "cloudflare-") {
		return false
	}
	if opts != nil {
		if hasExplicitAPIKey(opts.APIKey) || opts.Headers["cf-aig-authorization"] != nil {
			return false
		}
	}
	return builtinProviderFor(model) != nil
}

// Stream streams an assistant response using provider-native options.
//
// Divergence from pi (deliberate, G3): pi's stream() throws synchronously when
// no API provider is registered for model.Api (stream.ts resolveApiProvider).
// This Go port instead encodes the failure in the returned stream as a
// terminal "error" event, keeping a single return value and a uniform
// "failures live in the stream" contract for callers. Same applies to
// StreamSimple.
//
// The request is normalized once here (NormalizeContext): providers receive a
// transcript whose leading system message carries req.SystemPrompt and
// req.Tools (pi compat.ts stream, upstream 9e05370b2).
//
// A built-in cloudflare-* model without a resolved key streams through the
// built-in providers' Models runtime instead, as pi's does: it is observed
// under opts.Telemetry and must be a chat model.
func Stream(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream {
	if routesThroughModels(model, opts) {
		var options *ModelsStreamOptions
		if opts != nil {
			options = &ModelsStreamOptions{StreamOptions: *opts}
		}
		return compatModels().Stream(ctx, model, req, options)
	}
	transcript := NormalizeContext(req)
	p, err := resolveProvider(model.Api)
	if err != nil {
		return ErrorStream(model, err)
	}
	return p.Stream(ctx, model, transcript, withEnvAPIKey(model, opts))
}

// Complete runs Stream and waits for the final assistant message.
func Complete(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessage {
	return Stream(ctx, model, req, opts).Result()
}

// StreamSimple streams an assistant response using unified reasoning options.
// See Stream for the deliberate unknown-api divergence from pi (errors are
// encoded in the stream, not thrown), and for the normalization it applies.
func StreamSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream {
	var base *StreamOptions
	if opts != nil {
		base = &opts.StreamOptions
	}
	if routesThroughModels(model, base) {
		var options *ModelsSimpleStreamOptions
		if opts != nil {
			options = &ModelsSimpleStreamOptions{SimpleStreamOptions: *opts}
		}
		return compatModels().StreamSimple(ctx, model, req, options)
	}
	transcript := NormalizeContext(req)
	p, err := resolveProvider(model.Api)
	if err != nil {
		return ErrorStream(model, err)
	}
	return p.StreamSimple(ctx, model, transcript, withEnvAPIKeySimple(model, opts))
}

// CompleteSimple runs StreamSimple and waits for the final assistant message.
func CompleteSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessage {
	return StreamSimple(ctx, model, req, opts).Result()
}

// ErrorStream returns a closed stream carrying a terminal error event. Per the
// stream contract, a failure that pi raises by throwing out of stream setup —
// provider resolution here, an unusable option in a provider's StreamSimple —
// is encoded in the stream rather than returned alongside it.
//
// The message is stamped with the wall clock read just before the stream is
// created, as pi's lazyStream stamps a setup error (3ba22ce17), so the stream
// times it only when both readings fall in the same millisecond.
func ErrorStream(model *Model, err error) *AssistantMessageEventStream {
	return startStreamSetup(model, nil).fail(err)
}

// streamSetup is pi's lazyStream around setup the port runs inline before it
// hands over a provider's stream: the call's start is read and the stream
// created BEFORE setup runs, so a setup failure is stamped at the call's start
// and timed across the setup — a slow credential resolution or OAuth refresh
// that then fails reads as the long failure it was. On success the
// provider's stream is forwarded into this one (pi forwardStream), which
// keeps the inner stream's timing.
//
// observer, when set, sees each event the stream delivers and then its
// result, once.
type streamSetup struct {
	model     *Model
	startedAt int64
	stream    *AssistantMessageEventStream
	observer  streamObserver
}

// streamObserver is a synchronous, read-only view of a streamSetup's
// delivery (pi LazyStreamObserver, upstream 2ec55d808): onEvent sees each
// event and onResult the result — nil when there is none — before a
// consumer can.
//
// pi calls onEvent right after push and onResult right after end, but both
// run before a consumer iterating the stream resumes: an event loop runs the
// observer to completion first. Goroutines give no such ordering, so the
// setup observes first and delivers after — an observer never reads an event
// a consumer already holds, and a consumer never sees a result whose
// telemetry is not yet recorded.
//
// onEvent's error is pi's throwing observer: the event is delivered all the
// same, then the stream fails with the error, as lazyStream's catch ends it.
type streamObserver interface {
	onEvent(event AssistantMessageEvent) error
	onResult(result *AssistantMessage)
}

// startStreamSetup reads the call's start, then creates the stream.
func startStreamSetup(model *Model, observer streamObserver) streamSetup {
	startedAt := nowMillis()
	return streamSetup{model: model, startedAt: startedAt, stream: NewAssistantMessageEventStream(), observer: observer}
}

// fail ends the setup's stream with err (pi createSetupErrorMessage).
func (s streamSetup) fail(err error) *AssistantMessageEventStream {
	msg := NewErrorMessage(s.model.Api, s.model.Provider, s.model.ID, StopError, err.Error())
	msg.Timestamp = s.startedAt
	event := AssistantMessageEvent{Type: EventError, Reason: StopError, Error: msg}
	if s.observer != nil {
		// pi's catch calls onEvent unguarded, so an observer that throws here
		// leaves its stream unended for good. Go ends it regardless.
		_ = s.observer.onEvent(event)
		s.observer.onResult(msg)
	}
	s.stream.Push(event)
	s.stream.End()
	return s.stream
}

// forward delivers inner's events through the setup's stream, then ends it
// with inner's result (pi forwardStream). The observer sees each event before
// it is delivered, and the result before the terminal event and the end:
// pushing the terminal event is what makes the result available.
func (s streamSetup) forward(inner *AssistantMessageEventStream) *AssistantMessageEventStream {
	go func() {
		var terminal *AssistantMessageEvent
		for event := range inner.Events() {
			if s.observer != nil {
				if err := s.observer.onEvent(event); err != nil {
					// pi pushed the event before its observer threw; the
					// failure abandons the provider's stream.
					s.stream.Push(event)
					s.fail(err)
					return
				}
			}
			if event.Type == EventDone || event.Type == EventError {
				// The last event: a done stream drops later pushes.
				terminal = &event
				continue
			}
			s.stream.Push(event)
		}
		result := inner.Result()
		if s.observer != nil {
			s.observer.onResult(result)
		}
		if terminal != nil {
			s.stream.Push(*terminal)
		}
		if result != nil {
			s.stream.End(result)
		} else {
			s.stream.End()
		}
	}()
	return s.stream
}
