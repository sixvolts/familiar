package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/engine"
)

// loadConfig finds and loads the gateway config, falling back to defaults:
// $FAMILIAR_HOME/gateway.toml (~/.familiar without it), then ./gateway.toml.
func loadConfig(explicit string) (*config.Config, error) {
	if explicit != "" {
		return config.Load(explicit)
	}

	candidates := []string{
		filepath.Join(config.FamiliarHome(), "gateway.toml"),
		"./gateway.toml",
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			log.Printf("[gateway] using config: %s", path)
			return config.Load(path)
		}
	}

	log.Println("[gateway] no config file found, using defaults")
	return config.DefaultConfig(), nil
}

// makeAPIKeyFn returns a function that resolves a model's vault_key to its
// API key (the router asks only for models that set one): the model's
// api_key field, else the env var FAMILIAR_<ID>_KEY (the ID uppercased,
// anything but letters, digits and _ as _). There is no vault: the
// engine's VaultGet is unsupported, and asking it first cost a 3s
// timeout per resolution for nothing.
func makeAPIKeyFn(_ engine.Service, models []config.ModelConfig) func(string) string {
	// Build a map from model ID to ModelConfig for env-var fallback.
	modelsByVaultKey := make(map[string]config.ModelConfig)
	modelsByID := make(map[string]config.ModelConfig)
	for _, m := range models {
		if m.VaultKey != "" {
			modelsByVaultKey[m.VaultKey] = m
		}
		modelsByID[m.ID] = m
	}

	return func(vaultKey string) string {
		// 1. The model's api_key from config.
		if m, ok := modelsByVaultKey[vaultKey]; ok {
			if m.APIKey != "" {
				return m.APIKey
			}
			// 2. The env var FAMILIAR_<UPPERCASE_ID>_KEY.
			envKey := "FAMILIAR_" + strings.ToUpper(strings.ReplaceAll(m.ID, "/", "_")) + "_KEY"
			// Replace non-alphanumeric characters with underscores.
			envKey = sanitizeEnvKey(envKey)
			if v := os.Getenv(envKey); v != "" {
				return v
			}
		}

		return ""
	}
}

// sanitizeEnvKey replaces non-alphanumeric, non-underscore characters with underscores.
func sanitizeEnvKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
