package ai

import (
	"slices"
	"sync"
)

var (
	modelRegMu sync.RWMutex
	modelReg   = map[string]map[string]*Model{} // provider -> id -> model
	// providerOrder and modelOrder keep registration order, as pi's registry
	// does: providers in the order they were registered, each provider's
	// models in theirs (the catalog's, for a built-in provider).
	providerOrder []string
	modelOrder    = map[string][]string{} // provider -> ids
)

// RegisterModel adds a model to the catalog, keyed by provider and id. A
// model registered again replaces the old one in its place, as an assignment
// to an existing key of pi's MODELS object does, and a new one comes after
// every model already there. The built-in catalog loads first, so a host's
// model is never overwritten by it and never listed ahead of it.
func RegisterModel(m *Model) {
	LoadBuiltinModels()
	registerModel(m)
}

// registerModel is RegisterModel for the catalog loader, which runs inside
// LoadBuiltinModels.
func registerModel(m *Model) {
	modelRegMu.Lock()
	defer modelRegMu.Unlock()
	registerProviderLocked(m.Provider)
	if _, ok := modelReg[m.Provider][m.ID]; !ok {
		modelOrder[m.Provider] = append(modelOrder[m.Provider], m.ID)
	}
	modelReg[m.Provider][m.ID] = m
}

// registerProvider records a provider that may have no models yet.
func registerProvider(provider string) {
	modelRegMu.Lock()
	defer modelRegMu.Unlock()
	registerProviderLocked(provider)
}

func registerProviderLocked(provider string) {
	if modelReg[provider] == nil {
		modelReg[provider] = map[string]*Model{}
		providerOrder = append(providerOrder, provider)
	}
}

// GetModel returns the registered model for provider+id, or nil.
func GetModel(provider, id string) *Model {
	LoadBuiltinModels()
	modelRegMu.RLock()
	defer modelRegMu.RUnlock()
	if pm := modelReg[provider]; pm != nil {
		return pm[id]
	}
	return nil
}

// GetProviders returns the registered provider names, in registration order
// (the catalog's for the built-in ones).
func GetProviders() []string {
	LoadBuiltinModels()
	modelRegMu.RLock()
	defer modelRegMu.RUnlock()
	return slices.Clone(providerOrder)
}

// GetModels returns all registered models for a provider, in registration
// order (the catalog's for a built-in provider).
func GetModels(provider string) []*Model {
	LoadBuiltinModels()
	modelRegMu.RLock()
	defer modelRegMu.RUnlock()
	ids := modelOrder[provider]
	out := make([]*Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, modelReg[provider][id])
	}
	return out
}

// CalculateCost computes per-bucket dollar cost for usage and stores it in
// usage.Cost (mutating in place), returning the breakdown.
func CalculateCost(model *Model, usage *Usage) CostBreakdown {
	// Input-based pricing tiers: pick the highest tier whose threshold the total
	// input usage clears, else the base rates (pi calculateCost, models.ts).
	inputTokens := usage.Input + usage.CacheRead + usage.CacheWrite
	rateInput, rateOutput, rateCacheRead, rateCacheWrite := model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite
	matchedThreshold := -1
	for _, tier := range model.Cost.Tiers {
		if inputTokens > tier.InputTokensAbove && tier.InputTokensAbove > matchedThreshold {
			rateInput, rateOutput, rateCacheRead, rateCacheWrite = tier.Input, tier.Output, tier.CacheRead, tier.CacheWrite
			matchedThreshold = tier.InputTokensAbove
		}
	}

	// Anthropic charges 2x base input for 1h cache writes.
	//
	// Each product is converted explicitly before it is summed. The Go spec
	// lets the compiler fuse x*y + z into one FMA (arm64 does; so does amd64 at
	// GOAMD64=v3), rounding once where V8 rounds the multiply and the add
	// separately, and the conversion is what forbids that fusion. Without it the
	// total and cacheWrite land a last bit off pi's on some usages.
	longWrite := usage.CacheWrite1h
	shortWrite := usage.CacheWrite - longWrite
	usage.Cost.Input = float64(rateInput / 1_000_000 * float64(usage.Input))
	usage.Cost.Output = float64(rateOutput / 1_000_000 * float64(usage.Output))
	usage.Cost.CacheRead = float64(rateCacheRead / 1_000_000 * float64(usage.CacheRead))
	usage.Cost.CacheWrite = (float64(rateCacheWrite*float64(shortWrite)) + float64(rateInput*2*float64(longWrite))) / 1_000_000
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	// The total just computed is the catalog estimate (upstream 7c22a93d0),
	// whatever the usage carried before.
	usage.Cost.TotalFromProvider = OptBoolFalse
	return usage.Cost
}

var extendedThinkingLevels = []ModelThinkingLevel{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// GetSupportedThinkingLevels returns the reasoning levels a model supports.
func GetSupportedThinkingLevels(model *Model) []ModelThinkingLevel {
	if !model.Reasoning {
		return []ModelThinkingLevel{"off"}
	}
	var out []ModelThinkingLevel
	for _, level := range extendedThinkingLevels {
		mapped, present := model.ThinkingLevelMap[level]
		if present && mapped == nil {
			// null => explicitly unsupported
			continue
		}
		// xhigh and max are opt-in: a model exposes them only when its
		// thinkingLevelMap carries an explicit entry (pi getSupportedThinkingLevels).
		if (level == "xhigh" || level == "max") && !present {
			continue
		}
		out = append(out, level)
	}
	return out
}

// ClampThinkingLevel clamps a requested level to the nearest supported level.
func ClampThinkingLevel(model *Model, level ModelThinkingLevel) ModelThinkingLevel {
	available := GetSupportedThinkingLevels(model)
	contains := func(l ModelThinkingLevel) bool {
		for _, a := range available {
			if a == l {
				return true
			}
		}
		return false
	}
	if contains(level) {
		return level
	}
	idx := -1
	for i, l := range extendedThinkingLevels {
		if l == level {
			idx = i
			break
		}
	}
	if idx == -1 {
		if len(available) > 0 {
			return available[0]
		}
		return "off"
	}
	for i := idx; i < len(extendedThinkingLevels); i++ {
		if contains(extendedThinkingLevels[i]) {
			return extendedThinkingLevels[i]
		}
	}
	for i := idx - 1; i >= 0; i-- {
		if contains(extendedThinkingLevels[i]) {
			return extendedThinkingLevels[i]
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return "off"
}

// ModelsAreEqual reports whether two models share type, id and provider (pi
// modelsAreEqual; the type joined the comparison in upstream a328aa89a, so a
// chat model and an image model with one provider and id are different
// models). A model without a type compares as a chat model.
func ModelsAreEqual(a, b *Model) bool {
	if a == nil || b == nil {
		return false
	}
	return GetModelType(a) == GetModelType(b) && a.ID == b.ID && a.Provider == b.Provider
}
