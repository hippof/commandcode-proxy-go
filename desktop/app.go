// App is the single service bound to the Wails frontend: it orchestrates the
// vault, the credential directory, the CLI login flow, the local proxy child
// process, and plan probing. All key material stays server-side; the frontend
// only ever sees masked keys.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v3/pkg/application"

	"commandcode-desktop/internal/ccdir"
	"commandcode-desktop/internal/creds"
	"commandcode-desktop/internal/login"
	"commandcode-desktop/internal/probe"
	"commandcode-desktop/internal/proxy"
	"commandcode-desktop/internal/settings"
	"commandcode-desktop/internal/vault"
)

// AccountView is one vault account plus its activation state.
type AccountView struct {
	vault.Account
	Active bool `json:"active"`
}

// Snapshot is everything the UI needs in one refresh.
type Snapshot struct {
	Accounts    []AccountView `json:"accounts"`
	ActiveID    string        `json:"activeId"`
	DirUser     string        `json:"dirUser"` // userId currently in ~/.commandcode ("" = none)
	DirUserName string        `json:"dirUserName"`
	DirKeyHint  string        `json:"dirKeyHint"` // masked key of the dir account, if known
	Proxy       proxy.State   `json:"proxy"`
	Login       login.Status  `json:"login"`
	ProbeModels []string      `json:"probeModels"`
	KeyHelper   string        `json:"keyHelper"` // path to the apiKeyHelper script
	CloseAction string        `json:"closeAction"`
}

// App holds long-lived managers. It is constructed once in main.
type App struct {
	runtime *wailsruntime.App
	vl      *vault.Vault
	px      *proxy.Manager
	sess    *login.Session

	mu        sync.Mutex
	win       wailsruntime.Window
	cfg       settings.Config
	probers   map[string]*probe.Prober // per base URL
	onChanged func()                   // tray refresh hook (set from main)
}

// NewApp wires the services and recovers from an interrupted login.
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
	if login.RecoverAfterRestart() {
		// Best-effort breadcrumb; never contains secrets.
		fmt.Println("recovered credential directory from an interrupted login")
	}
	a := &App{
		cfg:     cfg,
		vl:      vl,
		px:      proxy.NewManager(cfg),
		probers: map[string]*probe.Prober{},
	}
	a.sess = login.NewSession(a.emitLogin)
	a.sess.SetSuccessHook(a.archiveLoginResult)
	return a, nil
}

// SetRuntime stores the Wails app for event emission.
func (a *App) SetRuntime(r *wailsruntime.App) { a.runtime = r }

// ShowWindow brings the main window back from the tray.
func (a *App) ShowWindow() {
	a.mu.Lock()
	w := a.win
	a.mu.Unlock()
	if w != nil {
		_ = w.Show()
		w.Focus()
	}
}

// EmitError pushes a toast message to the frontend.
func (a *App) EmitError(msg string) { a.emit(EventError, msg) }

// ActiveAccountID returns the last activated account id.
func (a *App) ActiveAccountID() string { return a.vl.ActiveID() }

// MainWindow exposes the primary window for tray wiring.
func (a *App) MainWindow() wailsruntime.Window {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.win
}

// SetMainWindow stores the window created in main.go.
func (a *App) SetMainWindow(w wailsruntime.Window) {
	a.mu.Lock()
	a.win = w
	a.mu.Unlock()
}

func (a *App) emit(s string, data any) {
	if a.runtime != nil {
		a.runtime.Event.Emit(s, data)
	}
}

func (a *App) emitLogin(st login.Status) {
	a.emit(EventLogin, st)
	a.emit(EventChanged, true)
}

// Event names registered with Wails (mirrored in JS).
const (
	EventLogin   = "login:status"
	EventChanged = "app:changed"
	EventError   = "app:error"
)

func (a *App) archiveLoginResult(auth *creds.Auth, raw []byte) (bool, error) {
	_, lookErr := a.vl.Account(auth.UserID)
	isNew := lookErr != nil // absent (or unreadable) => new entry
	if _, err := a.vl.Put(auth, raw); err != nil {
		return false, err
	}
	return isNew, nil
}

// ---------------------------------------------------------------- API surface

// Snapshot returns the full UI state.
func (a *App) Snapshot() (*Snapshot, error) {
	accounts, err := a.vl.List()
	if err != nil {
		return nil, err
	}
	activeID := a.vl.ActiveID()
	views := make([]AccountView, 0, len(accounts))
	for _, ac := range accounts {
		views = append(views, AccountView{Account: ac, Active: ac.ID == activeID})
	}
	snap := &Snapshot{
		Accounts:    views,
		ActiveID:    activeID,
		Proxy:       a.px.Status(),
		Login:       a.sess.State(),
		ProbeModels: a.cfg.ProbeModels,
	}
	if auth, _, err := ccdir.ReadAuth(); err == nil {
		snap.DirUser = creds.SafeID(auth.UserID)
		snap.DirUserName = auth.UserName
		snap.DirKeyHint = creds.MaskKey(auth.APIKey)
	}
	if p, err := settings.EnsureKeyHelper(); err == nil {
		snap.KeyHelper = p
	}
	snap.CloseAction = a.cfg.CloseAction
	return snap, nil
}

// ImportCurrent archives the account currently logged in via the CLI.
func (a *App) ImportCurrent() error {
	auth, raw, err := ccdir.ReadAuth()
	if err != nil {
		if isNotExist(err) {
			return fmt.Errorf("当前没有已登录的 CLI 账号（%s 不存在）。请先在终端运行 `cmdc login`（mac/linux：`cmd login`）完成登录，或点「登录新账号」由本程序打开终端。", ccdir.AuthFileName)
		}
		return fmt.Errorf("读取当前 CLI 凭据失败：%w", err)
	}
	if _, err := a.vl.Put(auth, raw); err != nil {
		return err
	}
	a.emit(EventChanged, true)
	a.notifyUI()
	return nil
}

// Activate makes a vault account the current one everywhere: it installs the
// auth.json into the CLI directory, records the marker, and exports the key
// for apiKeyHelper consumers.
func (a *App) Activate(id string) error {
	raw, err := a.vl.Auth(id)
	if err != nil {
		return fmt.Errorf("account %q not found in vault", id)
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
	a.emit(EventChanged, true)
	a.notifyUI()
	return nil
}

// Deactivate removes the active credential (auth.json + exported key).
func (a *App) Deactivate() error {
	if err := ccdir.Remove(ccdir.AuthFileName); err != nil && !isNotExist(err) {
		return err
	}
	_ = a.vl.ClearActive()
	_ = settings.RemoveActiveKey()
	a.emit(EventChanged, true)
	a.notifyUI()
	return nil
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// SetNote updates an account's free-form remark.
func (a *App) SetNote(id, note string) error {
	if err := a.vl.SetNote(id, note); err != nil {
		return err
	}
	a.emit(EventChanged, true)
	return nil
}

// SetCloseAction configures whether the window X hides to tray or quits.
func (a *App) SetCloseAction(action string) error {
	if action != "tray" && action != "quit" {
		return fmt.Errorf("closeAction must be \"tray\" or \"quit\"")
	}
	a.cfg.CloseAction = action
	if err := settings.Save(a.cfg); err != nil {
		return err
	}
	a.notifyUI()
	return nil
}

// CloseActionIsTray reports the configured window-X behavior.
func (a *App) CloseActionIsTray() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.CloseAction != "quit"
}

// SetAccountsChangedHook lets main.go refresh the tray menu on any change.
func (a *App) SetAccountsChangedHook(fn func()) {
	a.mu.Lock()
	a.onChanged = fn
	a.mu.Unlock()
}

func (a *App) notifyUI() {
	a.mu.Lock()
	fn := a.onChanged
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Remove deletes an account from the vault (never the active auth.json file).
func (a *App) Remove(id string) error {
	if a.vl.ActiveID() != "" && id == a.vl.ActiveID() {
		return fmt.Errorf("deactivate this account before removing it")
	}
	if err := a.vl.Remove(id); err != nil {
		return err
	}
	a.emit(EventChanged, true)
	a.notifyUI()
	return nil
}

// StartLogin begins the browser sign-in flow for a NEW account. The active
// account is preserved.
func (a *App) StartLogin() error {
	return a.sess.Begin(a.cfg.CLICommand)
}

// AbortLogin cancels an in-progress login.
func (a *App) AbortLogin() { a.sess.Abort() }

// ------------------------------------------------------------------- proxy

// ProxyStatus returns the child-process state.
func (a *App) ProxyStatus() proxy.State { return a.px.Status() }

// StartProxy ensures the binary exists (building if needed) and runs it.
func (a *App) StartProxy() error {
	err := a.px.Start()
	a.notifyUI()
	return err
}

// StopProxy terminates the managed child process.
func (a *App) StopProxy() error {
	err := a.px.Stop()
	a.notifyUI()
	return err
}

// BuildProxy recompiles the proxy from the repository.
func (a *App) BuildProxy() error {
	_, err := a.px.Build()
	a.emit(EventChanged, true)
	return err
}

// OpenDashboard opens the proxy's /admin page in the system browser.
func (a *App) OpenDashboard() {
	if a.runtime != nil {
		_ = a.runtime.Browser.OpenURL(a.px.BaseURL() + "/admin")
	}
}

// -------------------------------------------------------------------- plans

// RefreshPlan probes a vault account's key against the configured models and
// caches the result. The local proxy must be running (it is the known-good
// translation layer).
func (a *App) RefreshPlan(id string) error {
	raw, err := a.vl.Auth(id)
	if err != nil {
		return fmt.Errorf("account %q not found", id)
	}
	auth, err := creds.Parse(raw)
	if err != nil {
		return err
	}
	st := a.px.Status()
	if !st.Running {
		// The proxy process is the known-good translation layer for
		// /alpha/generate; start it on demand rather than failing.
		if err := a.px.Start(); err != nil {
			return fmt.Errorf("plan checks go through the local proxy, and it failed to start: %w", err)
		}
		st = a.px.Status()
	}
	p := a.prober(st.BaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := p.Plan(ctx, auth.APIKey, a.cfg.ProbeModels)
	if err != nil {
		return err
	}
	plan := &vault.PlanInfo{ProbeAt: time.Now(), Allowed: res.Allowed, Denied: res.Denied, QuotaHeaders: res.QuotaHeaders, Blocked: res.Blocked, Reason: res.Reason}
	if err := a.vl.PutPlan(id, plan); err != nil {
		return err
	}
	a.emit(EventChanged, true)
	return nil
}

// ProxyRunning reports tray menu state.
func (a *App) ProxyRunning() bool {
	st := a.px.Status()
	return st.Running
}

func (a *App) prober(baseURL string) *probe.Prober {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.probers[baseURL]
	if !ok {
		p = probe.New(baseURL)
		a.probers[baseURL] = p
	}
	return p
}

// ------------------------------------------------------------------ settings

// SaveConfig updates host/port/probe settings and rebuilds the proxy manager.
func (a *App) SaveConfig(cfg settings.Config) error {
	def := settings.DefaultConfig()
	def.Merge(cfg) // cfg wins where set
	a.cfg = def
	if err := settings.Save(a.cfg); err != nil {
		return err
	}
	root, err := a.cfg.ResolveVaultRoot()
	if err != nil {
		return err
	}
	vl, err := vault.Open(root)
	if err != nil {
		return err
	}
	a.vl = vl
	a.px = proxy.NewManager(a.cfg)
	a.emit(EventChanged, true)
	a.notifyUI()
	return nil
}

// Catalog returns Command Code's global model list (cached per process).
func (a *App) Catalog() ([]probe.CatalogEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := probe.FetchCatalog(ctx, a.cfg.ModelsURL)
	if err != nil {
		return nil, err
	}
	return c.Models, nil
}
