package ai

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// StreamFunction streams an assistant response for a model + request transcript.
//
// Contract (mirrors pi):
//   - It receives a normalized transcript: the system prompt and tools live in
//     the leading system message, never on a Context (see NormalizeContext).
//   - Once invoked, request/model/runtime failures must be encoded in the
//     returned stream (terminal "error" event with stopReason
//     "error"/"aborted"), not returned as a Go error.
type StreamFunction func(ctx context.Context, model *Model, req TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream

// StreamSimpleFunction is StreamFunction with unified reasoning options.
type StreamSimpleFunction func(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream

// FetchDeferredFunction redeems a DeferredHandle, streaming the response the
// provider has been producing asynchronously. Like StreamFunction it reports
// failures through the returned stream (pi ProviderStreams.fetchDeferred).
type FetchDeferredFunction func(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream

// CancelDeferredFunction drops a deferred response. It has no stream to fail
// through, so it returns an error (pi ProviderStreams.cancelDeferred).
type CancelDeferredFunction func(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredCancelOptions) error

// ApiProvider binds an Api to its stream implementations. FetchDeferred and
// CancelDeferred are nil unless the api supports deferred responses — pi marks
// the corresponding methods optional (upstream 382aa641c).
type ApiProvider struct {
	Api            Api
	Stream         StreamFunction
	StreamSimple   StreamSimpleFunction
	FetchDeferred  FetchDeferredFunction
	CancelDeferred CancelDeferredFunction
}

type registeredProvider struct {
	provider ApiProvider
	sourceID string
	// seq tells registrations apart, as pi compares the registry's provider
	// objects by identity (compat.ts builtinApiProviderInstances).
	seq uint64
}

var (
	registryMu sync.RWMutex
	registry   = map[Api]registeredProvider{}
	// registryOrder keeps the apis in registration order, as pi's registry
	// Map does: a provider registered again keeps its place, and one removed
	// and registered again comes last.
	registryOrder []Api
	registrySeq   uint64
)

// registrations returns each api's current registration (registeredProvider.seq).
func registrations() map[Api]uint64 {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make(map[Api]uint64, len(registry))
	for api, entry := range registry {
		out[api] = entry.seq
	}
	return out
}

// registration returns api's current registration, if it has one.
func registration(api Api) (uint64, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	entry, ok := registry[api]
	return entry.seq, ok
}

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
		stream = func(ctx context.Context, model *Model, req TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
			if model.Api != api {
				panic(fmt.Sprintf("Mismatched api: %s expected %s", model.Api, api))
			}
			return orig(ctx, model, req, opts)
		}
	}
	streamSimple := p.StreamSimple
	if streamSimple != nil {
		orig := streamSimple
		streamSimple = func(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			if model.Api != api {
				panic(fmt.Sprintf("Mismatched api: %s expected %s", model.Api, api))
			}
			return orig(ctx, model, req, opts)
		}
	}
	fetchDeferred := p.FetchDeferred
	if fetchDeferred != nil {
		orig := fetchDeferred
		fetchDeferred = func(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
			if model.Api != api {
				panic(fmt.Sprintf("Mismatched api: %s expected %s", model.Api, api))
			}
			return orig(ctx, model, handle, opts)
		}
	}
	cancelDeferred := p.CancelDeferred
	if cancelDeferred != nil {
		orig := cancelDeferred
		cancelDeferred = func(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredCancelOptions) error {
			if model.Api != api {
				panic(fmt.Sprintf("Mismatched api: %s expected %s", model.Api, api))
			}
			return orig(ctx, model, handle, opts)
		}
	}
	registrySeq++
	if _, ok := registry[api]; !ok {
		registryOrder = append(registryOrder, api)
	}
	registry[api] = registeredProvider{
		provider: ApiProvider{
			Api:            api,
			Stream:         stream,
			StreamSimple:   streamSimple,
			FetchDeferred:  fetchDeferred,
			CancelDeferred: cancelDeferred,
		},
		sourceID: sid,
		seq:      registrySeq,
	}
}

// GetApiProvider returns the provider registered for api, if any.
func GetApiProvider(api Api) (ApiProvider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	r, ok := registry[api]
	return r.provider, ok
}

// GetApiProviders returns all registered providers in registration order
// (compat.ts getApiProviders, the registry Map's insertion order).
func GetApiProviders() []ApiProvider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]ApiProvider, 0, len(registryOrder))
	for _, api := range registryOrder {
		out = append(out, registry[api].provider)
	}
	return out
}

// UnregisterApiProviders removes all providers registered with sourceID.
func UnregisterApiProviders(sourceID string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registryOrder = slices.DeleteFunc(registryOrder, func(api Api) bool {
		if registry[api].sourceID != sourceID {
			return false
		}
		delete(registry, api)
		return true
	})
}

// ClearApiProviders removes all registered providers.
func ClearApiProviders() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[Api]registeredProvider{}
	registryOrder = nil
}
