// Package settings persists the desktop app's own configuration and exports
// the active account's API key for consumers like Claude Code's apiKeyHelper.
//
// Everything lives under %APPDATA%\cmdc-desktop (or the platform
// equivalent), never in the repository or the CLI credential directory.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"commandcode-desktop/internal/applog"
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

	// GatewayHost/GatewayPort is the loopback address of the credential
	// injecting gateway that editors point their base URL at. The port is
	// deliberately uncommon and fixed: editors remember it, so it must not
	// drift (the proxy behind it may fall back to another port freely).
	GatewayHost string `json:"gatewayHost"`
	GatewayPort int    `json:"gatewayPort"`

	// ProxyAuto/GatewayAuto are "on" (default) to start that service with the
	// app, "off" to leave it stopped until asked from the tray.
	ProxyAuto   string `json:"proxyAuto"`
	GatewayAuto string `json:"gatewayAuto"`

	// LogLevel is the minimum level written to the daily log file
	// (debug, info, warn, error). LogKeepDays prunes older daily files.
	LogLevel    string `json:"logLevel"`
	LogKeepDays int    `json:"logKeepDays"`

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
		ModelsURL:   "https://api.commandcode.ai/provider/v1/models",
		GatewayHost: "127.0.0.1",
		GatewayPort: 54321,
		ProxyAuto:   "on",
		GatewayAuto: "on",
		LogLevel:    "info",
		LogKeepDays: 14,
	}
}

// Merge fills empty fields of c from d, so callers keep whatever they
// configured and only inherit the parts they left unset.
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
	if c.GatewayHost == "" {
		c.GatewayHost = d.GatewayHost
	}
	if c.GatewayPort == 0 {
		c.GatewayPort = d.GatewayPort
	}
	if c.ProxyAuto == "" {
		c.ProxyAuto = d.ProxyAuto
	}
	if c.GatewayAuto == "" {
		c.GatewayAuto = d.GatewayAuto
	}
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}
	if c.LogKeepDays == 0 {
		c.LogKeepDays = d.LogKeepDays
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
	d := filepath.Join(base, appDirName)
	if err := migrateLegacyDir(base, d); err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

const (
	// appDirName is the app-data folder the app owns.
	appDirName = "cmdc-desktop"
	// legacyAppDirName is the folder earlier builds used; it is migrated on
	// first run so existing config/logs/exported keys survive the rename.
	legacyAppDirName = "commandcode-desktop"
)

// migrateLegacyDir renames the pre-rename app-data folder when the new one
// does not exist yet. A failure is reported but never fatal: the app simply
// starts with a fresh folder if the move is not possible.
func migrateLegacyDir(base, current string) error {
	if _, err := os.Stat(current); err == nil {
		return nil // already on the new name
	}
	old := filepath.Join(base, legacyAppDirName)
	if _, err := os.Stat(old); err != nil {
		return nil // nothing to migrate
	}
	if err := os.Rename(old, current); err != nil {
		// Cross-device or locked: copy what we can, then leave the old folder.
		if cerr := copyDir(old, current); cerr != nil {
			return cerr
		}
	}
	applog.Info("settings", "已迁移应用数据目录", "from", old, "to", current)
	return nil
}

// copyDir copies a directory tree (used only as a rename fallback).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

// legacyGatewayPort is the port this app used before 54321 became the fixed
// default. Configs that still carry it were never a deliberate choice, so they
// are migrated — editors remember the gateway address, it must not drift.
const legacyGatewayPort = 8790

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
	if stored.GatewayPort == legacyGatewayPort {
		stored.GatewayPort = 0 // adopt the current default
	}
	// Stored values win; Merge only fills what the file left unset. (The other
	// order — defaults winning — silently ignored every hand-edited setting.)
	stored.Merge(cfg)
	return stored, nil
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
