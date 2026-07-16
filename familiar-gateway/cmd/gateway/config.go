package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/engine"
)

// loadConfig finds and loads the gateway config, falling back to defaults.
func loadConfig(explicit string) (*config.Config, error) {
	if explicit != "" {
		return config.Load(explicit)
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".familiar", "gateway.toml"),
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

// makeAPIKeyFn returns a function that resolves vault keys to API key strings.
// Resolution order: engine vault → model's api_key field → env var FAMILIAR_<ID>_KEY.
func makeAPIKeyFn(eng engine.Service, models []config.ModelConfig) func(string) string {
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
		// 1. Try engine vault.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		val, found, err := eng.VaultGet(ctx, vaultKey)
		if err == nil && found && val != "" {
			return val
		}

		// 2. Try direct api_key from config.
		if m, ok := modelsByVaultKey[vaultKey]; ok {
			if m.APIKey != "" {
				return m.APIKey
			}
			// 3. Try env var FAMILIAR_<UPPERCASE_ID>_KEY.
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
