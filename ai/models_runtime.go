package ai

import (
	"context"
	"errors"
	"sync"
)

// Models runtime ported from pi packages/ai/src/models.ts (732bb161): the
// Provider/Models object-model, createModels/createProvider, and auth
// application. The pre-existing global free functions (Stream/GetModel/
// GetModels/GetProviders/GetEnvApiKey, models.go + stream.go) are the compat
// surface — pi's "@earendil-works/pi-ai/compat" — and stay available.
//
// pi defers provider resolution into the returned stream via lazyStream
// (async). The Go port keeps its existing contract (G3, stream.go): resolution
// runs synchronously and failures are encoded as a terminal stream error, so
// applyAuth runs inline and errors flow through errorStream.

// ProviderStreams binds an API's stream implementations (pi ProviderStreams).
type ProviderStreams struct {
	Stream       StreamFunction
	StreamSimple StreamSimpleFunction
}

// Provider is the concrete runtime unit (pi Provider). It owns id/name/base
// metadata, auth, model listing, and stream behavior.
type Provider interface {
	ID() string
	Name() string
	BaseURL() string
	Headers() map[string]string

	// Auth reports the provider's auth semantics. At least one of
	// APIKey/OAuth is set, even for ambient/keyless providers.
	Auth() ProviderAuth

	// GetModels returns the current known models (last-known list for dynamic
	// providers). Must not panic.
	GetModels() []*Model

	// RefreshModels re-fetches a dynamic provider's model list; a no-op for
	// static providers. Concurrent calls share one in-flight fetch.
	RefreshModels() error

	Stream(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream
	StreamSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream
}

// CreateProviderOptions are the parts createProvider assembles into a Provider.
// Exactly one of API / APIByApi is used: API streams all models; APIByApi
// dispatches on model.Api (a model whose api has no entry produces a stream
// error). RefreshModels is nil for static providers.
type CreateProviderOptions struct {
	ID            string
	Name          string
	BaseURL       string
	Headers       map[string]string
	Auth          ProviderAuth
	Models        []*Model
	RefreshModels func() ([]*Model, error)
	API           *ProviderStreams
	APIByApi      map[Api]ProviderStreams
}

type providerImpl struct {
	id, name, baseURL string
	headers           map[string]string
	auth              ProviderAuth
	single            *ProviderStreams
	byAPI             map[Api]ProviderStreams
	refreshFn         func() ([]*Model, error)

	mu             sync.Mutex
	models         []*Model
	inflight       chan struct{}
	lastRefreshErr error
}

// CreateProvider builds a Provider from parts (pi createProvider). Built-in
// factories and custom-model providers both go through this.
func CreateProvider(input CreateProviderOptions) Provider {
	name := input.Name
	if name == "" {
		name = input.ID
	}
	return &providerImpl{
		id:        input.ID,
		name:      name,
		baseURL:   input.BaseURL,
		headers:   input.Headers,
		auth:      input.Auth,
		single:    input.API,
		byAPI:     input.APIByApi,
		refreshFn: input.RefreshModels,
		models:    input.Models,
	}
}

func (p *providerImpl) ID() string                 { return p.id }
func (p *providerImpl) Name() string               { return p.name }
func (p *providerImpl) BaseURL() string            { return p.baseURL }
func (p *providerImpl) Headers() map[string]string { return p.headers }
func (p *providerImpl) Auth() ProviderAuth         { return p.auth }

func (p *providerImpl) GetModels() []*Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.models
}

// RefreshModels fetches and stores the model list; concurrent callers share the
// one in-flight fetch (pi's inflightRefresh). Static providers (nil refreshFn)
// are a no-op.
func (p *providerImpl) RefreshModels() error {
	p.mu.Lock()
	if p.refreshFn == nil {
		p.mu.Unlock()
		return nil
	}
	if p.inflight != nil {
		ch := p.inflight
		p.mu.Unlock()
		<-ch
		p.mu.Lock()
		err := p.lastRefreshErr
		p.mu.Unlock()
		return err
	}
	ch := make(chan struct{})
	p.inflight = ch
	fn := p.refreshFn
	p.mu.Unlock()

	models, err := fn()

	p.mu.Lock()
	if err == nil {
		p.models = models
	}
	p.lastRefreshErr = err
	p.inflight = nil
	close(ch)
	p.mu.Unlock()
	return err
}

// streamsFor selects the ProviderStreams for a model's api.
func (p *providerImpl) streamsFor(model *Model) (ProviderStreams, bool) {
	if p.single != nil {
		return *p.single, true
	}
	s, ok := p.byAPI[model.Api]
	return s, ok
}

func (p *providerImpl) Stream(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream {
	s, ok := p.streamsFor(model)
	if !ok || s.Stream == nil {
		return errorStream(model, newModelsError(ErrStream, "Provider "+p.id+" has no API implementation for \""+model.Api+"\"", nil))
	}
	return s.Stream(ctx, model, req, opts)
}

func (p *providerImpl) StreamSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream {
	s, ok := p.streamsFor(model)
	if !ok || s.StreamSimple == nil {
		return errorStream(model, newModelsError(ErrStream, "Provider "+p.id+" has no API implementation for \""+model.Api+"\"", nil))
	}
	return s.StreamSimple(ctx, model, req, opts)
}

// Models is the runtime collection of providers plus auth application and
// stream convenience (pi Models). Providers own stream behavior; Models
// resolves auth and delegates each request to the provider that owns the model.
type Models interface {
	GetProviders() []Provider
	GetProvider(id string) Provider

	// GetModels returns last-known models for one provider, or for all when
	// provider is "" (pi getModels(provider?)). Best-effort.
	GetModels(provider string) []*Model
	GetModel(provider, id string) *Model

	// Refresh asks dynamic providers to re-fetch. With a provider id, returns
	// that provider's fetch error (wrapped ModelsError "model_source"); with
	// "" refreshes all best-effort and never errors.
	Refresh(provider string) error

	// GetAuth resolves request auth for a model. Returns (nil, nil) when the
	// provider is unknown or unconfigured; a ModelsError on refresh/store
	// failure.
	GetAuth(model *Model) (*AuthResult, error)

	Stream(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream
	Complete(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessage
	StreamSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream
	CompleteSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessage
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
	AuthContext AuthContext
}

type modelsImpl struct {
	mu          sync.RWMutex
	providers   map[string]Provider
	order       []string // insertion order, mirroring pi's Map iteration
	credentials CredentialStore
	authContext AuthContext
}

// CreateModels builds an empty Models collection (pi createModels). Defaults:
// an InMemoryCredentialStore and the OS-backed AuthContext.
func CreateModels(options *CreateModelsOptions) MutableModels {
	var creds CredentialStore = NewInMemoryCredentialStore()
	var ac AuthContext = DefaultProviderAuthContext()
	if options != nil {
		if options.Credentials != nil {
			creds = options.Credentials
		}
		if options.AuthContext != nil {
			ac = options.AuthContext
		}
	}
	return &modelsImpl{providers: map[string]Provider{}, credentials: creds, authContext: ac}
}

func (m *modelsImpl) SetProvider(provider Provider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.providers[provider.ID()]; !exists {
		m.order = append(m.order, provider.ID())
	}
	m.providers[provider.ID()] = provider
}

func (m *modelsImpl) DeleteProvider(id string) {
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

func (m *modelsImpl) ClearProviders() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers = map[string]Provider{}
	m.order = nil
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

func (m *modelsImpl) Refresh(provider string) error {
	if provider != "" {
		p := m.GetProvider(provider)
		if p == nil {
			return nil
		}
		if err := p.RefreshModels(); err != nil {
			var me *ModelsError
			if errors.As(err, &me) {
				return err
			}
			return newModelsError(ErrModelSource, "Model refresh failed for "+provider, err)
		}
		return nil
	}
	// Best-effort: refresh all, swallow individual failures (pi allSettled).
	for _, p := range m.GetProviders() {
		_ = p.RefreshModels()
	}
	return nil
}

func (m *modelsImpl) GetAuth(model *Model) (*AuthResult, error) {
	p := m.GetProvider(model.Provider)
	if p == nil {
		return nil, nil
	}
	return resolveProviderAuth(p.ID(), p.Auth(), model, m.credentials, m.authContext)
}

// applyAuth resolves auth and folds it into the request model + options.
// Explicit request options win per field; headers and env merge per key
// (pi applyAuth + 2cbce395 env merge).
func (m *modelsImpl) applyAuth(model *Model, opts *StreamOptions) (*Model, *StreamOptions, error) {
	resolution, err := m.GetAuth(model)
	if err != nil {
		return nil, nil, err
	}
	if resolution == nil {
		return model, opts, nil
	}
	auth := resolution.Auth

	requestModel := model
	if auth.BaseURL != "" {
		clone := *model
		clone.BaseURL = auth.BaseURL
		requestModel = &clone
	}

	ro := StreamOptions{}
	if opts != nil {
		ro = *opts
	}
	if ro.APIKey == "" { // options?.apiKey ?? auth.apiKey
		ro.APIKey = auth.APIKey
	}
	ro.Headers = mergeStringMap(auth.Headers, ro.Headers) // explicit headers override
	ro.Env = mergeStringMap(resolution.Env, ro.Env)       // explicit env override
	return requestModel, &ro, nil
}

func (m *modelsImpl) Stream(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessageEventStream {
	p := m.GetProvider(model.Provider)
	if p == nil {
		return errorStream(model, newModelsError(ErrProvider, "Unknown provider: "+model.Provider, nil))
	}
	requestModel, requestOptions, err := m.applyAuth(model, opts)
	if err != nil {
		return errorStream(model, err)
	}
	return p.Stream(ctx, requestModel, req, requestOptions)
}

func (m *modelsImpl) Complete(ctx context.Context, model *Model, req Context, opts *StreamOptions) *AssistantMessage {
	return m.Stream(ctx, model, req, opts).Result()
}

func (m *modelsImpl) StreamSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessageEventStream {
	p := m.GetProvider(model.Provider)
	if p == nil {
		return errorStream(model, newModelsError(ErrProvider, "Unknown provider: "+model.Provider, nil))
	}
	var base *StreamOptions
	if opts != nil {
		base = &opts.StreamOptions
	}
	requestModel, requestOptions, err := m.applyAuth(model, base)
	if err != nil {
		return errorStream(model, err)
	}
	simple := SimpleStreamOptions{}
	if opts != nil {
		simple = *opts
	}
	if requestOptions != nil {
		simple.StreamOptions = *requestOptions
	}
	return p.StreamSimple(ctx, requestModel, req, &simple)
}

func (m *modelsImpl) CompleteSimple(ctx context.Context, model *Model, req Context, opts *SimpleStreamOptions) *AssistantMessage {
	return m.StreamSimple(ctx, model, req, opts).Result()
}

// HasApi reports whether a model uses the given api (pi hasApi narrowing).
func HasApi(model *Model, api Api) bool {
	return model.Api == api
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
