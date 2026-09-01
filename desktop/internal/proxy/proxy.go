// Package proxy manages the local commandcode-proxy child process: it can
// build the proxy from the fork's repository, start/stop it, and report
// health. The proxy itself is the unmodified upstream binary; this package
// only shells it in and out.
package proxy

import (
	"bytes"
	"encoding/json"
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

// State is the UI-facing status of the local proxy.
type State struct {
	Running      bool   `json:"running"`
	External     bool   `json:"external"` // health answered but we did not start it
	BaseURL      string `json:"baseUrl"`
	Version      string `json:"version"`
	BinaryPath   string `json:"binaryPath"`
	BinaryExists bool   `json:"binaryExists"`
	SourceRoot   string `json:"sourceRoot"`
	SourceFound  bool   `json:"sourceFound"`
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

	m.state.BinaryPath = m.resolveBinaryLocked(cfg)
	m.state.BinaryExists = m.state.BinaryPath != ""
	root, ok := findRepoRoot(cfg)
	m.state.SourceRoot, m.state.SourceFound = root, ok
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

// resolveBinaryLocked picks the binary: explicit config path > managed copy >
// repo build output. Returns "" when none exists yet.
func (m *Manager) resolveBinaryLocked(cfg settings.Config) string {
	if cfg.ProxyBinary != "" && fileExists(cfg.ProxyBinary) {
		return cfg.ProxyBinary
	}
	if p, err := managedBinaryPath(); err == nil && fileExists(p) {
		return p
	}
	if root, ok := findRepoRoot(cfg); ok {
		for _, name := range []string{"commandcode-proxy.exe", "commandcode-proxy"} {
			if p := filepath.Join(root, name); fileExists(p) {
				return p
			}
		}
	}
	return ""
}

// managedBinaryPath is the app's own build output under the app-data dir.
func managedBinaryPath() (string, error) {
	d, err := settings.Dir()
	if err != nil {
		return "", err
	}
	name := "commandcode-proxy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(d, name), nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// findRepoRoot locates the commandcode-proxy-go source tree: explicit config,
// else walk up from the executable (dev runs put the binary under desktop/bin).
func findRepoRoot(cfg settings.Config) (string, bool) {
	if cfg.ProxySource != "" && looksLikeRepo(cfg.ProxySource) {
		return cfg.ProxySource, true
	}
	if exe, err := os.Executable(); err == nil {
		for d := filepath.Dir(exe); ; {
			if looksLikeRepo(d) {
				return d, true
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return "", false
}

func looksLikeRepo(dir string) bool {
	if !fileExists(filepath.Join(dir, "go.mod")) || !fileExists(filepath.Join(dir, "cmd", "commandcode-proxy", "main.go")) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	return err == nil && strings.Contains(string(data), "commandcode-proxy")
}

// Build compiles the proxy from the source repo into the managed path.
func (m *Manager) Build() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, _ := settings.Load()
	return m.buildLocked(cfg)
}

// buildLocked performs the build; callers hold m.mu.
func (m *Manager) buildLocked(cfg settings.Config) (string, error) {
	root, ok := findRepoRoot(cfg)
	if !ok {
		return "", fmt.Errorf("proxy source repo not found — set its path in settings")
	}
	out, err := managedBinaryPath()
	if err != nil {
		return "", err
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("go toolchain not on PATH: %w", err)
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", goBin, "build", "-o", out, "./cmd/commandcode-proxy")
	} else {
		cmd = exec.Command(goBin, "build", "-o", out, "./cmd/commandcode-proxy")
	}
	cmd.Dir = root
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = hideWindow()
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		m.state.LastError = truncate(stderr.String(), 600)
		return "", fmt.Errorf("build failed: %v", err)
	}
	m.state.BinaryPath = out
	m.state.LastError = ""
	return out, nil
}

// Start launches the proxy child process. It builds the managed binary when
// none exists. An external proxy already answering on the port is reported by
// Status as External, not killed.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil && (m.cmd.ProcessState == nil || !m.cmd.ProcessState.Exited()) {
		return nil // already ours
	}
	cfg, _ := settings.Load()
	bin := m.resolveBinaryLocked(cfg)
	if bin == "" {
		out, err := m.buildLocked(cfg)
		if err != nil {
			return err
		}
		bin = out
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
	return fmt.Errorf("proxy started but /health never answered on %s", m.state.BaseURL)
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
