package main

import (
	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/router"
)

// modelCatalog projects *router.Registry into the narrow
// admin.ModelCatalog interface. Lives here (not in internal/admin)
// so the admin package doesn't take a dependency on the router
// package — admin already stays infrastructure-agnostic via similar
// adapters (notes/wiki/conversations stores, skill catalog).
// MODEL-SELECTOR.md.
type modelCatalog struct{ reg *router.Registry }

func (m modelCatalog) Models() []admin.ModelInfo {
	if m.reg == nil {
		return nil
	}
	entries := m.reg.List()
	out := make([]admin.ModelInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, admin.ModelInfo{
			ID:             e.Config.ID,
			DisplayName:    e.Config.DisplayLabel(),
			LatencyProfile: e.Config.LatencyProfile,
			Capabilities:   append([]string(nil), e.Config.Capabilities...),
		})
	}
	return out
}

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
