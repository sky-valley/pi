package ai

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"

	"github.com/sky-valley/pi/internal/jstext"
)

// Models runtime ported from pi packages/ai/src/models.ts (732bb161; facade
// merge ff28097a): the Provider/Models object-model, createModels/
// createProvider, and auth application. The pre-existing global free functions
// (Stream/GetModel/GetModels/GetProviders/GetEnvApiKey, models.go + stream.go)
// are the compat surface — pi's "@earendil-works/pi-ai/compat" — and stay
// available.
//
// pi defers provider resolution into the returned stream via lazyStream
// (async). The Go port keeps its existing contract (G3, stream.go): resolution
// runs synchronously and failures are encoded as a terminal stream error, so
// applyAuth runs inline and errors flow through errorStream.

// ProviderStreams binds an API's stream implementations (pi ProviderStreams).
// FetchDeferred and CancelDeferred are nil unless the api supports deferred
// responses; pi marks the corresponding methods optional (upstream 382aa641c).
type ProviderStreams struct {
	Stream         StreamFunction
	StreamSimple   StreamSimpleFunction
	FetchDeferred  FetchDeferredFunction
	CancelDeferred CancelDeferredFunction
}

// ModelsPublication is one atomic catalog publication (pi ModelsPublication,
// upstream fed6009c). Persistence policy stays provider-owned; Update runs
// synchronously right after the selected persistence mutation, so a provider's
// in-memory catalog can never disagree with what was just stored.
type ModelsPublication struct {
	// Persist writes this entry for the provider. Nil leaves storage unchanged
	// unless DeletePersisted is set (pi: persist omitted / an entry / null).
	Persist *ModelsStoreEntry
	// DeletePersisted deletes the provider's stored entry (pi persist: null).
	// Ignored when Persist is non-nil.
	DeletePersisted bool
	// Update applies provider-private in-memory catalog state. It runs
	// synchronously under the publication lock, only after the persistence
	// mutation succeeded and only while the refresh is still current.
	Update func()
}

// RefreshModelsContext is the input to a dynamic provider's model refresh
// (pi RefreshModelsContext). Cancellation travels on the context.Context
// passed to RefreshModels (pi's signal, which fed6009c made required — the
// Models runtime always supplies a live context).
type RefreshModelsContext struct {
	// Credential is the effective configured credential. OAuth credentials are
	// refreshed before network access.
	Credential *Credential
	// Stored is an immutable provider-scoped catalog snapshot captured before
	// this refresh phase; nil when nothing is stored (pi's `stored`, which
	// replaced the mutable per-provider store handle).
	Stored *ModelsStoreEntry
	// Publish is generation-checked publication. It reports false when the
	// refresh has been superseded by a newer one — the caller must then stop —
	// and an error when publication was cancelled or storage failed.
	Publish func(publication ModelsPublication) (bool, error)
	// AllowNetwork is false during offline/cache-only initialization.
	AllowNetwork bool
	// Force bypasses provider freshness checks and fetches immediately when
	// network access is allowed (pi 97f9978f).
	Force bool
	// BaseURL is the base URL of the registered provider, including
	// configuration overrides that wrappers apply outside the provider
	// receiving this context (upstream ee409a85f). Empty is pi's undefined.
	BaseURL string
}

// publish routes through Publish, reporting a resolvable error when a caller
// built the context by hand instead of going through Models.Refresh.
func (c RefreshModelsContext) publish(publication ModelsPublication) (bool, error) {
	if c.Publish == nil {
		return false, errors.New(
			"RefreshModelsContext.Publish is nil: obtain the refresh context from Models.Refresh, " +
				"which supplies generation-checked publication")
	}
	return c.Publish(publication)
}

// Provider is the concrete runtime unit (pi Provider). It owns id/name/base
// metadata, auth, model listing, and the operations its models support.
// Listing every model type is the optional AllModelsLister capability.
type Provider interface {
	ID() string
	Name() string
	BaseURL() string
	Headers() ProviderHeaders

	// Auth reports the provider's auth semantics. At least one of
	// APIKey/OAuth is set, even for ambient/keyless providers.
	Auth() ProviderAuth

	// GetModels returns the current known chat models: the static baseline
	// plus the last-known dynamic overlay. Must not panic.
	GetModels() []*Model

	// DynamicModels reports whether the provider has a dynamic model source
	// (pi: refreshModels !== undefined). Models.Refresh skips providers
	// without one.
	DynamicModels() bool

	// RefreshModels restores req.Stored and optionally fetches a newer list
	// using the effective credential (dynamic providers only; a no-op
	// otherwise). Implementations must retain their previous list on failure,
	// publish persistence and in-memory state through req.Publish, and honor
	// ctx for blocking work.
	RefreshModels(ctx context.Context, req RefreshModelsContext) error

	// FilterModels applies provider policy for credential-specific chat model
	// availability (pi Provider.filterModels; identity when the provider has
	// none). GetModels remains the complete synchronous chat catalog;
	// Models.GetAvailable applies this filter after confirming that provider
	// auth is configured.
	FilterModels(models []*Model, credential *Credential) []*Model

	// Stream and StreamSimple take a normalized transcript: Models normalizes the
	// caller's Context before dispatching here (pi Provider.stream, upstream
	// 9e05370b2).
	Stream(ctx context.Context, model *Model, req TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream
	StreamSimple(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream
}

// deferredUnsupportedHint tells the caller how to proceed when a provider has
// no deferred (background-mode) support, rather than only naming what is
// missing.
const deferredUnsupportedHint = "; use Stream/Complete for a synchronous response"

// DeferredFetcher is the optional Provider capability of redeeming a
// DeferredHandle (pi's optional Provider.fetchDeferred). A Provider announces
// it by implementing this interface, so callers detect support with a type
// assertion where pi checks whether the method is present. CreateProvider
// exposes it only when some underlying api implements it.
type DeferredFetcher interface {
	FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream
}

// DeferredCanceller is the optional Provider capability of dropping a deferred
// response (pi's optional Provider.cancelDeferred). It is independent of
// DeferredFetcher: an api may implement either alone.
type DeferredCanceller interface {
	CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredCancelOptions) error
}

// AllModelsLister is the optional Provider capability of listing models of
// every type (pi's optional Provider.getAllModels, upstream a328aa89a), with
// GetModels' contract. A provider with only chat models may leave it out;
// Models then reads GetModels in its place. Model ids are unique within each
// type; one upstream model may have separate entries for different operations.
type AllModelsLister interface {
	GetAllModels() []*Model
}

// AllModelsFilterer is the optional credential-specific availability policy
// across every model type (pi's optional Provider.filterAllModels). Without it,
// Models.GetAllAvailable filters the chat models through FilterModels and keeps
// every other model.
//
// That fallback differs from pi's in one case. pi filters chat models only
// when the provider HAS a filterModels and otherwise keeps getAllModels()
// whole; every Go Provider has FilterModels, so the Go fallback always
// filters. Take a handwritten provider without this interface whose
// FilterModels is the identity — the Go stand-in for pi's absent filterModels
// — and whose GetAllModels lists a chat model its GetModels does not: pi keeps
// that model and Go drops it. A CreateProvider provider is unaffected: it
// implements this interface with pi's own fallback.
type AllModelsFilterer interface {
	FilterAllModels(models []*Model, credential *Credential) []*Model
}

// CreateProviderOptions are the parts createProvider assembles into a Provider.
// API streams all chat models; otherwise APIByApi dispatches on model.Api (a
// model whose api has no entry produces a stream error). CreateProvider
// requires at least one implementation. FetchModels is nil for static
// providers.
type CreateProviderOptions struct {
	ID      string
	Name    string
	BaseURL string
	Headers ProviderHeaders
	Auth    ProviderAuth
	// Models is the static baseline model list, of every type (empty for
	// purely dynamic providers). A model without a Type is a chat model.
	Models []*Model
	// FetchModels fetches a dynamic model overlay of every type (pi
	// fetchModels). CreateProvider restores it from the snapshot, drops models
	// of types this version does not know, and publishes the fetched list
	// transactionally.
	FetchModels func(ctx context.Context, req RefreshModelsContext) ([]*Model, error)
	// FilterModels is the optional credential-specific chat model availability
	// policy (see Provider.FilterModels).
	FilterModels func(models []*Model, credential *Credential) []*Model
	// FilterAllModels is the optional credential-specific availability policy
	// across every model type (see AllModelsFilterer).
	FilterAllModels func(models []*Model, credential *Credential) []*Model
	// API is the single chat implementation for all chat models. One whose
	// functions are all nil is no implementation, as pi's `api: {}` is none.
	API *ProviderStreams
	// APIByApi holds chat implementations keyed by model.Api, for mixed-API
	// providers. Every entry counts as an implementation.
	APIByApi map[Api]ProviderStreams
}

// empty reports whether no stream function is set.
func (s *ProviderStreams) empty() bool {
	return s.Stream == nil && s.StreamSimple == nil && s.FetchDeferred == nil && s.CancelDeferred == nil
}

type providerImpl struct {
	id, name, baseURL string
	headers           ProviderHeaders
	auth              ProviderAuth
	single            *ProviderStreams
	byAPI             map[Api]ProviderStreams
	fetchFn           func(ctx context.Context, req RefreshModelsContext) ([]*Model, error)
	filterFn          func(models []*Model, credential *Credential) []*Model
	filterAllFn       func(models []*Model, credential *Credential) []*Model

	mu       sync.Mutex
	baseline []*Model
	dynamic  []*Model
}

// CreateProvider builds a Provider from parts (pi createProvider). Built-in
// factories and custom-model providers both go through this.
//
// It panics, with pi's message, when the input has no implementation at all:
// no API with a function set and no APIByApi entry. pi throws there (upstream
// a328aa89a), and a provider that can serve none of its models is a
// programming error, as a mismatched api is to RegisterApiProvider. pi's
// message also names the "images" and "classifiers" options, which arrive with
// the non-chat model operations.
func CreateProvider(input CreateProviderOptions) Provider {
	single := input.API
	if single != nil && single.empty() {
		single = nil
	}
	if single == nil && len(input.APIByApi) == 0 {
		panic("Provider " + input.ID + `: at least one of "api", "images", or "classifiers" is required.`)
	}
	name := input.Name
	if name == "" {
		name = input.ID
	}
	p := &providerImpl{
		id:          input.ID,
		name:        name,
		baseURL:     input.BaseURL,
		headers:     input.Headers,
		auth:        input.Auth,
		single:      single,
		byAPI:       input.APIByApi,
		fetchFn:     input.FetchModels,
		filterFn:    input.FilterModels,
		filterAllFn: input.FilterAllModels,
		baseline:    input.Models,
	}

	// The deferred capabilities are announced only when some underlying api
	// implements them, so a type assertion answers the question pi answers by
	// checking whether the optional method is present.
	canFetch, canCancel := false, false
	for _, s := range p.allStreams() {
		canFetch = canFetch || s.FetchDeferred != nil
		canCancel = canCancel || s.CancelDeferred != nil
	}
	switch {
	case canFetch && canCancel:
		return deferredProvider{p}
	case canFetch:
		return deferredFetchProvider{p}
	case canCancel:
		return deferredCancelProvider{p}
	}
	return p
}

// deferredFetchProvider, deferredCancelProvider and deferredProvider expose the
// deferred capabilities a provider's apis actually implement. Each embeds the
// same providerImpl, so only the announced interfaces differ.
type deferredFetchProvider struct{ *providerImpl }

func (p deferredFetchProvider) FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
	return p.fetchDeferred(ctx, model, handle, opts)
}

type deferredCancelProvider struct{ *providerImpl }

func (p deferredCancelProvider) CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredCancelOptions) error {
	return p.cancelDeferred(ctx, model, handle, opts)
}

type deferredProvider struct{ *providerImpl }

func (p deferredProvider) FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
	return p.fetchDeferred(ctx, model, handle, opts)
}

func (p deferredProvider) CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredCancelOptions) error {
	return p.cancelDeferred(ctx, model, handle, opts)
}

func (p *providerImpl) ID() string               { return p.id }
func (p *providerImpl) Name() string             { return p.name }
func (p *providerImpl) BaseURL() string          { return p.baseURL }
func (p *providerImpl) Headers() ProviderHeaders { return p.headers }
func (p *providerImpl) Auth() ProviderAuth       { return p.auth }
func (p *providerImpl) DynamicModels() bool      { return p.fetchFn != nil }

// allModels merges the static baseline with the dynamic overlay, as a new
// slice: a dynamic model replaces the baseline entry of the same type and id,
// otherwise it is appended (pi createProvider currentModels). Matching on the
// type too keeps a chat model and an image model that share an id apart.
func (p *providerImpl) allModels() []*Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	merged := make([]*Model, len(p.baseline), len(p.baseline)+len(p.dynamic))
	copy(merged, p.baseline)
	for _, model := range p.dynamic {
		i := slices.IndexFunc(merged, func(entry *Model) bool {
			return GetModelType(entry) == GetModelType(model) && entry.ID == model.ID
		})
		if i >= 0 {
			merged[i] = model
		} else {
			merged = append(merged, model)
		}
	}
	return merged
}

// GetModels returns the chat models of the merged catalog (pi createProvider
// getModels).
func (p *providerImpl) GetModels() []*Model {
	all := p.allModels()
	chat := make([]*Model, 0, len(all))
	for _, model := range all {
		if IsModelType(model, ModelTypeChat) {
			chat = append(chat, model)
		}
	}
	return chat
}

// GetAllModels returns the merged catalog, every type included (pi
// createProvider getAllModels).
func (p *providerImpl) GetAllModels() []*Model { return p.allModels() }

func (p *providerImpl) FilterModels(models []*Model, credential *Credential) []*Model {
	if p.filterFn == nil {
		return models
	}
	return p.filterFn(models, credential)
}

// FilterAllModels applies the provider's all-type availability policy. Without
// one it is pi's Models.getAllAvailable fallback, which inside createProvider
// is exactly this: no chat filter keeps every model; a chat filter keeps every
// non-chat model plus the chat models the filter keeps from GetModels.
func (p *providerImpl) FilterAllModels(models []*Model, credential *Credential) []*Model {
	if p.filterAllFn != nil {
		return p.filterAllFn(models, credential)
	}
	if p.filterFn == nil {
		return models
	}
	return keepAvailableChatModels(models, p.filterFn(p.GetModels(), credential))
}

// keepAvailableChatModels keeps every non-chat model, and the chat models whose
// id is among availableChat (pi getAllAvailable's availableChatIds).
func keepAvailableChatModels(models, availableChat []*Model) []*Model {
	ids := make(map[string]bool, len(availableChat))
	for _, model := range availableChat {
		ids[model.ID] = true
	}
	out := make([]*Model, 0, len(models))
	for _, model := range models {
		if !IsModelType(model, ModelTypeChat) || ids[model.ID] {
			out = append(out, model)
		}
	}
	return out
}

// setDynamic replaces the dynamic overlay.
func (p *providerImpl) setDynamic(models []*Model) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dynamic = models
}

// RefreshModels restores the stored dynamic overlay and, when the network is
// allowed, fetches a fresh one and publishes it (pi createProvider's
// refreshModels after fed6009c). Both the restore and the fetched list go
// through req.Publish, so the in-memory overlay and the persisted catalog move
// together and a superseded refresh stops instead of clobbering a newer one.
// pi's inflightRefresh dedup is gone upstream — a newer refresh now supersedes
// an older one rather than joining it. Static providers (nil fetchFn) are a
// no-op.
func (p *providerImpl) RefreshModels(ctx context.Context, req RefreshModelsContext) error {
	if p.fetchFn == nil {
		return nil
	}

	if req.Stored != nil {
		restored := make([]*Model, 0, len(req.Stored.Models))
		for _, model := range req.Stored.Models {
			if model.Provider == p.id {
				restored = append(restored, model)
			}
		}
		published, err := req.publish(ModelsPublication{Update: func() { p.setDynamic(restored) }})
		if err != nil {
			return err
		}
		if !published {
			return nil
		}
	}

	if !req.AllowNetwork || ctx.Err() != nil {
		return nil
	}
	fetched, err := p.fetchFn(ctx, req)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	// A fetched model of a type this version does not know is dropped rather
	// than failing the refresh, before it is persisted or published.
	refreshed := knownTypeModels(fetched)
	_, err = req.publish(ModelsPublication{
		Persist: &ModelsStoreEntry{Models: refreshed, CheckedAt: nowMillis()},
		Update:  func() { p.setDynamic(refreshed) },
	})
	return err
}

// allStreams returns every api implementation the provider was built with.
func (p *providerImpl) allStreams() []ProviderStreams {
	if p.single != nil {
		return []ProviderStreams{*p.single}
	}
	streams := make([]ProviderStreams, 0, len(p.byAPI))
	for _, s := range p.byAPI {
		streams = append(streams, s)
	}
	return streams
}

// streamsFor selects the ProviderStreams for a model's api.
func (p *providerImpl) streamsFor(model *Model) (ProviderStreams, bool) {
	if p.single != nil {
		return *p.single, true
	}
	s, ok := p.byAPI[model.Api]
	return s, ok
}

func (p *providerImpl) Stream(ctx context.Context, model *Model, req TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
	s, ok := p.streamsFor(model)
	if !ok || s.Stream == nil {
		return ErrorStream(model, newModelsError(ErrStream, "Provider "+p.id+" has no API implementation for \""+model.Api+"\"", nil))
	}
	return s.Stream(ctx, model, req, opts)
}

func (p *providerImpl) StreamSimple(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
	s, ok := p.streamsFor(model)
	if !ok || s.StreamSimple == nil {
		return ErrorStream(model, newModelsError(ErrStream, "Provider "+p.id+" has no API implementation for \""+model.Api+"\"", nil))
	}
	return s.StreamSimple(ctx, model, req, opts)
}

// fetchDeferred backs the announced DeferredFetcher. A provider announces the
// capability when any of its apis has it, so the model's own api may still not.
func (p *providerImpl) fetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
	s, ok := p.streamsFor(model)
	if !ok || s.FetchDeferred == nil {
		return ErrorStream(model, newModelsError(ErrProvider,
			"Provider "+p.id+" does not support deferred responses for \""+model.Api+"\""+
				deferredUnsupportedHint, nil))
	}
	return s.FetchDeferred(ctx, model, handle, opts)
}

// cancelDeferred backs the announced DeferredCanceller.
func (p *providerImpl) cancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *DeferredCancelOptions) error {
	s, ok := p.streamsFor(model)
	if !ok || s.CancelDeferred == nil {
		return newModelsError(ErrProvider,
			"Provider "+p.id+" cannot cancel deferred responses for \""+model.Api+"\"", nil)
	}
	return s.CancelDeferred(ctx, model, handle, opts)
}

// ModelsRefreshOptions configure Models.Refresh (pi ModelsRefreshOptions).
// Cancellation travels on the context passed to Refresh (pi's signal).
type ModelsRefreshOptions struct {
	// AllowNetwork gates network fetches; nil defaults to true (pi
	// allowNetwork ?? true).
	AllowNetwork *bool
	// Providers restricts the refresh to these provider ids; nil refreshes
	// every dynamic provider. Unknown and static ids are ignored (fed6009c).
	Providers []string
	// Force bypasses provider freshness checks and fetches immediately when
	// network access is allowed (pi 97f9978f).
	Force bool
}

// ModelsRefreshResult reports a refresh sweep (pi ModelsRefreshResult).
// Provider errors and cancellation are returned without failing the sweep.
type ModelsRefreshResult struct {
	Aborted bool
	Errors  map[string]error
}

// ModelsRequestTransforms are Models-only request hooks (pi
// ModelsRequestTransforms); they are stripped before provider dispatch.
type ModelsRequestTransforms struct {
	// TransformHeaders transforms the fully assembled model/auth/request
	// headers before provider dispatch. Deletion markers (nil values) are part
	// of the value it sees and returns: a transform that rebuilds the map must
	// pass them through, or the suppression they encode is lost. The map it
	// receives aliases the *string values of the model's own Headers, so a
	// transform must replace a pointer rather than write through one (see
	// ProviderHeaders).
	TransformHeaders func(headers ProviderHeaders) (ProviderHeaders, error)
}

// ModelsStreamOptions are Models.Stream/Complete options: provider stream
// options plus Models-only transforms (pi ModelsApiStreamOptions).
type ModelsStreamOptions struct {
	StreamOptions
	ModelsRequestTransforms
}

// ModelsSimpleStreamOptions are Models.StreamSimple/CompleteSimple options
// (pi ModelsSimpleStreamOptions).
type ModelsSimpleStreamOptions struct {
	SimpleStreamOptions
	ModelsRequestTransforms
}

// ModelsDeferredFetchOptions are Models.FetchDeferred options
// (pi ModelsDeferredFetchOptions).
type ModelsDeferredFetchOptions struct {
	DeferredFetchOptions
	ModelsRequestTransforms
}

// ModelsDeferredCancelOptions are Models.CancelDeferred options
// (pi ModelsDeferredCancelOptions).
type ModelsDeferredCancelOptions struct {
	DeferredCancelOptions
	ModelsRequestTransforms
}

// Models is the runtime collection of providers plus auth application and
// stream convenience (pi Models). Providers own stream behavior; Models
// resolves auth and delegates each request to the provider that owns the model.
//
// Concurrency: provider registration (SetProvider/DeleteProvider/
// ClearProviders) supersedes any in-flight refresh for the affected provider,
// so a catalog published by work that started against an older provider set is
// dropped rather than applied (pi fed6009c).
type Models interface {
	GetProviders() []Provider
	GetProvider(id string) Provider

	// GetModels returns last-known chat models for one provider, or for all
	// when provider is "" (pi getModels(provider?)). Best-effort. It reads
	// each provider's GetModels and never its GetAllModels.
	GetModels(provider string) []*Model
	// GetModel looks up a chat model by id (pi getModel).
	GetModel(provider, id string) *Model

	// GetModelsOfType returns last-known models of one type for one provider,
	// or for all when provider is "" (pi getModelsOfType).
	GetModelsOfType(t ModelType, provider string) []*Model
	// GetModelOfType looks up a model of one type by id (pi getModelOfType).
	GetModelOfType(t ModelType, provider, id string) *Model
	// GetAllModels returns last-known models of every type for one provider,
	// or for all when provider is "" (pi getAllModels). A provider that does
	// not implement AllModelsLister is read through GetModels.
	GetAllModels(provider string) []*Model

	// Refresh refreshes the selected configured dynamic providers concurrently,
	// or every one when Providers is unset (pi refresh(options?)). Provider
	// errors and cancellation are returned in the result without failing the
	// sweep; static, unknown, and unconfigured providers are skipped.
	Refresh(ctx context.Context, options *ModelsRefreshOptions) ModelsRefreshResult

	// CheckAuth checks whether a provider has complete auth configuration
	// without refreshing OAuth (pi checkAuth). (nil, nil) when the provider is
	// unknown or unconfigured.
	CheckAuth(ctx context.Context, providerID string) (*AuthCheck, error)

	// GetAvailable returns chat models whose providers have complete auth
	// configuration, for one provider or all when providerID is "" (pi
	// getAvailable(providerId?)).
	GetAvailable(ctx context.Context, providerID string) ([]*Model, error)

	// GetAvailableOfType returns models of one type whose providers have
	// complete auth configuration (pi getAvailableOfType).
	GetAvailableOfType(ctx context.Context, t ModelType, providerID string) ([]*Model, error)

	// GetAllAvailable returns models of every type whose providers have
	// complete auth configuration (pi getAllAvailable). Each provider's
	// AllModelsFilterer decides; without one, FilterModels decides the chat
	// models and every other model is kept.
	GetAllAvailable(ctx context.Context, providerID string) ([]*Model, error)

	// GetAuth resolves request auth for a model: provider auth plus the
	// model's static headers (pi getAuth(model, overrides?)). Returns
	// (nil, nil) when the provider is unknown or unconfigured; a ModelsError
	// on refresh/store failure.
	GetAuth(ctx context.Context, model *Model, overrides *AuthResolutionOverrides) (*AuthResult, error)

	// GetProviderAuth resolves provider-scoped auth by provider id (pi's
	// getAuth(providerId, overrides?) overload).
	GetProviderAuth(ctx context.Context, providerID string, overrides *AuthResolutionOverrides) (*AuthResult, error)

	// Login runs a provider-owned login flow and persists its returned
	// credential (pi login). ctx cancels the flow; a mutation cancelled before
	// it reached the store never runs. options, which may be nil, reaches an
	// OAuth login flow (upstream 02eed88fd).
	Login(ctx context.Context, providerID string, authType CredentialKind, interaction AuthInteraction, options *LoginOptions) (*Credential, error)

	// Logout removes the stored credential for a provider (pi logout).
	Logout(ctx context.Context, providerID string) error

	Stream(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessageEventStream
	Complete(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessage
	StreamSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessageEventStream
	CompleteSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessage

	// StreamDeferred redeems a DeferredHandle and returns the events the
	// redemption produces (pi streamDeferred, upstream b37834b69). It is the
	// streaming half of the pair FetchDeferred is defined over, for a caller —
	// a durable runner polling a long submission — that wants to render
	// progress rather than only the outcome.
	StreamDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessageEventStream

	// FetchDeferred redeems a DeferredHandle through the provider that owns the
	// model (pi fetchDeferred). Like Complete it returns the final message, and
	// resolution failures — an unknown provider, one that cannot fetch deferred
	// responses — arrive as an error message rather than a Go error. A response
	// the provider is still producing comes back with StopDeferred and the
	// handle to retry with.
	FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessage

	// CancelDeferred drops a deferred response (pi cancelDeferred). It has no
	// message to carry a failure, so it returns one.
	CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredCancelOptions) error
}

// MutableModels adds provider mutation (pi MutableModels).
type MutableModels interface {
	Models
	SetProvider(provider Provider)
	DeleteProvider(id string)
	ClearProviders()
}

// CreateModelsOptions configure a Models collection (pi CreateModelsOptions).
type CreateModelsOptions struct {
	Credentials CredentialStore
	ModelsStore ModelsStore
	AuthContext AuthContext
}

type modelsImpl struct {
	mu          sync.RWMutex
	providers   map[string]Provider
	order       []string // insertion order, mirroring pi's Map iteration
	credentials CredentialStore
	modelsStore ModelsStore
	authContext AuthContext

	// refreshMu guards the per-provider refresh generation counters and the
	// cancel funcs of the refreshes currently in flight (pi's
	// refreshGenerations / refreshControllers).
	refreshMu     sync.Mutex
	refreshGens   map[string]uint64
	refreshCancel map[string]*providerRefresh
	// publishQueue serializes publication per provider (pi's publicationChains).
	publishQueue *keyedLock
}

// CreateModels builds an empty Models collection (pi createModels). Defaults:
// an InMemoryCredentialStore, an InMemoryModelsStore, and the OS-backed
// AuthContext.
func CreateModels(options *CreateModelsOptions) MutableModels {
	var creds CredentialStore = NewInMemoryCredentialStore()
	var store ModelsStore = NewInMemoryModelsStore()
	var ac AuthContext = DefaultProviderAuthContext()
	if options != nil {
		if options.Credentials != nil {
			creds = options.Credentials
		}
		if options.ModelsStore != nil {
			store = options.ModelsStore
		}
		if options.AuthContext != nil {
			ac = options.AuthContext
		}
	}
	return &modelsImpl{
		providers:     map[string]Provider{},
		credentials:   creds,
		modelsStore:   store,
		authContext:   ac,
		refreshGens:   map[string]uint64{},
		refreshCancel: map[string]*providerRefresh{},
		publishQueue:  newKeyedLock(),
	}
}

func (m *modelsImpl) SetProvider(provider Provider) {
	m.supersedeProviderRefresh(provider.ID())
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.providers[provider.ID()]; !exists {
		m.order = append(m.order, provider.ID())
	}
	m.providers[provider.ID()] = provider
}

func (m *modelsImpl) DeleteProvider(id string) {
	m.supersedeProviderRefresh(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.providers[id]; !exists {
		return
	}
	delete(m.providers, id)
	for i, pid := range m.order {
		if pid == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// ClearProviders drops every provider and supersedes every refresh in flight.
//
// The sweep is deliberately two-phase, and the second phase is not redundant:
// the first covers the providers still in the collection, the second covers
// refreshes whose provider is already gone from it (pi's clearProviders unions
// the provider ids with the live controller keys). Neither phase alone sees
// both sets.
func (m *modelsImpl) ClearProviders() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.order))
	ids = append(ids, m.order...)
	m.mu.RUnlock()
	for _, id := range ids {
		m.supersedeProviderRefresh(id)
	}
	m.supersedeAllProviderRefreshes()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers = map[string]Provider{}
	m.order = nil
}

// providerRefresh is one in-flight provider refresh, identified by pointer (pi
// compares AbortController identity).
type providerRefresh struct{ cancel context.CancelFunc }

// supersedeProviderRefresh bumps a provider's refresh generation and cancels
// the refresh currently in flight for it, returning the new generation (pi
// supersedeProviderRefresh). Publications carrying an older generation are
// dropped.
func (m *modelsImpl) supersedeProviderRefresh(providerID string) uint64 {
	m.refreshMu.Lock()
	generation := m.refreshGens[providerID] + 1
	m.refreshGens[providerID] = generation
	previous := m.refreshCancel[providerID]
	delete(m.refreshCancel, providerID)
	m.refreshMu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	return generation
}

// supersedeAllProviderRefreshes supersedes every refresh still in flight,
// including ones whose provider is already gone from the collection (pi
// clearProviders unions the provider ids with the live controller keys).
func (m *modelsImpl) supersedeAllProviderRefreshes() {
	m.refreshMu.Lock()
	ids := make([]string, 0, len(m.refreshCancel))
	for id := range m.refreshCancel {
		ids = append(ids, id)
	}
	m.refreshMu.Unlock()
	for _, id := range ids {
		m.supersedeProviderRefresh(id)
	}
}

// beginProviderRefresh supersedes any refresh in flight for the provider and
// registers a fresh generation plus its cancellable context (pi
// beginProviderRefresh). The returned cancel must be passed to
// endProviderRefresh.
//
// The three steps below are not atomic with respect to each other, and that
// window is exactly why the generation counter is load-bearing rather than a
// second line of defence behind cancellation: supersedeProviderRefresh bumps the
// generation and clears the cancel entry, then the context is created, and only
// then is the new entry registered. A ClearProviders landing in that gap finds
// no entry to cancel — its supersede loop sees previous == nil — so this
// refresh keeps a LIVE context and runs to completion. Nothing stops its
// publication except the generation check in publishProviderModels, which sees
// the generation ClearProviders bumped and drops it.
func (m *modelsImpl) beginProviderRefresh(ctx context.Context, providerID string) (uint64, context.Context, *providerRefresh) {
	generation := m.supersedeProviderRefresh(providerID)
	refreshCtx, cancel := context.WithCancel(ctx)
	entry := &providerRefresh{cancel: cancel}
	m.refreshMu.Lock()
	m.refreshCancel[providerID] = entry
	m.refreshMu.Unlock()
	return generation, refreshCtx, entry
}

// endProviderRefresh releases a finished refresh's context, deregistering it
// only if it is still the current one.
func (m *modelsImpl) endProviderRefresh(providerID string, entry *providerRefresh) {
	m.refreshMu.Lock()
	if m.refreshCancel[providerID] == entry {
		delete(m.refreshCancel, providerID)
	}
	m.refreshMu.Unlock()
	entry.cancel()
}

// currentRefreshGeneration reports a provider's latest refresh generation.
func (m *modelsImpl) currentRefreshGeneration(providerID string) uint64 {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.refreshGens[providerID]
}

func (m *modelsImpl) GetProviders() []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Provider, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.providers[id])
	}
	return out
}

func (m *modelsImpl) GetProvider(id string) Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providers[id]
}

func (m *modelsImpl) GetModels(provider string) []*Model {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if provider != "" {
		p := m.providers[provider]
		if p == nil {
			return nil
		}
		return p.GetModels()
	}
	var out []*Model
	for _, id := range m.order {
		out = append(out, m.providers[id].GetModels()...)
	}
	return out
}

func (m *modelsImpl) GetModel(provider, id string) *Model {
	for _, model := range m.GetModels(provider) {
		if model.ID == id {
			return model
		}
	}
	return nil
}

// allModelsOf reads a provider's models of every type: GetAllModels when it
// implements AllModelsLister, and GetModels otherwise. pi reads
// `entry.getAllModels?.() ?? entry.getModels()`, whose `??` also falls back
// when a present getAllModels returns undefined or null. A []*Model has no such
// value besides nil, and a nil slice is Go's empty list — pi's [] — so an
// implemented GetAllModels always stands, even when it lists nothing.
func allModelsOf(p Provider) []*Model {
	if lister, ok := p.(AllModelsLister); ok {
		return lister.GetAllModels()
	}
	return p.GetModels()
}

func (m *modelsImpl) GetAllModels(provider string) []*Model {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if provider != "" {
		p := m.providers[provider]
		if p == nil {
			return nil
		}
		return allModelsOf(p)
	}
	var out []*Model
	for _, id := range m.order {
		out = append(out, allModelsOf(m.providers[id])...)
	}
	return out
}

func (m *modelsImpl) GetModelsOfType(t ModelType, provider string) []*Model {
	var out []*Model
	for _, model := range m.GetAllModels(provider) {
		if IsModelType(model, t) {
			out = append(out, model)
		}
	}
	return out
}

func (m *modelsImpl) GetModelOfType(t ModelType, provider, id string) *Model {
	for _, model := range m.GetModelsOfType(t, provider) {
		if model.ID == id {
			return model
		}
	}
	return nil
}

// publishProviderModels performs one generation-checked publication (pi
// publishProviderModels). Publications for a provider are serialized; a
// publication whose refresh has been superseded reports false and mutates
// nothing, and Update runs only after the persistence mutation succeeded and
// only while the refresh is still current.
func (m *modelsImpl) publishProviderModels(
	ctx context.Context,
	providerID string,
	generation uint64,
	publication ModelsPublication,
) (bool, error) {
	if err := m.publishQueue.lock(ctx, providerID); err != nil {
		return false, err
	}
	defer m.publishQueue.unlock(providerID)

	if err := ctx.Err(); err != nil {
		return false, err
	}
	if m.currentRefreshGeneration(providerID) != generation {
		return false, nil
	}

	switch {
	case publication.Persist != nil:
		if err := m.modelsStore.Write(ctx, providerID, *publication.Persist.clone()); err != nil {
			return false, err
		}
	case publication.DeletePersisted:
		if err := m.modelsStore.Delete(ctx, providerID); err != nil {
			return false, err
		}
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}
	if m.currentRefreshGeneration(providerID) != generation {
		return false, nil
	}
	if publication.Update != nil {
		publication.Update()
	}
	return true, nil
}

// runProviderRefreshPhase runs one refresh phase for a provider: snapshot the
// stored catalog, then hand the provider that snapshot plus a
// generation-checked publish (pi runProviderRefreshPhase).
func (m *modelsImpl) runProviderRefreshPhase(
	ctx context.Context,
	p Provider,
	credential *Credential,
	allowNetwork bool,
	force bool,
	generation uint64,
) error {
	stored, err := m.modelsStore.Read(ctx, p.ID())
	if err != nil {
		return err
	}
	return p.RefreshModels(ctx, RefreshModelsContext{
		Credential: credential,
		// Stored models of types this version does not know are dropped from
		// the snapshot the provider restores; the store itself is left as is.
		Stored: withKnownModelTypes(stored.clone()),
		Publish: func(pub ModelsPublication) (bool, error) {
			return m.publishProviderModels(ctx, p.ID(), generation, pub)
		},
		AllowNetwork: allowNetwork,
		Force:        force,
		BaseURL:      p.BaseURL(),
	})
}

// Refresh refreshes the selected configured dynamic providers concurrently (pi
// ModelsImpl.refresh). Each provider first runs a cache-only phase that
// restores its stored catalog before any auth resolution or network access,
// then — when the network is allowed and the provider is configured — a network
// phase. Failures are collected per provider unless that provider's refresh was
// cancelled or superseded; a provider's refresh settles as soon as it is, and
// the sweep stops waiting as soon as ctx is done, returning a snapshot of the
// errors collected so far. Provider work abandoned either way cannot publish,
// because its generation is no longer current.
func (m *modelsImpl) Refresh(ctx context.Context, options *ModelsRefreshOptions) ModelsRefreshResult {
	allowNetwork := true
	force := false
	var selected map[string]bool
	if options != nil {
		if options.AllowNetwork != nil {
			allowNetwork = *options.AllowNetwork
		}
		force = options.Force
		if options.Providers != nil {
			selected = make(map[string]bool, len(options.Providers))
			for _, id := range options.Providers {
				selected[id] = true
			}
		}
	}

	var (
		errsMu sync.Mutex
		errs   = map[string]error{}
		wg     sync.WaitGroup
	)
	if ctx.Err() != nil {
		return ModelsRefreshResult{Aborted: true, Errors: errs}
	}

	for _, provider := range m.GetProviders() {
		if !provider.DynamicModels() || (selected != nil && !selected[provider.ID()]) {
			continue
		}
		generation, refreshCtx, entry := m.beginProviderRefresh(ctx, provider.ID())
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			defer m.endProviderRefresh(p.ID(), entry)

			// Settles as soon as this provider's refresh is cancelled or
			// superseded, even while the provider ignores refreshCtx (pi's
			// raceWithAbortSignal(operation, signal)).
			_, err := raceContext(refreshCtx, func() (struct{}, error) {
				return struct{}{}, m.refreshProvider(refreshCtx, p, allowNetwork, force, generation)
			})
			if err != nil && refreshCtx.Err() == nil {
				errsMu.Lock()
				errs[p.ID()] = err
				errsMu.Unlock()
			}
		}(provider)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	errsMu.Lock()
	defer errsMu.Unlock()
	snapshot := make(map[string]error, len(errs))
	for id, err := range errs {
		snapshot[id] = err
	}
	return ModelsRefreshResult{Aborted: ctx.Err() != nil, Errors: snapshot}
}

// refreshProvider runs one provider's cache phase and, when allowed, its
// network phase. A credential-store failure is reported only after the cache
// phase has had its chance to restore the last-known catalog.
func (m *modelsImpl) refreshProvider(
	ctx context.Context,
	p Provider,
	allowNetwork bool,
	force bool,
	generation uint64,
) error {
	stored, credentialErr := readCredential(ctx, m.credentials, p.ID())

	// Restore cached provider state before auth resolution or network access.
	if err := m.runProviderRefreshPhase(ctx, p, stored, false, false, generation); err != nil {
		return err
	}
	if credentialErr != nil {
		return credentialErr
	}
	if !allowNetwork || ctx.Err() != nil {
		return nil
	}

	credential, err := m.resolveRefreshCredential(ctx, p, stored)
	if err != nil {
		return err
	}
	if credential == nil {
		return nil // unconfigured: skip
	}
	return m.runProviderRefreshPhase(ctx, p, credential, true, force, generation)
}

// resolveRefreshCredential resolves the effective credential for a model
// refresh (pi resolveRefreshCredential): stored OAuth is refreshed when
// expired (under the store lock); otherwise api-key auth resolves to a
// synthetic api_key credential. nil means unconfigured. It only runs in the
// network phase, so pi's allowNetwork parameter is gone (fed6009c).
func (m *modelsImpl) resolveRefreshCredential(
	ctx context.Context,
	p Provider,
	stored *Credential,
) (*Credential, error) {
	if stored != nil && stored.Type == CredentialOAuth {
		oauth := p.Auth().OAuth
		if oauth == nil {
			return nil, nil
		}
		if nowMillis() < stored.Expires {
			return stored, nil
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		// A refresh that has started survives cancellation or a superseding model refresh, so a
		// rotated refresh token is always persisted. A newer refresh then sees the fresh credential.
		return refreshStoredOAuthCredential(ctx, m.credentials, p.ID(), oauth,
			func(current *Credential) bool { return nowMillis() >= current.Expires })
	}

	apiKey := p.Auth().APIKey
	if apiKey == nil {
		return nil, nil
	}
	var credential *Credential
	if stored != nil && stored.Type == CredentialAPIKey {
		credential = stored
	}
	result, err := apiKey.Resolve(ctx, m.authContext, credential)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return &Credential{Type: CredentialAPIKey, Key: result.Auth.APIKey, Env: result.Env}, nil
}

// checkProviderAuth checks auth configuration without refreshing OAuth
// (pi checkProviderAuth).
func (m *modelsImpl) checkProviderAuth(ctx context.Context, p Provider, credential *Credential) (*AuthCheck, error) {
	if credential != nil && credential.Type == CredentialOAuth {
		if p.Auth().OAuth != nil {
			return &AuthCheck{Source: "OAuth", Type: CredentialOAuth}, nil
		}
		return nil, nil
	}
	apiKey := p.Auth().APIKey
	if apiKey == nil {
		return nil, nil
	}
	if apiKey.Check != nil {
		var cred *Credential
		if credential != nil && credential.Type == CredentialAPIKey {
			cred = credential
		}
		check, err := apiKey.Check(ctx, m.authContext, cred)
		if err != nil {
			if isCancellation(ctx, err) {
				return nil, err
			}
			return nil, newModelsError(ErrAuth, "API key auth check failed for provider "+p.ID(), err)
		}
		return check, nil
	}

	resolution, err := resolveProviderAuth(ctx, p.ID(), p.Auth(), m.credentials, m.authContext, nil)
	if err != nil {
		return nil, err
	}
	if resolution == nil {
		return nil, nil
	}
	return &AuthCheck{Source: resolution.Source, Type: CredentialAPIKey}, nil
}

// raceContext runs op and returns as soon as either op finishes or ctx is
// cancelled — pi's raceWithAbortSignal (utils/abort.ts), which every public auth
// entry point, resolveProviderAuth and each provider's model refresh wrap
// around their operation.
//
// It is load-bearing rather than belt-and-braces: a provider's Resolve/Check is
// free to ignore the ctx it is handed (ApiKeyAuth.Resolve is documented as
// possibly executing commands), so without the race an aborted caller is held
// hostage by it. The abandoned goroutine runs to completion and its result is
// discarded, mirroring pi's "continue to observe the abandoned promise"; the
// buffered channel means it never blocks on the send, so nothing leaks. Refresh
// races the same way over its WaitGroup.
func raceContext[T any](ctx context.Context, op func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := op()
		done <- outcome{value: value, err: err}
	}()
	select {
	case o := <-done:
		return o.value, o.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (m *modelsImpl) CheckAuth(ctx context.Context, providerID string) (*AuthCheck, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raceContext(ctx, func() (*AuthCheck, error) {
		p := m.GetProvider(providerID)
		if p == nil {
			return nil, nil
		}
		credential, err := readCredential(ctx, m.credentials, providerID)
		if err != nil {
			return nil, err
		}
		return m.checkProviderAuth(ctx, p, credential)
	})
}

// authenticatedProvider is a provider whose auth configuration is complete,
// with the credential its availability policy sees.
type authenticatedProvider struct {
	provider   Provider
	credential *Credential
}

// authenticatedProviders checks auth for one provider, or for all when
// providerID is "", and returns the configured ones in provider order (pi
// getAuthenticatedProviders).
//
// pi runs the per-provider checks under Promise.all, so every provider is
// invoked even when one fails; a sequential loop stopped at the first error and
// never asked the rest. Results are collected by index, so the reported error
// is the first in provider order — Promise.all reports the first to reject in
// time, which is not reproducible.
func (m *modelsImpl) authenticatedProviders(ctx context.Context, providerID string) ([]authenticatedProvider, error) {
	var providers []Provider
	if providerID != "" {
		if p := m.GetProvider(providerID); p != nil {
			providers = []Provider{p}
		}
	} else {
		providers = m.GetProviders()
	}

	type providerCheck struct {
		credential *Credential
		auth       *AuthCheck
		err        error
	}
	checks := make([]providerCheck, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			var c providerCheck
			if c.credential, c.err = readCredential(ctx, m.credentials, p.ID()); c.err == nil {
				c.auth, c.err = m.checkProviderAuth(ctx, p, c.credential)
			}
			checks[i] = c
		}(i, p)
	}
	wg.Wait()

	var out []authenticatedProvider
	for i, c := range checks {
		if c.err != nil {
			return nil, c.err
		}
		if c.auth != nil {
			out = append(out, authenticatedProvider{provider: providers[i], credential: c.credential})
		}
	}
	return out, nil
}

func (m *modelsImpl) GetAvailable(ctx context.Context, providerID string) ([]*Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raceContext(ctx, func() ([]*Model, error) {
		providers, err := m.authenticatedProviders(ctx, providerID)
		if err != nil {
			return nil, err
		}
		var out []*Model
		for _, a := range providers {
			out = append(out, a.provider.FilterModels(a.provider.GetModels(), a.credential)...)
		}
		return out, nil
	})
}

func (m *modelsImpl) GetAllAvailable(ctx context.Context, providerID string) ([]*Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raceContext(ctx, func() ([]*Model, error) {
		providers, err := m.authenticatedProviders(ctx, providerID)
		if err != nil {
			return nil, err
		}
		var out []*Model
		for _, a := range providers {
			models := allModelsOf(a.provider)
			if filterer, ok := a.provider.(AllModelsFilterer); ok {
				out = append(out, filterer.FilterAllModels(models, a.credential)...)
				continue
			}
			// See AllModelsFilterer for where this differs from pi's fallback.
			available := a.provider.FilterModels(a.provider.GetModels(), a.credential)
			out = append(out, keepAvailableChatModels(models, available)...)
		}
		return out, nil
	})
}

func (m *modelsImpl) GetAvailableOfType(ctx context.Context, t ModelType, providerID string) ([]*Model, error) {
	all, err := m.GetAllAvailable(ctx, providerID)
	if err != nil {
		return nil, err
	}
	var out []*Model
	for _, model := range all {
		if IsModelType(model, t) {
			out = append(out, model)
		}
	}
	return out, nil
}

func (m *modelsImpl) GetProviderAuth(ctx context.Context, providerID string, overrides *AuthResolutionOverrides) (*AuthResult, error) {
	p := m.GetProvider(providerID)
	if p == nil {
		return nil, nil
	}
	return resolveProviderAuth(ctx, p.ID(), p.Auth(), m.credentials, m.authContext, overrides)
}

// GetAuth resolves provider auth for a model and merges the model's static
// headers on top (pi getAuth(model, overrides?)).
func (m *modelsImpl) GetAuth(ctx context.Context, model *Model, overrides *AuthResolutionOverrides) (*AuthResult, error) {
	result, err := m.GetProviderAuth(ctx, model.Provider, overrides)
	if err != nil || result == nil {
		return result, err
	}
	if len(model.Headers) == 0 {
		return result, nil
	}
	merged := *result
	merged.Auth.Headers = mergeHeaders(result.Auth.Headers, model.Headers)
	return &merged, nil
}

func (m *modelsImpl) Login(
	ctx context.Context,
	providerID string,
	authType CredentialKind,
	interaction AuthInteraction,
	options *LoginOptions,
) (*Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := m.GetProvider(providerID)
	if p == nil {
		return nil, newModelsError(ErrProvider, "Unknown provider: "+providerID, nil)
	}
	var login func(context.Context, AuthInteraction) (*Credential, error)
	if authType == CredentialOAuth {
		if oauth := p.Auth().OAuth; oauth != nil && oauth.Login != nil {
			login = func(ctx context.Context, interaction AuthInteraction) (*Credential, error) {
				return oauth.Login(ctx, interaction, options)
			}
		}
	} else if apiKey := p.Auth().APIKey; apiKey != nil {
		login = apiKey.Login
	}
	if login == nil {
		return nil, newModelsError(ErrAuth, p.Name()+" does not support "+string(authType)+" login", nil)
	}
	// pi races the login operation itself (models.ts:558), not the store
	// mutation that follows it: a provider login flow that ignores its signal
	// must not hold an aborted caller.
	credential, err := raceContext(ctx, func() (*Credential, error) {
		return login(ctx, interaction)
	})
	if err != nil {
		return nil, err
	}
	// A mutation cancelled while still queued never runs; one that has reached
	// the store settles before the caller is released (pi's mutationStarted
	// hand-off, expressed by the store's cancellable queue).
	if _, err := m.credentials.Modify(ctx, providerID, func(*Credential) (*Credential, error) {
		return credential, nil
	}); err != nil {
		if isCancellation(ctx, err) {
			return nil, err
		}
		return nil, newModelsError(ErrAuth, "Credential store modify failed for "+providerID, err)
	}
	return credential, nil
}

func (m *modelsImpl) Logout(ctx context.Context, providerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.credentials.Delete(ctx, providerID); err != nil {
		if isCancellation(ctx, err) {
			return err
		}
		return newModelsError(ErrAuth, "Credential store delete failed for "+providerID, err)
	}
	return nil
}

// applyAuth resolves auth and folds it into the request model + options
// (pi applyAuth). Explicit request options win per field; headers merge
// case-insensitively, env merges per key, and the Models-only header
// transform runs last. An unconfigured provider is an error (ff28097a; the
// pre-facade runtime passed the request through untouched).
func (m *modelsImpl) applyAuth(
	ctx context.Context,
	model *Model,
	opts *ProviderRequestOptions,
	transforms ModelsRequestTransforms,
) (*Model, *ProviderRequestOptions, error) {
	var overrides *AuthResolutionOverrides
	if opts != nil {
		overrides = &AuthResolutionOverrides{APIKey: opts.APIKey, Env: opts.Env}
	}
	resolution, err := m.GetAuth(ctx, model, overrides)
	if err != nil {
		return nil, nil, err
	}
	if resolution == nil {
		return nil, nil, newModelsError(ErrAuth, "Provider is not configured: "+model.Provider, nil)
	}
	auth := resolution.Auth

	ro := ProviderRequestOptions{}
	if opts != nil {
		ro = *opts
	}
	if ro.APIKey == "" { // options?.apiKey ?? auth.apiKey
		ro.APIKey = auth.APIKey
	}
	headers := mergeHeaders(auth.Headers, ro.Headers)
	if transforms.TransformHeaders != nil {
		if headers == nil {
			headers = ProviderHeaders{} // pi: transformHeaders(headers ?? {})
		}
		headers, err = transforms.TransformHeaders(headers)
		if err != nil {
			return nil, nil, err
		}
	}
	ro.Headers = headers
	ro.Env = mergeStringMap(resolution.Env, ro.Env) // explicit env override

	requestModel := model
	if auth.BaseURL != "" {
		clone := *model
		clone.BaseURL = auth.BaseURL
		requestModel = &clone
	}
	return requestModel, &ro, nil
}

// Every chat entry point asserts the model is a chat model before anything
// else, the provider lookup included (pi requireChatProvider, upstream
// a328aa89a): a model of another type fails the same way whether or not its
// provider is known, and never reaches a provider.

func (m *modelsImpl) Stream(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessageEventStream {
	transcript := NormalizeContext(req)
	if err := AssertChatModel(model); err != nil {
		return ErrorStream(model, err)
	}
	p := m.GetProvider(model.Provider)
	if p == nil {
		return ErrorStream(model, newModelsError(ErrProvider, "Unknown provider: "+model.Provider, nil))
	}
	var base *ProviderRequestOptions
	var transforms ModelsRequestTransforms
	if opts != nil {
		base = &opts.ProviderRequestOptions
		transforms = opts.ModelsRequestTransforms
	}
	requestModel, requestOptions, err := m.applyAuth(ctx, model, base, transforms)
	if err != nil {
		return ErrorStream(model, err)
	}
	stream := StreamOptions{}
	if opts != nil {
		stream = opts.StreamOptions
	}
	stream.ProviderRequestOptions = *requestOptions
	return p.Stream(ctx, requestModel, transcript, &stream)
}

func (m *modelsImpl) Complete(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessage {
	return m.Stream(ctx, model, req, opts).Result()
}

func (m *modelsImpl) StreamSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessageEventStream {
	transcript := NormalizeContext(req)
	if err := AssertChatModel(model); err != nil {
		return ErrorStream(model, err)
	}
	p := m.GetProvider(model.Provider)
	if p == nil {
		return ErrorStream(model, newModelsError(ErrProvider, "Unknown provider: "+model.Provider, nil))
	}
	var base *ProviderRequestOptions
	var transforms ModelsRequestTransforms
	if opts != nil {
		base = &opts.SimpleStreamOptions.ProviderRequestOptions
		transforms = opts.ModelsRequestTransforms
	}
	requestModel, requestOptions, err := m.applyAuth(ctx, model, base, transforms)
	if err != nil {
		return ErrorStream(model, err)
	}
	simple := SimpleStreamOptions{}
	if opts != nil {
		simple = opts.SimpleStreamOptions
	}
	simple.ProviderRequestOptions = *requestOptions
	return p.StreamSimple(ctx, requestModel, transcript, &simple)
}

func (m *modelsImpl) CompleteSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessage {
	return m.StreamSimple(ctx, model, req, opts).Result()
}

func (m *modelsImpl) StreamDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessageEventStream {
	if err := AssertChatModel(model); err != nil {
		return ErrorStream(model, err)
	}
	p := m.GetProvider(model.Provider)
	fetcher, ok := p.(DeferredFetcher)
	if !ok {
		return ErrorStream(model, deferredUnsupported(p, model.Provider))
	}
	var base *ProviderRequestOptions
	var transforms ModelsRequestTransforms
	if opts != nil {
		base = &opts.ProviderRequestOptions
		transforms = opts.ModelsRequestTransforms
	}
	requestModel, requestOptions, err := m.applyAuth(ctx, model, base, transforms)
	if err != nil {
		return ErrorStream(model, err)
	}
	deferredOptions := DeferredFetchOptions{}
	if opts != nil {
		deferredOptions = opts.DeferredFetchOptions
	}
	deferredOptions.ProviderRequestOptions = *requestOptions
	return fetcher.FetchDeferred(ctx, requestModel, handle, &deferredOptions)
}

// FetchDeferred is pi's `streamDeferred(...).result()` (upstream b37834b69) —
// the same redemption, awaited. Everything it used to do inline now lives in
// StreamDeferred, so the two cannot disagree about auth or about how a
// resolution failure is reported.
func (m *modelsImpl) FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessage {
	return m.StreamDeferred(ctx, model, handle, opts).Result()
}

func (m *modelsImpl) CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredCancelOptions) error {
	if err := AssertChatModel(model); err != nil {
		return err
	}
	p := m.GetProvider(model.Provider)
	canceller, ok := p.(DeferredCanceller)
	if !ok {
		return deferredUnsupported(p, model.Provider)
	}
	var base *ProviderRequestOptions
	var transforms ModelsRequestTransforms
	if opts != nil {
		base = &opts.DeferredCancelOptions
		transforms = opts.ModelsRequestTransforms
	}
	requestModel, requestOptions, err := m.applyAuth(ctx, model, base, transforms)
	if err != nil {
		return err
	}
	return canceller.CancelDeferred(ctx, requestModel, handle, requestOptions)
}

// deferredUnsupported explains a failed deferred capability assertion: either
// no provider owns the model, or the one that does never announced deferred
// responses (pi's `!provider.fetchDeferred` / `!provider.cancelDeferred`).
func deferredUnsupported(p Provider, providerID string) error {
	if p == nil {
		return newModelsError(ErrProvider, "Unknown provider: "+providerID, nil)
	}
	return newModelsError(ErrProvider,
		"Provider "+providerID+" does not support deferred responses"+deferredUnsupportedHint, nil)
}

// HasApi reports whether a chat model uses the given api (pi hasApi
// narrowing). A model of another type never matches, even when its api id
// equals api.
func HasApi(model *Model, api Api) bool {
	return IsModelType(model, ModelTypeChat) && model.Api == api
}

// mergeHeaders returns base overlaid with override, deleting base entries
// whose names match an override key case-insensitively before setting it
// (pi models.ts mergeHeaders). Names match when their JavaScript toLowerCase
// does (jstext.ToLower): pi keeps {"x-id": "v"} under an override
// {"X-\u0130D": nil}, which strings.ToLower would fold onto it. A deletion marker survives the merge like any
// other value: an override entry with a nil value replaces the base entry, so
// the suppression it encodes reaches the provider. nil when both inputs are
// nil. Override keys are applied in sorted order so case-colliding overrides
// merge deterministically (pi iterates insertion order; Go maps are
// unordered). The nested scan is O(n*m) — fine for header-sized maps.
// Values are copied as pointers, not cloned — see ProviderHeaders.
func mergeHeaders(base, override ProviderHeaders) ProviderHeaders {
	if base == nil && override == nil {
		return nil
	}
	merged := make(ProviderHeaders, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	names := make([]string, 0, len(override))
	for name := range override {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lower := jstext.ToLower(name)
		for existing := range merged {
			if jstext.ToLower(existing) == lower {
				delete(merged, existing)
			}
		}
		merged[name] = override[name]
	}
	return merged
}

// mergeStringMap returns {...base, ...override} or nil when both are empty.
// override wins per key.
func mergeStringMap(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}
