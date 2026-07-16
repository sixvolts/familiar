// Package config holds the workspace service's startup configuration.
//
// The workspace is intentionally a thin Go service — its config is
// correspondingly small: where to listen, where to find the gateway,
// where to find static files, optional TLS cert paths. Everything
// else (auth, data, business logic) belongs to the gateway.
//
// Config is loaded from a TOML file plus environment-variable
// overrides, mirroring the gateway's pattern so operators see one
// shape across services.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the parsed workspace.toml. Listen + GatewayURL +
// StaticDir are required (defaults applied if missing). TLS is
// optional — without cert/key the service serves plain HTTP, which
// is fine for dev and for deployments fronted by another TLS
// terminator.
type Config struct {
	// ListenAddr is host:port for the workspace HTTP server. Default
	// ":8443" when TLS is configured, ":3000" otherwise (matches the
	// dev-vs-prod expectation in the spec).
	ListenAddr string `toml:"listen_addr"`

	// GatewayURL is the base URL of familiar-gateway, including
	// scheme and port. The reverse proxy forwards /v1/* and
	// /console/api/* to this host. Default "http://localhost:8000"
	// matches the gateway's API-only deployment after Phase 0.
	GatewayURL string `toml:"gateway_url"`

	// StaticDir is the directory served as workspace static files
	// (index.html, app.js, app.css, etc.). Resolved relative to the
	// process working directory when not absolute. Default "./static".
	StaticDir string `toml:"static_dir"`

	// TLS is optional. When Cert + Key are both set the service
	// listens with TLS; otherwise plain HTTP.
	TLS TLSConfig `toml:"tls"`
}

// TLSConfig holds paths to a PEM-encoded cert + private key.
// Both must be set for TLS to engage; either missing falls back
// to plain HTTP.
type TLSConfig struct {
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// Enabled reports whether both TLS paths are present.
func (t TLSConfig) Enabled() bool {
	return t.Cert != "" && t.Key != ""
}

// Load reads workspace.toml from path and applies defaults. Empty
// path uses ./workspace.toml. Returns an error if the file exists
// but can't be parsed; a missing file is NOT an error — defaults
// produce a usable dev-mode service (plain HTTP on :3000, gateway
// at localhost:8000, static from ./static).
func Load(path string) (*Config, error) {
	if path == "" {
		path = "workspace.toml"
	}
	cfg := &Config{}
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("workspace: parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace: stat %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.GatewayURL == "" {
		c.GatewayURL = "http://localhost:8000"
	}
	if c.StaticDir == "" {
		c.StaticDir = "./static"
	}
	if c.ListenAddr == "" {
		// :8443 when TLS, :3000 otherwise — keeps dev → prod port
		// migration to a single-line config change.
		if c.TLS.Enabled() {
			c.ListenAddr = ":8443"
		} else {
			c.ListenAddr = ":3000"
		}
	}
}

// ResolvedStaticDir returns the absolute path to StaticDir, with
// any relative path joined to the current working directory. Used
// in startup logs so operators see exactly which directory is being
// served and in the file-existence check that gates startup.
func (c *Config) ResolvedStaticDir() string {
	if filepath.IsAbs(c.StaticDir) {
		return c.StaticDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return c.StaticDir
	}
	return filepath.Join(wd, c.StaticDir)
}
