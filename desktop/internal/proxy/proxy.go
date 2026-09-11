// Package proxy manages the local commandcode-proxy child process.
//
// The proxy binary is EMBEDDED in this application (see proxybin/) and
// extracted to the app-data directory on demand, so a released desktop build
// is a single self-contained executable: no Go toolchain and no proxy source
// tree on the target machine. Updating the proxy means rebuilding the desktop
// app — which is exactly the release flow this app is built for.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"commandcode-desktop/internal/settings"
)

// embeddedBytes holds the proxy binary shipped inside this executable. It is
// injected by package main (which owns the embed directive over proxybin/),
// so this package stays free of build-time asset expectations.
var embeddedBytes []byte

// SetEmbeddedBinary supplies the shipped proxy binary. Passing nil/empty
// leaves the build without one (a checkout that has not produced it yet).
func SetEmbeddedBinary(b []byte) { embeddedBytes = b }

// BinaryName is the proxy executable's file name on this platform.
func BinaryName() string { return binaryName() }

// State is the UI-facing status of the local proxy.
type State struct {
	Running      bool   `json:"running"`
	External     bool   `json:"external"` // health answered but we did not start it
	BaseURL      string `json:"baseUrl"`
	Version      string `json:"version"`
	BinaryPath   string `json:"binaryPath"`
	BinaryExists bool   `json:"binaryExists"`
	PID          int    `json:"pid"`
	LastError    string `json:"lastError"`
}

// Manager owns the child process (at most one at a time).
type Manager struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	state  State
	client *http.Client
}

// NewManager creates a manager for cfg's host/port.
func NewManager(cfg settings.Config) *Manager {
	host, port := cfg.ProxyHost, cfg.ProxyPort
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 8787
	}
	return &Manager{
		state:  State{BaseURL: fmt.Sprintf("http://%s:%d", host, port)},
		client: &http.Client{Timeout: 4 * time.Second},
	}
}

// BaseURL is the proxy's HTTP base (also used by the prober).
func (m *Manager) BaseURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.BaseURL
}

// Status refreshes health + binary facts and returns the state.
func (m *Manager) Status() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, _ := settings.Load()
	m.refreshHealthLocked()

	bin := existingManagedBinary(cfg)
	m.state.BinaryPath = bin
	m.state.BinaryExists = bin != ""
	m.state.PID = 0
	if m.cmd != nil && m.cmd.Process != nil && m.state.Running {
		m.state.PID = m.cmd.Process.Pid
	}
	return m.state
}

func (m *Manager) refreshHealthLocked() {
	running := false
	version := m.state.Version
	if resp, err := m.client.Get(m.state.BaseURL + "/health"); err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusOK {
			running = true
			var h struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			if json.Unmarshal(body, &h) == nil && h.Version != "" {
				version = h.Version
			}
		}
	}
	if m.cmd != nil && m.cmd.Process != nil && m.cmd.ProcessState != nil && m.cmd.ProcessState.Exited() {
		running = false // our child died
	}
	m.state.Running = running
	m.state.External = running && m.cmd == nil
	m.state.Version = version
}

// binaryName is the file name of the proxy executable on this platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "commandcode-proxy.exe"
	}
	return "commandcode-proxy"
}

// ManagedBinaryPath is where the proxy lives inside the app-data directory.
func ManagedBinaryPath() (string, error) {
	d, err := settings.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, binaryName()), nil
}

// existingManagedBinary returns a runnable binary without extracting
// anything: an explicit override wins, else the previously extracted copy.
func existingManagedBinary(cfg settings.Config) string {
	if cfg.ProxyBinary != "" && fileExists(cfg.ProxyBinary) {
		return cfg.ProxyBinary
	}
	if p, err := ManagedBinaryPath(); err == nil && fileExists(p) {
		return p
	}
	return ""
}

// EmbeddedBinary returns the proxy bytes shipped inside this executable.
func EmbeddedBinary() ([]byte, error) {
	if len(embeddedBytes) == 0 {
		return nil, errors.New("这个构建里没有嵌入代理二进制")
	}
	return embeddedBytes, nil
}

// ExtractEmbedded writes the embedded proxy into dir, replacing an older copy
// by content digest. A binary that is currently running cannot be replaced on
// Windows; in that case the existing file is reused (the update simply lands
// on the next launch).
func ExtractEmbedded(dir string) (string, error) {
	data, err := EmbeddedBinary()
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, binaryName())
	digestPath := target + ".sha256"
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])

	if have, err := os.ReadFile(digestPath); err == nil && strings.TrimSpace(string(have)) == want && fileExists(target) {
		return target, nil // already current
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomic(target, data, 0o755); err != nil {
		if fileExists(target) {
			return target, nil // locked by a running proxy: keep using it
		}
		return "", err
	}
	_ = os.WriteFile(digestPath, []byte(want), 0o644)
	return target, nil
}

// EnsureExtracted extracts the embedded proxy into the app-data dir (a no-op
// once the copy matches the embedded digest) and returns its path.
func EnsureExtracted() (string, error) {
	d, err := settings.Dir()
	if err != nil {
		return "", err
	}
	return ExtractEmbedded(d)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".proxybin-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(name, mode); err != nil {
			return err
		}
	}
	return os.Rename(name, path)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Start launches the proxy child process, extracting the embedded binary
// first when needed. An external proxy already answering on the port is
// reported by Status as External, not killed.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil && (m.cmd.ProcessState == nil || !m.cmd.ProcessState.Exited()) {
		return nil // already ours
	}
	cfg, _ := settings.Load()

	bin := existingManagedBinary(cfg)
	if cfg.ProxyBinary == "" {
		// Refresh the managed copy from the embedded binary (a no-op while the
		// digest matches). If extraction fails but an older copy is on disk,
		// that copy is still usable — never fail a start over a stale binary.
		p, err := EnsureExtracted()
		switch {
		case err == nil:
			bin = p
		case bin != "":
			m.state.LastError = err.Error()
		default:
			err = fmt.Errorf("无法释放内置代理二进制（%v）—— 请重新打包桌面程序，或在配置里设置 proxyBinary 指向一个代理可执行文件", err)
			m.state.LastError = err.Error()
			return err
		}
	}
	if bin == "" {
		err := errors.New("没有可用的代理二进制：内置资源缺失，且配置的 proxyBinary 不存在")
		m.state.LastError = err.Error()
		return err
	}

	host, port := "127.0.0.1", "8787"
	if cfg.ProxyHost != "" {
		host = cfg.ProxyHost
	}
	if cfg.ProxyPort != 0 {
		port = fmt.Sprint(cfg.ProxyPort)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"COMMANDCODE_PROXY_HOST="+host,
		"COMMANDCODE_PROXY_PORT="+port,
	)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = hideWindow()
	}
	if d, err := settings.Dir(); err == nil {
		if f, err := os.OpenFile(filepath.Join(d, "proxy.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
	}
	if err := cmd.Start(); err != nil {
		m.state.LastError = err.Error()
		return err
	}
	m.cmd = cmd
	m.state.BinaryPath = bin
	m.state.LastError = ""
	go func() { _ = cmd.Wait() }()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		m.refreshHealthLocked()
		if m.state.Running {
			return nil
		}
	}
	return fmt.Errorf("代理已启动但 %s/health 无响应", m.state.BaseURL)
}

// Stop terminates the managed child (external proxies are left alone).
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}
	err := m.cmd.Process.Kill()
	m.cmd = nil
	m.refreshHealthLocked()
	return err
}
