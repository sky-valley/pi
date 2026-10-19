package ai

import (
	"slices"
	"sync"
)

// compat.ts's module state: the built-in api implementations, the registry
// entries they had when they were last registered, and compatModels, the
// built-in providers as a Models runtime with no stored credentials. compat's
// Stream sends a built-in model to its built-in provider only while the
// registry still holds that entry for the model's api
// (getBuiltinProviderForModel); an implementation a host registered over it
// is streamed directly.

var (
	builtinMu sync.RWMutex
	// builtinApis are the built-in api implementations, as ai/providers hands
	// them over (pi BUILTIN_APIS). BuiltinModels streams through them.
	builtinApis = map[Api]ApiProvider{}
	// builtinGen counts the handovers that added an api, so compatModels is
	// rebuilt from a set that grew.
	builtinGen uint64
	// builtinInstances is, for each built-in api, the registration the
	// registry held when the built-ins were last registered (pi
	// builtinApiProviderInstances).
	builtinInstances = map[Api]uint64{}
)

// RegisterBuiltInApiProviders is pi's registerBuiltInApiProviders, which
// ai/providers runs when it is imported: it registers each built-in api
// implementation whose api has no provider yet — never replacing one a host
// registered — and records what the registry now holds for those apis as
// the built-ins compat's Stream routes through. Running it again records the
// registry anew. The first implementation handed over for an api is the one
// BuiltinModels streams through, whatever the registry holds.
func RegisterBuiltInApiProviders(providers ...ApiProvider) {
	builtinMu.Lock()
	defer builtinMu.Unlock()
	for _, p := range providers {
		if _, ok := builtinApis[p.Api]; !ok {
			builtinApis[p.Api] = p
			builtinGen++
		}
		if _, ok := GetApiProvider(p.Api); !ok {
			RegisterApiProvider(builtinApis[p.Api])
		}
		if seq, ok := registration(p.Api); ok {
			builtinInstances[p.Api] = seq
		} else {
			delete(builtinInstances, p.Api)
		}
	}
}

// builtinApi returns api's built-in implementation, if it has one.
func builtinApi(api Api) (ApiProvider, bool) {
	builtinMu.RLock()
	defer builtinMu.RUnlock()
	p, ok := builtinApis[api]
	return p, ok
}

// compatState holds compatModels. pi builds it when compat loads; it is built
// here on first use, and again once the built-in implementations grew.
var compatState struct {
	sync.Mutex
	models MutableModels
	gen    uint64
}

// compatModels is compat.ts's compatModels (builtinModels()).
func compatModels() MutableModels {
	builtinMu.RLock()
	gen := builtinGen
	builtinMu.RUnlock()
	compatState.Lock()
	defer compatState.Unlock()
	if compatState.models == nil || compatState.gen != gen {
		compatState.models = BuiltinModels()
		compatState.gen = gen
	}
	return compatState.models
}

// builtinProviderFor is compat.ts getBuiltinProviderForModel: the built-in
// provider of model's name, when the registry still holds the entry the
// built-ins had for model's api when they were last registered and that
// provider has a model on the api; nil otherwise.
func builtinProviderFor(model *Model) Provider {
	current, registered := registration(model.Api)
	builtinMu.RLock()
	built, wasRegistered := builtinInstances[model.Api]
	builtinMu.RUnlock()
	if registered != wasRegistered || current != built {
		return nil
	}
	provider := compatModels().GetProvider(model.Provider)
	if provider == nil || !slices.ContainsFunc(provider.GetModels(), func(m *Model) bool { return m.Api == model.Api }) {
		return nil
	}
	return provider
}
