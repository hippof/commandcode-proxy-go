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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"commandcode-desktop/internal/applog"
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

// canBind reports whether host:port is free right now.
func canBind(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// pickPort returns the port to run the proxy on, falling back to an OS-chosen
// free one when the configured port is taken. The second result reports the
// fallback so callers can log/announce it.
func pickPort(host string, want int) (int, bool) {
	if want <= 0 {
		want = 8787
	}
	if canBind(host, want) {
		return want, false
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return want, false // let the child fail loudly rather than guess
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, true
}

// healthOn reports whether a proxy answers /health at the given base URL.
func (m *Manager) healthOn(baseURL string) bool {
	resp, err := m.client.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode == http.StatusOK
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
			// Locked by a running proxy: keep using the older copy and let the
			// next launch pick the update up.
			applog.Warn("proxy", "内置代理更新失败（文件被占用），继续使用现有副本", "err", err.Error(), "path", target)
			return target, nil
		}
		applog.Error("proxy", err, "stage", "extract", "path", target)
		return "", err
	}
	_ = os.WriteFile(digestPath, []byte(want), 0o644)
	applog.Info("proxy", "已释放内置代理二进制", "path", target, "bytes", len(data))
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

	host, wantPort := "127.0.0.1", 8787
	if cfg.ProxyHost != "" {
		host = cfg.ProxyHost
	}
	if cfg.ProxyPort != 0 {
		wantPort = cfg.ProxyPort
	}
	// Someone (another instance, or the user's own proxy) already serves the
	// configured port: use it as-is instead of starting a second process.
	configured := fmt.Sprintf("http://%s:%d", host, wantPort)
	if m.healthOn(configured) {
		m.state.BaseURL = configured
		applog.Info("proxy", "配置端口上已有可用代理，直接复用", "addr", configured)
		return nil
	}
	port, fellBack := pickPort(host, wantPort)
	m.state.BaseURL = fmt.Sprintf("http://%s:%d", host, port)
	if fellBack {
		applog.Warn("proxy", "配置端口被占用，改用空闲端口启动",
			"wanted", wantPort, "using", port, "addr", m.state.BaseURL)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"COMMANDCODE_PROXY_HOST="+host,
		"COMMANDCODE_PROXY_PORT="+strconv.Itoa(port),
	)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = hideWindow()
	}
	if f, err := os.OpenFile(childLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		m.state.LastError = err.Error()
		applog.Error("proxy", err, "stage", "start", "bin", bin)
		return err
	}
	m.cmd = cmd
	m.state.BinaryPath = bin
	m.state.LastError = ""
	applog.Info("proxy", "已启动代理子进程", "bin", bin, "pid", cmd.Process.Pid, "addr", m.state.BaseURL)
	go func() {
		err := cmd.Wait()
		if err != nil {
			applog.Warn("proxy", "代理子进程退出", "pid", cmd.Process.Pid, "err", err.Error())
		} else {
			applog.Info("proxy", "代理子进程正常退出", "pid", cmd.Process.Pid)
		}
	}()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		m.refreshHealthLocked()
		if m.state.Running {
			return nil
		}
	}
	err := fmt.Errorf("代理已启动但 %s/health 无响应", m.state.BaseURL)
	applog.Error("proxy", err, "stage", "health")
	return err
}

// childLogPath is where the proxy's own output goes: the daily-log folder when
// available (kept as one continuous file, since the proxy writes its own
// structured lines), else the app-data directory.
func childLogPath() string {
	if dir := applog.Path(); dir != "" {
		return filepath.Join(dir, "commandcode-proxy.log")
	}
	if d, err := settings.Dir(); err == nil {
		return filepath.Join(d, "proxy.log")
	}
	return filepath.Join(os.TempDir(), "commandcode-proxy.log")
}

// Stop terminates the managed child (external proxies are left alone).
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}
	err := m.cmd.Process.Kill()
	pid := m.cmd.Process.Pid
	m.cmd = nil
	m.refreshHealthLocked()
	if err != nil {
		applog.Warn("proxy", "停止代理子进程失败", "pid", pid, "err", err.Error())
	} else {
		applog.Info("proxy", "已停止代理子进程", "pid", pid)
	}
	return err
}
