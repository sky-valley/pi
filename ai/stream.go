package ai

import (
	"context"
	"fmt"
	"strings"
)

func hasExplicitAPIKey(key string) bool {
	return strings.TrimSpace(key) != ""
}

func withEnvAPIKey(model *Model, opts *StreamOptions) *StreamOptions {
	if opts != nil && hasExplicitAPIKey(opts.APIKey) {
		return opts
	}
	key := GetEnvApiKey(model.Provider)
	if key == "" {
		return opts
	}
	if opts == nil {
		return &StreamOptions{APIKey: key}
	}
	clone := *opts
	clone.APIKey = key
	return &clone
}

func withEnvAPIKeySimple(model *Model, opts *SimpleStreamOptions) *SimpleStreamOptions {
	if opts != nil && hasExplicitAPIKey(opts.APIKey) {
		return opts
	}
	key := GetEnvApiKey(model.Provider)
	if key == "" {
		return opts
	}
	if opts == nil {
		return &SimpleStreamOptions{StreamOptions: StreamOptions{APIKey: key}}
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

// Stream streams an assistant response using provider-native options.
func Stream(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream {
	p, err := resolveProvider(model.Api)
	if err != nil {
		return errorStream(model, err)
	}
	return p.Stream(ctx, model, req, withEnvAPIKey(model, opts))
}

// Complete runs Stream and waits for the final assistant message.
func Complete(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessage {
	return Stream(ctx, model, req, opts).Result()
}

// StreamSimple streams an assistant response using unified reasoning options.
func StreamSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream {
	p, err := resolveProvider(model.Api)
	if err != nil {
		return errorStream(model, err)
	}
	return p.StreamSimple(ctx, model, req, withEnvAPIKeySimple(model, opts))
}

// CompleteSimple runs StreamSimple and waits for the final assistant message.
func CompleteSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessage {
	return StreamSimple(ctx, model, req, opts).Result()
}

// errorStream returns a closed stream carrying a terminal error event. Per the
// stream contract, provider-resolution failures are encoded in the stream.
func errorStream(model *Model, err error) *AssistantMessageEventStream {
	s := NewAssistantMessageEventStream()
	msg := &AssistantMessage{
		Api:          model.Api,
		Provider:     model.Provider,
		Model:        model.ID,
		StopReason:   StopError,
		ErrorMessage: err.Error(),
		Timestamp:    nowMillis(),
	}
	s.Push(AssistantMessageEvent{Type: EventError, Reason: StopError, Error: msg})
	s.End()
	return s
}
