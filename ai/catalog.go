package ai

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed models_catalog.json
var modelsCatalogJSON []byte

var loadCatalogOnce sync.Once

// LoadBuiltinModels registers pi's generated model catalog (idempotent). It is
// invoked automatically by GetModel/GetModels/GetProviders.
func LoadBuiltinModels() {
	loadCatalogOnce.Do(func() {
		var catalog map[string]map[string]*Model
		if err := json.Unmarshal(modelsCatalogJSON, &catalog); err != nil {
			return
		}
		for provider, models := range catalog {
			for id, m := range models {
				if m.Provider == "" {
					m.Provider = provider
				}
				if m.ID == "" {
					m.ID = id
				}
				RegisterModel(m)
			}
		}
	})
}
