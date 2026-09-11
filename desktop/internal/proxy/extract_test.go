package proxy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"commandcode-desktop/internal/settings"
)

// withEmbedded swaps the injected binary for the duration of a test.
func withEmbedded(t *testing.T, blob []byte) {
	t.Helper()
	prev := embeddedBytes
	t.Cleanup(func() { SetEmbeddedBinary(prev) })
	SetEmbeddedBinary(blob)
}

// isolateAppData points settings.Dir() (and thus the managed binary path) at a
// temp tree on every platform.
func isolateAppData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	return dir
}

func TestExtractEmbedded(t *testing.T) {
	dir := t.TempDir()

	// Without an embedded binary there is nothing to write and the error says
	// so (a checkout that has not built the proxy yet).
	withEmbedded(t, nil)
	if _, err := ExtractEmbedded(dir); err == nil || !strings.Contains(err.Error(), "没有嵌入") {
		t.Fatalf("want a clear missing-binary error, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("nothing should be written when nothing is embedded: %v", entries)
	}

	// First extraction writes the binary plus a digest sidecar.
	blob := []byte("fake proxy binary v1")
	withEmbedded(t, blob)
	path, err := ExtractEmbedded(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != string(blob) {
		t.Fatalf("extracted %q err=%v", b, err)
	}
	if _, err := os.Stat(path + ".sha256"); err != nil {
		t.Fatalf("digest sidecar missing: %v", err)
	}
	if filepath.Base(path) != BinaryName() {
		t.Fatalf("binary name = %q, want %q", filepath.Base(path), BinaryName())
	}

	// Identical content is not rewritten (so a running binary is left alone).
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := ExtractEmbedded(dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged binary was rewritten")
	}

	// A new build replaces the copy.
	blob2 := []byte("fake proxy binary v2 — rebuilt, longer payload")
	withEmbedded(t, blob2)
	if _, err := ExtractEmbedded(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(blob2) {
		t.Fatalf("binary not replaced: %q", b)
	}

	// Extraction into a path that cannot be replaced must fail loudly rather
	// than silently keep a stale binary.
	t.Run("unreplaceable target", func(t *testing.T) {
		blocked := t.TempDir()
		if err := os.Mkdir(filepath.Join(blocked, BinaryName()), 0o700); err != nil {
			t.Fatal(err)
		}
		withEmbedded(t, []byte("payload"))
		if _, err := ExtractEmbedded(blocked); err == nil {
			t.Fatal("want an error when the target cannot be written")
		}
	})
}

func TestStartWithoutAnyBinary(t *testing.T) {
	cfg := settings.Config{ProxyHost: "127.0.0.1", ProxyPort: 18787}
	dir := isolateAppData(t)
	t.Setenv("COMMANDCODE_HOME", filepath.Join(dir, "cc-absent"))
	withEmbedded(t, nil) // no embedded copy, none on disk either

	m := NewManager(settings.Config{ProxyHost: cfg.ProxyHost, ProxyPort: cfg.ProxyPort})
	err := m.Start()
	if err == nil {
		t.Fatal("Start must fail when no proxy binary exists")
	}
	msg := err.Error()
	if !strings.Contains(msg, "内置") && !strings.Contains(msg, "proxyBinary") {
		t.Fatalf("error should explain how to fix it, got: %v", err)
	}
	if st := m.Status(); st.BinaryExists {
		t.Fatal("Status must not claim a binary exists")
	}
}

// TestExtractKeepsRunningCopy documents the Windows reality that a running
// executable cannot be replaced: extraction must fall back to the existing
// file instead of failing the app.
func TestExtractKeepsRunningCopy(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("file locking of a running exe is a Windows behaviour")
	}
	dir := t.TempDir()
	withEmbedded(t, []byte("v1 payload"))
	path, err := ExtractEmbedded(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate "in use" the same way Windows does for a running image.
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o666) })

	withEmbedded(t, []byte("v2 payload, different length"))
	got, err := ExtractEmbedded(dir)
	if err != nil {
		t.Fatalf("extraction must fall back to the existing copy, got %v", err)
	}
	if got != path {
		t.Fatalf("got %q, want the existing %q", got, path)
	}
	if b, _ := os.ReadFile(path); string(b) != "v1 payload" {
		t.Fatalf("existing copy was damaged: %q", b)
	}
}

func TestEmbeddedBinaryReportsEmptiness(t *testing.T) {
	withEmbedded(t, nil)
	if _, err := EmbeddedBinary(); err == nil {
		t.Fatal("want an error when no binary is embedded")
	}
	withEmbedded(t, []byte("x"))
	if b, err := EmbeddedBinary(); err != nil || string(b) != "x" {
		t.Fatalf("EmbeddedBinary = %q err=%v", b, err)
	}
}
