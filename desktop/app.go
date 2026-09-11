// App is the backend of the tray-only manager. It owns the credential vault,
// the local proxy child process, and plan probing. There is no window and no
// frontend: the system tray menu plus native dialogs are the entire UI, so
// every method here is either a tray action or a piece of tray state.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v3/pkg/application"

	"commandcode-desktop/internal/ccdir"
	"commandcode-desktop/internal/creds"
	"commandcode-desktop/internal/probe"
	"commandcode-desktop/internal/proxy"
	"commandcode-desktop/internal/settings"
	"commandcode-desktop/internal/vault"
)

// proxyCtl is the slice of the proxy manager the app uses. It is an interface
// so tests can drive the tray without spawning a real child process —
// *proxy.Manager satisfies it.
type proxyCtl interface {
	Status() proxy.State
	Start() error
	Stop() error
	BaseURL() string
}

// planProber probes one account key; *probe.Prober satisfies it.
type planProber interface {
	Plan(ctx context.Context, apiKey string, models []string) (*probe.Result, error)
}

// App holds long-lived managers. It is constructed once in main.
type App struct {
	runtime *wailsruntime.App
	vl      *vault.Vault
	px      proxyCtl

	mu        sync.Mutex
	cfg       settings.Config
	probers   map[string]planProber
	newProber func(baseURL string) planProber
}

// NewApp wires the vault, proxy manager and settings.
func NewApp() (*App, error) {
	cfg, err := settings.Load()
	if err != nil {
		return nil, err
	}
	root, err := cfg.ResolveVaultRoot()
	if err != nil {
		return nil, err
	}
	vl, err := vault.Open(root)
	if err != nil {
		return nil, err
	}
	return &App{
		cfg:       cfg,
		vl:        vl,
		px:        proxy.NewManager(cfg),
		probers:   map[string]planProber{},
		newProber: func(baseURL string) planProber { return probe.New(baseURL) },
	}, nil
}

// SetRuntime stores the Wails app handle (needed for native dialogs).
func (a *App) SetRuntime(r *wailsruntime.App) { a.runtime = r }

// PrepareProxy extracts the embedded proxy binary into the app-data directory
// at startup, so the first "start proxy" needs no extraction step. Failures
// are non-fatal: the proxy start path reports them with full context.
func (a *App) PrepareProxy() (string, error) { return proxy.EnsureExtracted() }

// RecoverInterruptedSwap undoes a credential-directory swap left behind by an
// older build that orchestrated logins itself. Called once at startup.
func (a *App) RecoverInterruptedSwap() bool { return ccdir.RecoverInterruptedSwap() }

// ------------------------------------------------------------------- state

// CurrentCLI describes the credential currently installed for the CLI.
type CurrentCLI struct {
	Present bool
	UserID  string
	Name    string
	Masked  string
}

// CurrentCLI reads ~/.commandcode/auth.json (the credential the CLI and any
// consumer will actually use right now).
func (a *App) CurrentCLI() CurrentCLI {
	auth, _, err := ccdir.ReadAuth()
	if err != nil {
		return CurrentCLI{}
	}
	return CurrentCLI{
		Present: true,
		UserID:  creds.SafeID(auth.UserID),
		Name:    auth.UserName,
		Masked:  creds.MaskKey(auth.APIKey),
	}
}

// Accounts lists the vault, newest display order (userName).
func (a *App) Accounts() []vault.Account {
	list, err := a.vl.List()
	if err != nil {
		return nil
	}
	return list
}

// ActiveID is the account id recorded as activated.
func (a *App) ActiveID() string { return a.vl.ActiveID() }

// VaultDir is the folder holding the saved credentials.
func (a *App) VaultDir() string { return a.vl.Root() }

// TrayState is everything the menu needs to render itself *and* to decide
// which entries are actionable, so the tray never offers a click that is
// guaranteed to fail.
type TrayState struct {
	LoggedIn      bool // a credential is installed in the CLI dir
	CurrentName   string
	CurrentID     string
	CurrentMasked string

	Accounts []vault.Account
	ActiveID string // last account we activated

	ProxyRunning  bool
	ProxyExternal bool
	ProxyBaseURL  string
	ProxyCanStart bool // already running, or the embedded binary is on disk
}

// TrayState snapshots the current state for menu rendering.
func (a *App) TrayState() TrayState {
	st := TrayState{
		Accounts: a.Accounts(),
		ActiveID: a.vl.ActiveID(),
	}
	if cur := a.CurrentCLI(); cur.Present {
		st.LoggedIn = true
		st.CurrentName = cur.Name
		st.CurrentID = cur.UserID
		st.CurrentMasked = cur.Masked
	}
	ps := a.px.Status()
	st.ProxyRunning = ps.Running
	st.ProxyExternal = ps.External
	st.ProxyBaseURL = ps.BaseURL
	st.ProxyCanStart = ps.Running || ps.BinaryExists
	return st
}

// Protected reports whether an account is currently in use (so deleting its
// saved copy would be a mistake).
func (st TrayState) Protected(id string) bool {
	return id != "" && (id == st.ActiveID || id == st.CurrentID)
}

// Tooltip is the tray hover text.
func (a *App) Tooltip() string {
	cur := a.CurrentCLI()
	if !cur.Present {
		return "Command Code 账号 · 未登录"
	}
	return "Command Code 账号 · 当前：" + displayNameOf(cur.Name, cur.UserID)
}

func displayNameOf(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// PlanLabel renders an account's cached plan probe for a menu label.
func PlanLabel(ac vault.Account) string {
	if ac.Plan == nil {
		return "未探测"
	}
	if ac.Plan.Blocked {
		return "无额度"
	}
	return fmt.Sprintf("可用模型 %d", len(ac.Plan.Allowed))
}

// AccountLabel is the menu text for one saved account.
func AccountLabel(ac vault.Account) string {
	name := displayNameOf(ac.UserName, ac.ID)
	if ac.Note != "" {
		name += "（" + ac.Note + "）"
	}
	return fmt.Sprintf("%s — %s", name, PlanLabel(ac))
}

// ------------------------------------------------------------------ actions

// SaveCurrentCredential stores the credential the CLI is logged in with
// (reads auth.json) into the vault. It is the counterpart of the manual
// `cmdc login` flow: log in anywhere, then save.
func (a *App) SaveCurrentCredential() (vault.Account, bool, error) {
	auth, raw, err := ccdir.ReadAuth()
	if err != nil {
		if os.IsNotExist(err) {
			return vault.Account{}, false, fmt.Errorf("没有找到登录凭证（%s 不存在）。请先在终端运行 cmdc login（mac/linux：cmd login）完成登录，再点「保存当前登录凭证」。", ccdir.AuthFileName)
		}
		return vault.Account{}, false, fmt.Errorf("读取登录凭证失败：%w", err)
	}
	_, lookErr := a.vl.Account(auth.UserID)
	ac, err := a.vl.Put(auth, raw)
	if err != nil {
		return vault.Account{}, false, err
	}
	return *ac, lookErr != nil, nil
}

// SwitchTo activates a saved credential: it becomes the CLI's auth.json, the
// exported active.key (for apiKeyHelper consumers), and the recorded current.
func (a *App) SwitchTo(id string) error {
	raw, err := a.vl.Auth(id)
	if err != nil {
		return fmt.Errorf("保管库里没有这个账号：%w", err)
	}
	auth, err := creds.Parse(raw)
	if err != nil {
		return err
	}
	if err := ccdir.Install(raw); err != nil {
		return err
	}
	if err := a.vl.SetActive(id); err != nil {
		return err
	}
	if err := settings.WriteActiveKey(auth.APIKey); err != nil {
		return err
	}
	_, _ = settings.EnsureKeyHelper()
	return nil
}

// Deactivate removes the CLI credential and the exported key (the vault copy
// is untouched).
func (a *App) Deactivate() error {
	if err := ccdir.Remove(ccdir.AuthFileName); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = a.vl.ClearActive()
	_ = settings.RemoveActiveKey()
	return nil
}

// DeleteFromVault permanently drops one saved credential. The credential
// currently installed for the CLI is protected, as is the recorded active one.
func (a *App) DeleteFromVault(id string) error {
	st := a.TrayState()
	if st.Protected(id) {
		return fmt.Errorf("这个账号正在使用中，请先「停用当前登录」或切换到别的账号再删除")
	}
	return a.vl.Remove(id)
}

// RefreshPlans probes every saved credential through the local proxy (which
// it starts if needed) and caches the verdict per account.
func (a *App) RefreshPlans() (string, error) {
	accounts := a.Accounts()
	if len(accounts) == 0 {
		return "", fmt.Errorf("保管库还是空的，先「保存当前登录凭证」")
	}
	if !a.px.Status().Running {
		if err := a.px.Start(); err != nil {
			return "", fmt.Errorf("套餐探测走本地代理，但代理启动失败：%w", err)
		}
	}
	base := a.px.BaseURL()
	models := a.cfg.ProbeModels
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	lines := make([]string, 0, len(accounts))
	for _, ac := range accounts {
		raw, err := a.vl.Auth(ac.ID)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s：读取失败", displayNameOf(ac.UserName, ac.ID)))
			continue
		}
		auth, err := creds.Parse(raw)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s：凭证无效", displayNameOf(ac.UserName, ac.ID)))
			continue
		}
		res, err := a.prober(base).Plan(ctx, auth.APIKey, models)
		name := displayNameOf(ac.UserName, ac.ID)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s：探测失败（%v）", name, err))
			continue
		}
		plan := &vault.PlanInfo{
			ProbeAt: time.Now(), Allowed: res.Allowed, Denied: res.Denied,
			QuotaHeaders: res.QuotaHeaders, Blocked: res.Blocked, Reason: res.Reason,
		}
		if err := a.vl.PutPlan(ac.ID, plan); err != nil {
			lines = append(lines, fmt.Sprintf("%s：缓存失败（%v）", name, err))
			continue
		}
		if res.Blocked {
			lines = append(lines, fmt.Sprintf("%s：⛔ %s", name, res.Reason))
		} else {
			lines = append(lines, fmt.Sprintf("%s：可用 %d 个模型（不可用 %d）", name, len(res.Allowed), len(res.Denied)))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (a *App) prober(baseURL string) planProber {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.probers[baseURL]
	if !ok {
		p = a.newProber(baseURL)
		a.probers[baseURL] = p
	}
	return p
}

// -------------------------------------------------------------------- proxy

// ProxyRunning reports the child-process state for the tray checkmark.
func (a *App) ProxyRunning() bool { return a.px.Status().Running }

// ProxyBaseURL is the local endpoint consumers should target.
func (a *App) ProxyBaseURL() string { return a.px.BaseURL() }

// StartProxy builds (if needed) and starts the local proxy.
func (a *App) StartProxy() error { return a.px.Start() }

// StopProxy stops the proxy this app started (external instances untouched).
func (a *App) StopProxy() error { return a.px.Stop() }

// OpenDashboard opens the proxy's request-log page in the browser.
func (a *App) OpenDashboard() {
	if a.runtime != nil {
		_ = a.runtime.Browser.OpenURL(a.px.BaseURL() + "/admin")
	}
}

// OpenVaultDir reveals the credential folder in the file manager (handy for
// renaming or deleting saved accounts by hand).
func (a *App) OpenVaultDir() error {
	dir := a.vl.Root()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}

// Quit exits the app (stopping a proxy it manages via the shutdown hook).
func (a *App) Quit() {
	if a.runtime != nil {
		a.runtime.Quit()
	}
}

// sortedAccounts is Accounts ordered for menu display.
func (a *App) sortedAccounts() []vault.Account {
	list := a.Accounts()
	sort.SliceStable(list, func(i, j int) bool {
		return displayNameOf(list[i].UserName, list[i].ID) < displayNameOf(list[j].UserName, list[j].ID)
	})
	return list
}
