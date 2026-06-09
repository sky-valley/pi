package ai

import "sync"

var (
	modelRegMu sync.RWMutex
	modelReg   = map[string]map[string]*Model{} // provider -> id -> model
)

// RegisterModel adds a model to the catalog, keyed by provider and id.
func RegisterModel(m *Model) {
	modelRegMu.Lock()
	defer modelRegMu.Unlock()
	if modelReg[m.Provider] == nil {
		modelReg[m.Provider] = map[string]*Model{}
	}
	modelReg[m.Provider][m.ID] = m
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

// GetProviders returns the registered provider names.
func GetProviders() []string {
	LoadBuiltinModels()
	modelRegMu.RLock()
	defer modelRegMu.RUnlock()
	out := make([]string, 0, len(modelReg))
	for p := range modelReg {
		out = append(out, p)
	}
	return out
}

// GetModels returns all registered models for a provider.
func GetModels(provider string) []*Model {
	LoadBuiltinModels()
	modelRegMu.RLock()
	defer modelRegMu.RUnlock()
	pm := modelReg[provider]
	out := make([]*Model, 0, len(pm))
	for _, m := range pm {
		out = append(out, m)
	}
	return out
}

// CalculateCost computes per-bucket dollar cost for usage and stores it in
// usage.Cost (mutating in place), returning the breakdown.
func CalculateCost(model *Model, usage *Usage) CostBreakdown {
	usage.Cost.Input = model.Cost.Input / 1_000_000 * float64(usage.Input)
	usage.Cost.Output = model.Cost.Output / 1_000_000 * float64(usage.Output)
	usage.Cost.CacheRead = model.Cost.CacheRead / 1_000_000 * float64(usage.CacheRead)
	usage.Cost.CacheWrite = model.Cost.CacheWrite / 1_000_000 * float64(usage.CacheWrite)
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	return usage.Cost
}

var extendedThinkingLevels = []ModelThinkingLevel{"off", "minimal", "low", "medium", "high", "xhigh"}

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
		if level == "xhigh" && !present {
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

// ModelsAreEqual reports whether two models share id and provider.
func ModelsAreEqual(a, b *Model) bool {
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID && a.Provider == b.Provider
}
