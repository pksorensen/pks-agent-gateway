package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds persisted CLI configuration.
type Config struct {
	ServerURL      string `json:"serverUrl"`
	OIDCIssuer     string `json:"oidcIssuer"`
	OIDCClientID   string `json:"oidcClientId"`
	CurrentProject string `json:"currentProject"`
}

// ConfigDir returns ~/.config/gateway-cli (cross-platform via os.UserConfigDir).
func ConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "gateway-cli")
}

func configPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// Load reads config.json, returning defaults if the file is missing.
// The GATEWAY_URL env var overrides ServerURL.
func Load() (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(configPath())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}
	if env := os.Getenv("GATEWAY_URL"); env != "" {
		cfg.ServerURL = env
	}
	return cfg, nil
}

// Save writes cfg to config.json atomically.
func Save(cfg *Config) error {
	if err := os.MkdirAll(ConfigDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}
