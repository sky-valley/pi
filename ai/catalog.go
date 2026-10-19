package ai

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed models_catalog.json
var modelsCatalogJSON []byte

var loadCatalogOnce sync.Once

// LoadBuiltinModels registers pi's generated model catalog (idempotent). It is
// invoked automatically by GetModel/GetModels/GetProviders.
func LoadBuiltinModels() {
	loadCatalogOnce.Do(func() {
		err := decodeCatalog(modelsCatalogJSON, func(provider string) {
			// pi's getBuiltinProviders is Object.keys(MODELS): a key with no
			// chat models (typesafe, which serves only classifiers) is still a
			// built-in provider.
			registerProvider(provider)
		}, func(provider, id string, m *Model) {
			if m.Provider == "" {
				m.Provider = provider
			}
			if m.ID == "" {
				m.ID = id
			}
			RegisterModel(m)
		})
		if err != nil {
			// The catalog is a compile-time embed regenerated from pi's npm
			// build; corruption is a programmer error and must fail loud (a
			// silent empty catalog masked behind GetModel nils otherwise).
			panic("ai: embedded models_catalog.json is corrupt: " + err.Error())
		}
	})
}

// decodeCatalog walks the catalog's provider -> id -> model object in the
// order it is written, pi's MODELS order, which a map would lose.
func decodeCatalog(data []byte, onProvider func(provider string), onModel func(provider, id string, m *Model)) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	key := func() (string, error) {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		k, ok := tok.(string)
		if !ok {
			return "", fmt.Errorf("expected an object key, got %v", tok)
		}
		return k, nil
	}
	delim := func(want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if tok != want {
			return fmt.Errorf("expected %v, got %v", want, tok)
		}
		return nil
	}
	if err := delim('{'); err != nil {
		return err
	}
	for dec.More() {
		provider, err := key()
		if err != nil {
			return err
		}
		onProvider(provider)
		if err := delim('{'); err != nil {
			return err
		}
		for dec.More() {
			id, err := key()
			if err != nil {
				return err
			}
			var m *Model
			if err := dec.Decode(&m); err != nil {
				return fmt.Errorf("%s/%s: %w", provider, id, err)
			}
			onModel(provider, id, m)
		}
		if err := delim('}'); err != nil {
			return err
		}
	}
	return delim('}')
}
