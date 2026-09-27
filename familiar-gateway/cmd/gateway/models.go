package main

import (
	"github.com/familiar/gateway/internal/config"
)

// modelByID looks a model up in the [[models]] config list. The
// router registry holds the same data but exposes no by-ID lookup;
// config is the source the registry was built from, so checking it
// directly is equivalent.
func modelByID(models []config.ModelConfig, id string) (config.ModelConfig, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return config.ModelConfig{}, false
}
