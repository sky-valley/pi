package ai

import (
	"context"
	"fmt"
	"sync"
)

// StreamFunction streams an assistant response for a model + request context.
//
// Contract (mirrors pi): once invoked, request/model/runtime failures must be
// encoded in the returned stream (terminal "error" event with stopReason
// "error"/"aborted"), not returned as a Go error.
type StreamFunction func(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream

// StreamSimpleFunction is StreamFunction with unified reasoning options.
type StreamSimpleFunction func(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream

// ApiProvider binds an Api to its stream implementations.
type ApiProvider struct {
	Api          Api
	Stream       StreamFunction
	StreamSimple StreamSimpleFunction
}

type registeredProvider struct {
	provider ApiProvider
	sourceID string
}

var (
	registryMu sync.RWMutex
	registry   = map[Api]registeredProvider{}
)

// RegisterApiProvider registers a provider for its Api. sourceID groups
// providers for bulk unregistration (extensions).
func RegisterApiProvider(p ApiProvider, sourceID ...string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	sid := ""
	if len(sourceID) > 0 {
		sid = sourceID[0]
	}
	api := p.Api
	// Guard against api mismatch, mirroring wrapStream/wrapStreamSimple.
	stream := p.Stream
	if stream != nil {
		orig := stream
		stream = func(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream {
			if model.Api != api {
				panic(fmt.Sprintf("Mismatched api: %s expected %s", model.Api, api))
			}
			return orig(ctx, model, req, opts)
		}
	}
	streamSimple := p.StreamSimple
	if streamSimple != nil {
		orig := streamSimple
		streamSimple = func(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			if model.Api != api {
				panic(fmt.Sprintf("Mismatched api: %s expected %s", model.Api, api))
			}
			return orig(ctx, model, req, opts)
		}
	}
	registry[api] = registeredProvider{
		provider: ApiProvider{Api: api, Stream: stream, StreamSimple: streamSimple},
		sourceID: sid,
	}
}

// GetApiProvider returns the provider registered for api, if any.
func GetApiProvider(api Api) (ApiProvider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	r, ok := registry[api]
	return r.provider, ok
}

// UnregisterApiProviders removes all providers registered with sourceID.
func UnregisterApiProviders(sourceID string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for api, entry := range registry {
		if entry.sourceID == sourceID {
			delete(registry, api)
		}
	}
}

// ClearApiProviders removes all registered providers.
func ClearApiProviders() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[Api]registeredProvider{}
}
