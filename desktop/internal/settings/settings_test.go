package settings

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolateConfigDir points os.UserConfigDir() at a temp tree on every platform.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("APPDATA", base)         // windows
	t.Setenv("XDG_CONFIG_HOME", base) // linux
	t.Setenv("HOME", base)            // darwin fallback
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", filepath.Join(base, "home"))
	}
	return base
}

// TestDirMigratesLegacyFolder is the safety net for the rename: an existing
// install must keep its config, logs and exported key.
func TestDirMigratesLegacyFolder(t *testing.T) {
	base := isolateConfigDir(t)

	old := filepath.Join(base, legacyAppDirName)
	if err := os.MkdirAll(filepath.Join(old, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"proxyPort":9999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "logs", "2026-01-01.log"), []byte("old log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "active.key"), []byte("user_exported"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, appDirName)
	if got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}

	// Everything moved, including nested files.
	for _, rel := range []string{"config.json", "active.key", filepath.Join("logs", "2026-01-01.log")} {
		if _, err := os.Stat(filepath.Join(got, rel)); err != nil {
			t.Fatalf("migrated folder is missing %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("legacy folder should be gone after a successful rename")
	}

	// The migrated config is what Load() actually reads.
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyPort != 9999 {
		t.Fatalf("migrated config not used: ProxyPort = %d", cfg.ProxyPort)
	}
}

// TestDirKeepsNewFolderWhenBothExist: never merge into an existing new folder.
func TestDirKeepsNewFolderWhenBothExist(t *testing.T) {
	base := isolateConfigDir(t)
	old := filepath.Join(base, legacyAppDirName)
	current := filepath.Join(base, appDirName)
	for _, d := range []string{old, current} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"proxyPort":1111}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "config.json"), []byte(`{"proxyPort":2222}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Fatalf("Dir() = %q", got)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyPort != 2222 {
		t.Fatalf("existing new folder was overwritten: ProxyPort = %d", cfg.ProxyPort)
	}
}

// TestDirCreatesFreshFolder covers a first run with nothing on disk.
func TestDirCreatesFreshFolder(t *testing.T) {
	base := isolateConfigDir(t)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(base, appDirName) {
		t.Fatalf("Dir() = %q", got)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
}

// TestActiveKeyAndHelperUseTheNewFolder keeps the consumer-facing paths in sync
// with the rename (apiKeyHelper target must not point at a dead path).
func TestActiveKeyAndHelperUseTheNewFolder(t *testing.T) {
	base := isolateConfigDir(t)
	if err := WriteActiveKey("user_key_1234567890"); err != nil {
		t.Fatal(err)
	}
	p, err := ActiveKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(p)) != appDirName {
		t.Fatalf("active.key lives in %q, want the %s folder", p, appDirName)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "user_key_1234567890" {
		t.Fatalf("active.key content = %q err=%v", b, err)
	}

	helper, err := EnsureKeyHelper()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(helper)) != appDirName {
		t.Fatalf("keyhelper at %q, want the %s folder", helper, appDirName)
	}
	if filepath.Join(base, appDirName) != filepath.Dir(helper) {
		t.Fatalf("keyhelper path %q does not match the app dir", helper)
	}
}

// TestLoadHonorsStoredValues is a regression guard: Load used to merge in the
// wrong direction, so every hand-edited setting was silently discarded.
func TestLoadHonorsStoredValues(t *testing.T) {
	base := isolateConfigDir(t)
	d := filepath.Join(base, appDirName)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	stored := `{"proxyPort":9999,"gatewayPort":54321,"logLevel":"debug","logKeepDays":3,` +
		`"proxyAuto":"off","gatewayAuto":"off","probeModels":["m-a","m-b"],"vaultRoot":"X:/vault"}`
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyPort != 9999 {
		t.Fatalf("proxyPort = %d, want the stored 9999", cfg.ProxyPort)
	}
	if cfg.LogLevel != "debug" || cfg.LogKeepDays != 3 {
		t.Fatalf("logging settings ignored: level=%q keep=%d", cfg.LogLevel, cfg.LogKeepDays)
	}
	if cfg.ProxyAuto != "off" || cfg.GatewayAuto != "off" {
		t.Fatalf("auto-start flags ignored: proxy=%q gateway=%q", cfg.ProxyAuto, cfg.GatewayAuto)
	}
	if len(cfg.ProbeModels) != 2 || cfg.ProbeModels[0] != "m-a" {
		t.Fatalf("probeModels = %v", cfg.ProbeModels)
	}
	if cfg.VaultRoot != "X:/vault" {
		t.Fatalf("vaultRoot = %q", cfg.VaultRoot)
	}
	// Unset fields still inherit defaults.
	if cfg.ProxyHost == "" || cfg.ModelsURL == "" {
		t.Fatalf("defaults not applied: host=%q models=%q", cfg.ProxyHost, cfg.ModelsURL)
	}
	if cfg.GatewayPort == legacyGatewayPort {
		t.Fatal("legacy gateway port was not migrated")
	}
}
