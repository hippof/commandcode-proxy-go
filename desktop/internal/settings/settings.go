// Package settings persists the desktop app's own configuration and exports
// the active account's API key for consumers like Claude Code's apiKeyHelper.
//
// Everything lives under %APPDATA%\commandcode-desktop (or the platform
// equivalent), never in the repository or the CLI credential directory.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config is persisted to config.json. Zero fields fall back to defaults.
type Config struct {
	// ProxyHost/ProxyPort describe where the local proxy listens (also the
	// probe endpoint).
	ProxyHost string `json:"proxyHost"`
	ProxyPort int    `json:"proxyPort"`

	// ProxyBinary is an absolute path to a commandcode-proxy executable. When
	// empty the app manages its own copy under the app-data dir and builds it
	// from ProxySource when needed.
	ProxyBinary string `json:"proxyBinary"`

	// VaultRoot overrides the credential archive location.
	VaultRoot string `json:"vaultRoot"`

	// ModelsURL is Command Code's global model catalog endpoint.
	ModelsURL string `json:"modelsURL"`

	// ProbeModels are model ids exercised against the account key to infer
	// plan availability.
	ProbeModels []string `json:"probeModels"`
}

// DefaultConfig returns sane defaults: local proxy on 8787 (the README
// default), a small probe model set, CLI named commandcode.
func DefaultConfig() Config {
	return Config{
		ProxyHost: "127.0.0.1",
		ProxyPort: 8787,
		ProbeModels: []string{
			"qwen/qwen3.7-plus",
			"deepseek/deepseek-v4-flash",
			"claude-sonnet-5",
		},
		ModelsURL: "https://api.commandcode.ai/provider/v1/models",
	}
}

// Merge fills empty fields of c from d.
func (c *Config) Merge(d Config) {
	if c.ProxyHost == "" {
		c.ProxyHost = d.ProxyHost
	}
	if c.ProxyPort == 0 {
		c.ProxyPort = d.ProxyPort
	}
	if c.ProxyBinary == "" {
		c.ProxyBinary = d.ProxyBinary
	}
	if c.VaultRoot == "" {
		c.VaultRoot = d.VaultRoot
	}
	if c.ModelsURL == "" {
		c.ModelsURL = d.ModelsURL
	}
	if len(c.ProbeModels) == 0 {
		c.ProbeModels = d.ProbeModels
	}
}

// Dir is the app-data directory (created on demand).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		// UserConfigDir fails only when env vars are absent; fall back to
		// home so the app keeps working in odd shells.
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	d := filepath.Join(base, "commandcode-desktop")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// Load reads config.json, returning defaults when missing or malformed.
func Load() (Config, error) {
	d, err := Dir()
	if err != nil {
		return DefaultConfig(), err
	}
	cfg := DefaultConfig()
	data, err := os.ReadFile(filepath.Join(d, "config.json"))
	if err != nil {
		return cfg, nil
	}
	var stored Config
	if json.Unmarshal(data, &stored) != nil {
		return cfg, nil
	}
	cfg.Merge(stored)
	return cfg, nil
}

// Save writes config.json.
func Save(c Config) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "config.json"), b, 0o644)
}

// ResolveVaultRoot resolves the effective vault location.
func (c *Config) ResolveVaultRoot() (string, error) {
	if c.VaultRoot != "" {
		if err := os.MkdirAll(c.VaultRoot, 0o700); err != nil {
			return "", err
		}
		return c.VaultRoot, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".commandcode-accounts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

// ActiveKeyPath is where the current account's bare API key is written for
// apiKeyHelper consumers.
func ActiveKeyPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "active.key"), nil
}

// WriteActiveKey stores the bare API key (no trailing newline) so
// `type active.key` style helpers return it cleanly.
func WriteActiveKey(key string) error {
	key = strings.TrimSpace(key)
	p, err := ActiveKeyPath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(key), 0o600)
}

// RemoveActiveKey deletes the exported key.
func RemoveActiveKey() error {
	p, err := ActiveKeyPath()
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// KeyHelperPath is a batch script that prints the active key — point Claude
// Code's apiKeyHelper at it.
func KeyHelperPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "keyhelper.cmd"), nil
}

// EnsureKeyHelper writes the apiKeyHelper script if missing. It prints
// whatever WriteActiveKey last stored.
func EnsureKeyHelper() (string, error) {
	p, err := KeyHelperPath()
	if err != nil {
		return "", err
	}
	const body = "@echo off\r\nrem Prints the Command Code API key selected by Command Code Accounts.\r\nrem Point Claude Code's apiKeyHelper at this file:\r\nrem   \"apiKeyHelper\": \"<this file's absolute path>\"\r\nif not exist \"%~dp0active.key\" (\r\n  echo keyhelper: no active account >&2\r\n  exit /b 1\r\n)\r\ntype \"%~dp0active.key\"\r\n"
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return "", err
	}
	return p, nil
}
