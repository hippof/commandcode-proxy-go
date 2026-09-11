// App is the backend of the tray-only manager. It owns the credential vault,
// the local proxy child process, and plan probing. There is no window and no
// frontend: the system tray menu plus native dialogs are the entire UI, so
// every method here is either a tray action or a piece of tray state.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v3/pkg/application"

	"commandcode-desktop/internal/applog"
	"commandcode-desktop/internal/ccdir"
	"commandcode-desktop/internal/creds"
	"commandcode-desktop/internal/gateway"
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
	gw      *gateway.Gateway

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
	app := &App{
		cfg:       cfg,
		vl:        vl,
		px:        proxy.NewManager(cfg),
		probers:   map[string]planProber{},
		newProber: func(baseURL string) planProber { return probe.New(baseURL) },
	}
	// The gateway resolves the proxy address per request (the proxy may fall
	// back to another port when its configured one is taken) and brings it up
	// on demand, so an editor configured against the gateway keeps working.
	gw, err := gateway.New(func() string { return app.px.BaseURL() }, app.activeKey, func() error {
		if app.px.Status().Running {
			return nil
		}
		return app.px.Start()
	})
	if err != nil {
		return nil, err
	}
	app.gw = gw
	return app, nil
}

// SetRuntime stores the Wails app handle (needed for native dialogs).
func (a *App) SetRuntime(r *wailsruntime.App) { a.runtime = r }

// PrepareProxy extracts the embedded proxy binary into the app-data directory
// at startup, so the first "start proxy" needs no extraction step. Failures
// are non-fatal: the proxy start path reports them with full context.
func (a *App) PrepareProxy() (string, error) {
	bin, err := proxy.EnsureExtracted()
	if err != nil {
		applog.Error("proxy", err, "action", "extract-embedded")
		return "", err
	}
	applog.Info("proxy", "内置代理已释放", "path", bin)
	return bin, nil
}

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

	GatewayRunning bool
	GatewayAddr    string
	GatewayTarget  string // proxy address the gateway forwards to
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
	if gs := a.GatewayStatus(); gs.Running {
		st.GatewayRunning = true
		st.GatewayAddr = gs.Addr
		if a.gw != nil {
			st.GatewayTarget = a.gw.Upstream()
		}
	}
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
		applog.Error("vault", err, "action", "save", "user", auth.UserName)
		return vault.Account{}, false, err
	}
	applog.Info("vault", "已保存登录凭证", "account", auth.UserName, "userId", auth.UserID, "new", lookErr != nil)
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
		applog.Error("vault", err, "action", "switch", "account", auth.UserName)
		return err
	}
	_, _ = settings.EnsureKeyHelper()
	applog.Info("vault", "已切换账号", "account", auth.UserName, "userId", auth.UserID, "maskedKey", creds.MaskKey(auth.APIKey))
	return nil
}

// Deactivate removes the CLI credential and the exported key (the vault copy
// is untouched).
func (a *App) Deactivate() error {
	if err := ccdir.Remove(ccdir.AuthFileName); err != nil && !os.IsNotExist(err) {
		applog.Error("vault", err, "action", "deactivate")
		return err
	}
	_ = a.vl.ClearActive()
	_ = settings.RemoveActiveKey()
	applog.Info("vault", "已停用当前登录")
	return nil
}

// DeleteFromVault permanently drops one saved credential. The credential
// currently installed for the CLI is protected, as is the recorded active one.
func (a *App) DeleteFromVault(id string) error {
	st := a.TrayState()
	if st.Protected(id) {
		err := fmt.Errorf("这个账号正在使用中，请先「停用当前登录」或切换到别的账号再删除")
		applog.Warn("vault", "拒绝删除在用账号", "account", id)
		return err
	}
	if err := a.vl.Remove(id); err != nil {
		applog.Error("vault", err, "action", "delete", "account", id)
		return err
	}
	applog.Info("vault", "已从保管库删除账号", "account", id)
	return nil
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
			applog.Error("probe", err, "account", name)
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
			applog.Warn("probe", "账号无额度", "account", name, "reason", res.Reason)
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

// ------------------------------------------------------------------ gateway

// activeKey resolves the credential the gateway injects: the account the CLI is
// logged in with, else the exported key file. Reading the credential file (not
// a cached value) is what makes a tray switch — or a manual cmdc login — take
// effect on the very next request.
func (a *App) activeKey() (string, error) {
	if auth, _, err := ccdir.ReadAuth(); err == nil && auth.APIKey != "" {
		return auth.APIKey, nil
	}
	if p, err := settings.ActiveKeyPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			if k := strings.TrimSpace(string(b)); k != "" {
				return k, nil
			}
		}
	}
	applog.Warn("gateway", "请求时没有激活账号")
	return "", errors.New("没有激活的账号：请在托盘里「切换」一个账号，或用 cmdc login 登录后「保存当前登录凭证」")
}

// LogDir is the daily-log folder ("" when file logging is unavailable).
func (a *App) LogDir() string { return applog.Path() }

// OpenLogDir reveals the log folder in the file manager.
func (a *App) OpenLogDir() error { return openInFileManager(applog.Path()) }

// ProxyAutoOn reports the persisted auto-start preference for the proxy.
func (a *App) ProxyAutoOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.ProxyAuto != "off"
}

// SetProxyAuto persists whether the proxy starts with the app.
func (a *App) SetProxyAuto(on bool) error {
	a.mu.Lock()
	if on {
		a.cfg.ProxyAuto = "on"
	} else {
		a.cfg.ProxyAuto = "off"
	}
	cfg := a.cfg
	a.mu.Unlock()
	return settings.Save(cfg)
}

// EnsureProxyRunning starts the proxy when needed and waits until it answers,
// returning the address it actually listens on (may differ from the configured
// port when that one was taken).
func (a *App) EnsureProxyRunning() (string, error) {
	if !a.px.Status().Running {
		if err := a.StartProxy(); err != nil {
			return "", err
		}
	}
	st := a.px.Status()
	if !st.Running {
		return "", fmt.Errorf("本地代理未就绪（%s 无响应）", st.BaseURL)
	}
	return st.BaseURL, nil
}

// StartGateway starts the credential-injecting gateway editors connect to. The
// proxy comes first: the gateway is only started once a live proxy address is
// known, so it can never come up pointing at nothing.
func (a *App) StartGateway() error {
	if a.gw == nil {
		return errors.New("网关未初始化")
	}
	upstream, err := a.EnsureProxyRunning()
	if err != nil {
		err = fmt.Errorf("IDE 网关需要先有可用的本地代理：%w", err)
		applog.Error("gateway", err, "action", "start", "stage", "await-proxy")
		return err
	}
	if err := a.gw.Start(a.cfg.GatewayHost, a.cfg.GatewayPort); err != nil {
		applog.Error("gateway", err, "action", "start", "host", a.cfg.GatewayHost, "port", a.cfg.GatewayPort, "upstream", upstream)
		return err
	}
	applog.Info("gateway", "已启动", "addr", a.gw.Addr(), "upstream", upstream)
	return nil
}

// StartServices brings the services up in order (proxy first, then the
// gateway) honouring the auto-start preferences, and returns a human summary.
// The first failure stops the sequence and is returned with it.
func (a *App) StartServices() ([]string, error) {
	lines := []string{}
	proxyAddr := ""
	if a.ProxyAutoOn() {
		addr, err := a.EnsureProxyRunning()
		if err != nil {
			if a.GatewayAutoOn() {
				applog.Warn("gateway", "因为代理没起来，跳过 IDE 网关的自动启动")
			}
			return lines, fmt.Errorf("本地代理启动失败：%w", err)
		}
		proxyAddr = addr
		lines = append(lines, "本地代理："+addr)
	} else {
		applog.Info("proxy", "未随应用启动（设置里为 off）")
	}

	if a.GatewayAutoOn() {
		if err := a.StartGateway(); err != nil {
			return lines, err
		}
		lines = append(lines, fmt.Sprintf("IDE 网关：%s → %s", a.gw.Addr(), a.gw.Upstream()))
	} else {
		applog.Info("gateway", "未随应用启动（设置里为 off）")
	}
	_ = proxyAddr
	return lines, nil
}

// StopGateway stops it (idempotent).
func (a *App) StopGateway() error {
	if a.gw == nil {
		return nil
	}
	if err := a.gw.Stop(); err != nil {
		applog.Error("gateway", err, "action", "stop")
		return err
	}
	applog.Info("gateway", "已停止")
	return nil
}

// GatewayStatus reports whether it is listening.
func (a *App) GatewayStatus() gateway.Status {
	if a.gw == nil {
		return gateway.Status{}
	}
	return a.gw.Status()
}

// SetGatewayAuto persists whether the gateway starts with the app.
func (a *App) SetGatewayAuto(on bool) error {
	a.mu.Lock()
	if on {
		a.cfg.GatewayAuto = "on"
	} else {
		a.cfg.GatewayAuto = "off"
	}
	cfg := a.cfg
	a.mu.Unlock()
	return settings.Save(cfg)
}

// GatewayAutoOn reports the persisted auto-start preference.
func (a *App) GatewayAutoOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.GatewayAuto != "off"
}

// GatewayInfoText is the snippet users paste into an editor's provider config.
func (a *App) GatewayInfoText() string {
	addr := a.GatewayStatus().Addr
	if addr == "" {
		addr = fmt.Sprintf("http://%s:%d", a.cfg.GatewayHost, a.cfg.GatewayPort)
	}
	return "Command Code 本地网关（key 随便填，账号由托盘决定）\n" +
		"Anthropic 协议 baseURL: " + addr + "\n" +
		"OpenAI 兼容 baseURL:    " + addr + "/v1\n" +
		"模型 ID 例：deepseek/deepseek-v4-flash、deepseek/deepseek-v4-pro、moonshotai/kimi-k3"
}

// CopyToClipboard puts text on the system clipboard (no-op without a runtime).
func (a *App) CopyToClipboard(text string) bool {
	if a.runtime == nil {
		return false
	}
	return a.runtime.Clipboard.SetText(text)
}

// OpenVaultDir reveals the credential folder in the file manager (handy for
// renaming or deleting saved accounts by hand).
func (a *App) OpenVaultDir() error { return openInFileManager(a.vl.Root()) }

// openInFileManager opens dir with the platform file manager.
func openInFileManager(dir string) error {
	if dir == "" {
		return errors.New("目录不可用")
	}
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
