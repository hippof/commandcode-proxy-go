package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"

	"commandcode-desktop/internal/ccdir"
	"commandcode-desktop/internal/creds"
	"commandcode-desktop/internal/probe"
	"commandcode-desktop/internal/proxy"
	"commandcode-desktop/internal/settings"
	"commandcode-desktop/internal/vault"
)

// ---------------------------------------------------------------- test doubles

// fakeNotifier records what the tray tells the user; Ask optionally confirms
// immediately, or leaves the callback for the test to invoke.
type fakeNotifier struct {
	infos   []string
	fails   []string
	asks    []string
	confirm bool
	lastAsk func()
}

func (f *fakeNotifier) Info(title, msg string) { f.infos = append(f.infos, title+"｜"+msg) }

func (f *fakeNotifier) Fail(title string, err error) {
	f.fails = append(f.fails, title+"｜"+err.Error())
}

func (f *fakeNotifier) Ask(title, msg, _ string, onYes func()) {
	f.asks = append(f.asks, title+"｜"+msg)
	f.lastAsk = onYes
	if f.confirm {
		onYes()
	}
}

// fakeProxy stands in for the proxy manager: no process is ever started.
type fakeProxy struct {
	running      bool
	external     bool
	binaryExists bool
	base         string
	starts       int
	stops        int
	startErr     error
}

func (p *fakeProxy) Status() proxy.State {
	return proxy.State{
		Running:      p.running,
		External:     p.external,
		BaseURL:      p.BaseURL(),
		BinaryExists: p.binaryExists,
	}
}

func (p *fakeProxy) Start() error {
	if p.startErr != nil {
		return p.startErr
	}
	p.starts++
	p.running = true
	return nil
}

func (p *fakeProxy) Stop() error { p.stops++; p.running = false; return nil }

func (p *fakeProxy) BaseURL() string {
	if p.base == "" {
		return "http://127.0.0.1:8787"
	}
	return p.base
}

// fakeProber returns canned results in call order, so plan flows need no network.
type fakeProber struct {
	queue  []*probe.Result
	errs   []error
	calls  int
	models [][]string
}

func (p *fakeProber) Plan(_ context.Context, _ string, models []string) (*probe.Result, error) {
	i := p.calls
	p.calls++
	p.models = append(p.models, models)
	if i < len(p.errs) && p.errs[i] != nil {
		return nil, p.errs[i]
	}
	if i < len(p.queue) {
		return p.queue[i], nil
	}
	return &probe.Result{}, nil
}

// ------------------------------------------------------------------- harness

type testEnv struct {
	app     *App
	credDir string
	px      *fakeProxy
	pr      *fakeProber
	n       *fakeNotifier
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	home := t.TempDir()
	credDir := filepath.Join(home, ".commandcode")
	t.Setenv("COMMANDCODE_HOME", credDir)
	t.Setenv("APPDATA", filepath.Join(home, "appdata")) // isolates settings.Dir()
	if _, err := settings.Dir(); err != nil {
		t.Fatalf("settings dir: %v", err)
	}
	vl, err := vault.Open(filepath.Join(home, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{
		credDir: credDir,
		px:      &fakeProxy{running: true},
		pr:      &fakeProber{},
		n:       &fakeNotifier{},
	}
	env.app = &App{
		cfg:       settings.Config{ProbeModels: []string{"m1", "m2"}},
		vl:        vl,
		px:        env.px,
		probers:   map[string]planProber{},
		newProber: func(string) planProber { return env.pr },
	}
	return env
}

// tray builds a controller with the recording notifier and no real tray icon.
func (e *testEnv) tray() *trayController { return &trayController{svc: e.app, n: e.n} }

// loginAs simulates `cmdc login` writing a credential file.
func (e *testEnv) loginAs(t *testing.T, userID, userName, key string) {
	t.Helper()
	if err := os.MkdirAll(e.credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"apiKey":"` + key + `","userId":"` + userID + `","userName":"` + userName +
		`","keyName":"cli","authenticatedAt":"2026-01-01T00:00:00.000Z"}`
	if err := os.WriteFile(filepath.Join(e.credDir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// saveBoth collects two accounts through the tray save action.
func (e *testEnv) saveBoth(t *testing.T) (alice, bob vault.Account) {
	t.Helper()
	e.loginAs(t, "u-a", "alice", "user_alice_key_aaaaaaaaa")
	e.tray().actSaveCurrent()
	e.loginAs(t, "u-b", "bob", "user_bob_key_bbbbbbbbbbb")
	e.tray().actSaveCurrent()
	for _, ac := range e.app.Accounts() {
		switch ac.UserName {
		case "alice":
			alice = ac
		case "bob":
			bob = ac
		}
	}
	if alice.ID == "" || bob.ID == "" {
		t.Fatalf("expected both accounts saved, got %+v", e.app.Accounts())
	}
	return alice, bob
}

// ---------------------------------------------------------- save / switch

func TestSaveCurrentCredentialRequiresLogin(t *testing.T) {
	env := newEnv(t)
	_, _, err := env.app.SaveCurrentCredential()
	if err == nil || !strings.Contains(err.Error(), "cmdc login") {
		t.Fatalf("want guidance mentioning cmdc login, got %v", err)
	}
}

func TestTraySaveActionReportsNewThenUpdate(t *testing.T) {
	env := newEnv(t)
	tc := env.tray()

	// Nothing logged in: an error dialog, no state change.
	tc.actSaveCurrent()
	if len(env.n.fails) != 1 || len(env.n.infos) != 0 {
		t.Fatalf("want one failure dialog, got fails=%v infos=%v", env.n.fails, env.n.infos)
	}
	if len(env.app.Accounts()) != 0 {
		t.Fatal("vault must stay empty")
	}

	// First save is reported as new, re-save as an update.
	env.loginAs(t, "u-a", "alice", "user_alice_key_aaaaaaaaa")
	tc.actSaveCurrent()
	if len(env.n.infos) != 1 || !strings.Contains(env.n.infos[0], "已新增") {
		t.Fatalf("first save should say 已新增: %v", env.n.infos)
	}
	if !strings.Contains(env.n.infos[0], "alice") {
		t.Fatalf("dialog should name the account: %v", env.n.infos)
	}
	tc.actSaveCurrent()
	if len(env.n.infos) != 2 || !strings.Contains(env.n.infos[1], "已更新") {
		t.Fatalf("second save should say 已更新: %v", env.n.infos)
	}
	if got := len(env.app.Accounts()); got != 1 {
		t.Fatalf("re-save must not duplicate: %d accounts", got)
	}
}

func TestTraySwitchActionAppliesAndNotifies(t *testing.T) {
	env := newEnv(t)
	alice, bob := env.saveBoth(t)
	tc := env.tray()
	env.n.infos = nil

	tc.actSwitchTo(alice)

	if len(env.n.infos) != 1 || !strings.Contains(env.n.infos[0], "alice") {
		t.Fatalf("switch should confirm the new account: %v", env.n.infos)
	}
	auth, _, err := ccdir.ReadAuth()
	if err != nil || auth.UserID != "u-a" {
		t.Fatalf("CLI credential = %+v err=%v, want u-a", auth, err)
	}
	keyPath, _ := settings.ActiveKeyPath()
	if b, err := os.ReadFile(keyPath); err != nil || strings.TrimSpace(string(b)) != "user_alice_key_aaaaaaaaa" {
		t.Fatalf("exported key = %q err=%v", b, err)
	}
	if env.app.ActiveID() != alice.ID {
		t.Fatalf("active marker = %q, want %q", env.app.ActiveID(), alice.ID)
	}
	if env.app.ActiveID() == bob.ID {
		t.Fatal("bob must not stay active")
	}

	// Switching to a credential that is not in the vault fails loudly...
	env.n.infos = nil
	tc.actSwitchTo(vault.Account{ID: "missing", UserName: "ghost"})
	if len(env.n.fails) != 1 || !strings.Contains(env.n.fails[0], "切换失败") {
		t.Fatalf("want a 切换失败 dialog: %v", env.n.fails)
	}
	// ...and leaves the current credential untouched.
	if auth, _, err := ccdir.ReadAuth(); err != nil || auth.UserID != "u-a" {
		t.Fatalf("failed switch changed the credential: %+v %v", auth, err)
	}
}

// ------------------------------------------------------ confirm-gated actions

// TestTrayDeactivateAppliesAndNotifies: deactivation is reversible, so it
// acts immediately and reports via notification — no modal confirmation.
func TestTrayDeactivateAppliesAndNotifies(t *testing.T) {
	env := newEnv(t)
	alice, _ := env.saveBoth(t)
	tc := env.tray()
	if err := env.app.SwitchTo(alice.ID); err != nil {
		t.Fatal(err)
	}
	keyPath, _ := settings.ActiveKeyPath()
	env.n.infos, env.n.fails, env.n.asks = nil, nil, nil

	tc.actDeactivate()

	if len(env.n.asks) != 0 {
		t.Fatalf("deactivate must not ask for confirmation: %v", env.n.asks)
	}
	if len(env.n.fails) != 0 {
		t.Fatalf("unexpected failure: %v", env.n.fails)
	}
	if len(env.n.infos) != 1 || !strings.Contains(env.n.infos[0], "已停用") {
		t.Fatalf("want a 已停用 notification, got %v", env.n.infos)
	}
	if _, _, err := ccdir.ReadAuth(); err == nil {
		t.Fatal("credential still present after deactivate")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("exported key still present after deactivate")
	}
	if len(env.app.Accounts()) != 2 {
		t.Fatal("vault copies must survive deactivate")
	}
}

func TestTrayDeleteAsksAndRespectsCancel(t *testing.T) {
	env := newEnv(t)
	alice, bob := env.saveBoth(t)
	tc := env.tray()
	if err := env.app.SwitchTo(bob.ID); err != nil {
		t.Fatal(err)
	}

	// Cancelled: account stays.
	tc.actDeleteFromVault(alice)
	if len(env.app.Accounts()) != 2 {
		t.Fatal("cancelled delete removed an account")
	}
	// Confirmed: gone.
	env.n.confirm = true
	tc.actDeleteFromVault(alice)
	if len(env.app.Accounts()) != 1 {
		t.Fatalf("delete did not remove the account: %v", env.app.Accounts())
	}
	// The active account is refused with a clear reason.
	tc.actDeleteFromVault(bob)
	if len(env.n.fails) == 0 || !strings.Contains(env.n.fails[len(env.n.fails)-1], "停用") {
		t.Fatalf("deleting the active account should be refused: %v", env.n.fails)
	}
	if len(env.app.Accounts()) != 1 {
		t.Fatal("refused delete must not remove anything")
	}
}

// -------------------------------------------------------------- plan refresh

func TestTrayRefreshPlansSummarizesAndCaches(t *testing.T) {
	env := newEnv(t)
	env.saveBoth(t)
	env.pr.queue = []*probe.Result{
		{Allowed: []string{"m1"}, Denied: []string{"m2"}},
		{Blocked: true, Reason: "账户额度不足（未购买套餐或已用尽）"},
	}
	env.n.infos, env.n.fails = nil, nil // drop the save-action dialogs
	// Results are consumed in account order (Accounts() is name-sorted: alice, bob).
	tc := env.tray()
	tc.actRefreshPlans()

	if env.pr.calls != 2 {
		t.Fatalf("prober calls = %d, want 2", env.pr.calls)
	}
	if len(env.pr.models[0]) != 2 {
		t.Fatalf("probe models not passed through: %v", env.pr.models[0])
	}
	if len(env.n.fails) != 0 {
		t.Fatalf("unexpected failure dialog: %v", env.n.fails)
	}
	if len(env.n.infos) != 1 {
		t.Fatalf("want one summary dialog, got %v", env.n.infos)
	}
	summary := env.n.infos[0]
	if !strings.Contains(summary, "alice") || !strings.Contains(summary, "可用 1 个模型") {
		t.Fatalf("summary missing alice/available count: %q", summary)
	}
	if !strings.Contains(summary, "bob") || !strings.Contains(summary, "额度不足") {
		t.Fatalf("summary missing bob/blocked reason: %q", summary)
	}

	// Verdicts are cached per account and surface in the menu labels.
	for _, ac := range env.app.Accounts() {
		if ac.Plan == nil {
			t.Fatalf("plan not cached for %s", ac.UserName)
		}
		if ac.UserName == "bob" && !strings.Contains(AccountLabel(ac), "无额度") {
			t.Fatalf("blocked label = %q", AccountLabel(ac))
		}
		if ac.UserName == "alice" && !strings.Contains(AccountLabel(ac), "可用模型 1") {
			t.Fatalf("allowed label = %q", AccountLabel(ac))
		}
	}
}

func TestTrayRefreshPlansStartsProxyWhenNeeded(t *testing.T) {
	env := newEnv(t)
	env.saveBoth(t)
	env.px.running = false
	env.pr.queue = []*probe.Result{{Allowed: []string{"m1"}}, {Allowed: []string{"m1"}}}

	env.tray().actRefreshPlans()

	if env.px.starts != 1 {
		t.Fatalf("want the proxy started once, got %d", env.px.starts)
	}
	if len(env.n.fails) != 0 {
		t.Fatalf("unexpected failure: %v", env.n.fails)
	}
}

func TestTrayRefreshPlansWithoutAccounts(t *testing.T) {
	env := newEnv(t)
	env.px.running = false
	env.tray().actRefreshPlans()
	if len(env.n.fails) != 1 || !strings.Contains(env.n.fails[0], "空") {
		t.Fatalf("want an empty-vault message, got %v", env.n.fails)
	}
	if env.px.starts != 0 {
		t.Fatal("must not start the proxy with nothing to probe")
	}
}

// ----------------------------------------------------------------- proxy/menu

func TestTrayToggleProxy(t *testing.T) {
	env := newEnv(t)
	tc := env.tray()

	env.px.running = true
	tc.actToggleProxy()
	if env.px.stops != 1 || env.px.running {
		t.Fatalf("toggle with running proxy: stops=%d running=%v", env.px.stops, env.px.running)
	}

	tc.actToggleProxy()
	if env.px.starts != 1 || !env.px.running {
		t.Fatalf("toggle with stopped proxy: starts=%d running=%v", env.px.starts, env.px.running)
	}

	env.px.startErr = context.DeadlineExceeded
	env.px.running = false
	tc.actToggleProxy()
	if len(env.n.fails) != 1 || !strings.Contains(env.n.fails[0], "启动代理失败") {
		t.Fatalf("want a start failure dialog, got %v", env.n.fails)
	}
}

// menuItems reads a Menu's items. wails keeps the item slice unexported, so
// the test reaches it with reflection + unsafe (test-only; if wails changes
// its internals this is the only place that needs updating).
func menuItems(t *testing.T, menu *application.Menu) []*application.MenuItem {
	t.Helper()
	v := reflect.ValueOf(menu).Elem().FieldByName("items")
	if !v.IsValid() || v.Kind() != reflect.Slice {
		t.Fatalf("wails Menu internals changed")
	}
	return *(*[]*application.MenuItem)(unsafe.Pointer(v.UnsafeAddr()))
}

func menuLabels(t *testing.T, menu *application.Menu) []string {
	t.Helper()
	items := menuItems(t, menu)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it != nil {
			out = append(out, it.Label())
		}
	}
	return out
}

func menuItemByLabel(t *testing.T, menu *application.Menu, label string) *application.MenuItem {
	t.Helper()
	for _, it := range menuItems(t, menu) {
		if it.Label() == label {
			return it
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMenuIsFlatAndGrouped(t *testing.T) {
	env := newEnv(t)
	env.saveBoth(t)
	menu := env.tray().buildMenu()

	// No submenus anywhere: everything is reachable in one level.
	for _, it := range menuItems(t, menu) {
		if it.GetSubmenu() != nil {
			t.Fatalf("menu item %q is a submenu; the menu must stay flat", it.Label())
		}
	}
	labels := menuLabels(t, menu)
	for _, want := range []string{
		"保存当前登录凭证", "停用当前登录", "刷新套餐额度",
		"打开仪表盘 /admin", "打开保管库目录", "退出",
	} {
		if !contains(labels, want) {
			t.Fatalf("menu is missing %q; got %v", want, labels)
		}
	}
}

func TestMenuDisablesImpossibleActions(t *testing.T) {
	env := newEnv(t)
	// Nothing is logged in, nothing saved, and no proxy can be brought up.
	env.px.running, env.px.binaryExists = false, false
	menu := env.tray().buildMenu()
	labels := menuLabels(t, menu)

	// Nothing logged in, nothing saved: these can only fail, so no clicking.
	for _, label := range []string{"保存当前登录凭证", "停用当前登录", "刷新套餐额度", "打开仪表盘 /admin"} {
		item := menuItemByLabel(t, menu, label)
		if item == nil {
			t.Fatalf("menu is missing %q; got %v", label, labels)
		}
		if item.Enabled() {
			t.Fatalf("%q must be disabled when it cannot succeed", label)
		}
	}
	// The header explains how to get out of that state.
	if !strings.Contains(labels[0], "cmdc login") {
		t.Fatalf("header should guide the user: %q", labels[0])
	}
	if !contains(labels, "（保管库为空，先「保存当前登录凭证」）") {
		t.Fatalf("empty vault should be stated: %v", labels)
	}
	// Starting a proxy that does not exist cannot work either.
	if item := menuItemByLabel(t, menu, "本地代理：已停止（点击启动）"); item == nil || item.Enabled() {
		t.Fatal("the proxy toggle must be disabled when there is no binary to start")
	}
}

func TestMenuEnablesWhatIsUsable(t *testing.T) {
	env := newEnv(t)
	alice, bob := env.saveBoth(t) // last login is bob; vault holds both
	formenu := env.tray().buildMenu()

	if item := menuItemByLabel(t, formenu, "保存当前登录凭证"); !item.Enabled() {
		t.Fatal("保存当前登录凭证 should be enabled while logged in")
	}
	if item := menuItemByLabel(t, formenu, "停用当前登录"); !item.Enabled() {
		t.Fatal("停用当前登录 should be enabled while logged in")
	}
	if item := menuItemByLabel(t, formenu, "刷新套餐额度"); !item.Enabled() {
		t.Fatal("刷新套餐额度 should be enabled with saved accounts and a startable proxy")
	}
	if item := menuItemByLabel(t, formenu, "打开仪表盘 /admin"); !item.Enabled() {
		t.Fatal("打开仪表盘 should be enabled while the proxy runs")
	}

	// The installed credential is a state marker, not a clickable action;
	// every other account is switchable.
	bobItem := menuItemByLabel(t, formenu, AccountLabel(bob))
	if bobItem == nil {
		t.Fatalf("bob missing from the menu: %v", menuLabels(t, formenu))
	}
	if bobItem.Enabled() {
		t.Fatal("the currently installed account must not be clickable")
	}
	if !bobItem.Checked() {
		t.Fatal("the currently installed account should be marked")
	}
	aliceItem := menuItemByLabel(t, formenu, AccountLabel(alice))
	if aliceItem == nil || !aliceItem.Enabled() {
		t.Fatalf("other accounts must stay switchable: %v", menuLabels(t, formenu))
	}

	// Deletion is offered only for copies that are not in use.
	if item := menuItemByLabel(t, formenu, "删除保管库副本："+bob.UserName); item != nil {
		t.Fatal("the in-use account must not offer deletion")
	}
	if item := menuItemByLabel(t, formenu, "删除保管库副本："+alice.UserName); item == nil {
		t.Fatalf("delete entry for the idle account is missing: %v", menuLabels(t, formenu))
	}

	// After switching, the marks and the delete entries follow the new state.
	if err := env.app.SwitchTo(alice.ID); err != nil {
		t.Fatal(err)
	}
	after := env.tray().buildMenu()
	if item := menuItemByLabel(t, after, AccountLabel(alice)); item == nil || item.Enabled() {
		t.Fatal("after switching, the new current account must become non-clickable")
	}
	if item := menuItemByLabel(t, after, AccountLabel(bob)); item == nil || !item.Enabled() {
		t.Fatal("after switching, the previous account must become switchable again")
	}
	if item := menuItemByLabel(t, after, "删除保管库副本："+bob.UserName); item == nil {
		t.Fatal("after switching, the previous account should offer deletion")
	}
	if item := menuItemByLabel(t, after, "删除保管库副本："+alice.UserName); item != nil {
		t.Fatal("after switching, the new current account must not offer deletion")
	}
}

func TestMenuProxyStates(t *testing.T) {
	env := newEnv(t)

	// External instance owns the port: not ours to start or stop.
	env.px.running, env.px.external = true, true
	menu := env.tray().buildMenu()
	if item := menuItemByLabel(t, menu, "本地代理：外部实例运行中（"+env.px.BaseURL()+"）"); item == nil || item.Enabled() {
		t.Fatalf("external proxy entry must be a disabled status marker: %v", menuLabels(t, menu))
	}
	if item := menuItemByLabel(t, menu, "打开仪表盘 /admin"); item == nil || !item.Enabled() {
		t.Fatal("dashboard should be reachable while any proxy runs")
	}

	// Stopped with no binary: nothing proxy-dependent is usable.
	env.px.running, env.px.external = false, false
	env.px.binaryExists = false
	env.loginAs(t, "u-a", "alice", "user_alice_key_aaaaaaaaa")
	if _, _, err := env.app.SaveCurrentCredential(); err != nil {
		t.Fatal(err)
	}
	menu = env.tray().buildMenu()
	for _, label := range []string{"刷新套餐额度", "打开仪表盘 /admin"} {
		if item := menuItemByLabel(t, menu, label); item == nil || item.Enabled() {
			t.Fatalf("%q must be disabled when no proxy can be brought up: %v", label, menuLabels(t, menu))
		}
	}
	if item := menuItemByLabel(t, menu, "本地代理：已停止（点击启动）"); item == nil || item.Enabled() {
		t.Fatal("the proxy toggle must be disabled when no binary exists to start")
	}

	// Once a binary is on disk the toggle and probing become usable again.
	env.px.binaryExists = true
	menu = env.tray().buildMenu()
	if item := menuItemByLabel(t, menu, "本地代理：已停止（点击启动）"); item == nil || !item.Enabled() {
		t.Fatal("the proxy toggle should be enabled when a binary is available")
	}
	if item := menuItemByLabel(t, menu, "刷新套餐额度"); item == nil || !item.Enabled() {
		t.Fatal("刷新套餐额度 should be enabled when the proxy can be started")
	}
}

// ---------------------------------------------------------------- vault state

func TestSaveThenSwitchRoundTrip(t *testing.T) {
	env := newEnv(t)
	alice, bob := env.saveBoth(t)

	// Re-saving the same account is an update, not a new entry.
	if _, isNew, err := env.app.SaveCurrentCredential(); err != nil || isNew {
		t.Fatalf("re-save: err=%v isNew=%v", err, isNew)
	}
	if got := len(env.app.Accounts()); got != 2 {
		t.Fatalf("vault has %d accounts, want 2", got)
	}

	if err := env.app.SwitchTo(alice.ID); err != nil {
		t.Fatal(err)
	}
	auth, _, err := ccdir.ReadAuth()
	if err != nil || auth.UserID != "u-a" {
		t.Fatalf("after switch: %+v err=%v", auth, err)
	}
	keyPath, _ := settings.ActiveKeyPath()
	if b, _ := os.ReadFile(keyPath); strings.TrimSpace(string(b)) != "user_alice_key_aaaaaaaaa" {
		t.Fatalf("active.key = %q", b)
	}
	if env.app.ActiveID() != creds.SafeID("u-a") {
		t.Fatalf("active marker = %q", env.app.ActiveID())
	}

	cur := env.app.CurrentCLI()
	if !cur.Present || cur.Name != "alice" {
		t.Fatalf("CurrentCLI = %+v", cur)
	}
	if strings.Contains(cur.Masked, "alice_key") {
		t.Fatalf("masked key leaks the secret: %q", cur.Masked)
	}
	if bob.ID == alice.ID {
		t.Fatal("distinct accounts collapsed into one id")
	}
}

func TestDeleteFromVault(t *testing.T) {
	env := newEnv(t)
	alice, bob := env.saveBoth(t)
	if err := env.app.SwitchTo(bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.app.DeleteFromVault(bob.ID); err == nil {
		t.Fatal("deleting the active account should be refused")
	}
	if err := env.app.DeleteFromVault(alice.ID); err != nil {
		t.Fatalf("deleting a non-active account failed: %v", err)
	}
	if got := len(env.app.Accounts()); got != 1 {
		t.Fatalf("after delete: %d accounts, want 1", got)
	}
}

func TestMenuLabelsAndTooltip(t *testing.T) {
	env := newEnv(t)
	if got := env.app.Tooltip(); !strings.Contains(got, "未登录") {
		t.Fatalf("tooltip before login = %q", got)
	}
	env.loginAs(t, "u-a", "alice", "user_alice_key_aaaaaaaaa")
	if _, _, err := env.app.SaveCurrentCredential(); err != nil {
		t.Fatal(err)
	}
	if got := env.app.Tooltip(); !strings.Contains(got, "alice") {
		t.Fatalf("tooltip after login = %q", got)
	}
	ac := env.app.Accounts()[0]
	if label := AccountLabel(ac); !strings.Contains(label, "alice") || !strings.Contains(label, "未探测") {
		t.Fatalf("account label = %q", label)
	}
	if err := env.app.vl.PutPlan(ac.ID, &vault.PlanInfo{Blocked: true, Reason: "额度不足"}); err != nil {
		t.Fatal(err)
	}
	if label := AccountLabel(env.app.Accounts()[0]); !strings.Contains(label, "无额度") {
		t.Fatalf("blocked label = %q", label)
	}
}

// renderMenu turns a menu into readable lines: "✓" clickable, "·" disabled
// status/state entries, "[•]" a checked radio.
func renderMenu(t *testing.T, menu *application.Menu) []string {
	t.Helper()
	out := []string{}
	for _, it := range menuItems(t, menu) {
		label := it.Label()
		if label == "" { // separator
			out = append(out, "---")
			continue
		}
		mark := "✓"
		if !it.Enabled() {
			mark = "·"
		}
		if it.Checked() {
			mark += " [•]"
		}
		out = append(out, mark+" "+label)
	}
	return out
}

// TestMenuGolden pins the flattened layout and its clickability so a change to
// the menu is always deliberate.
func TestMenuGolden(t *testing.T) {
	env := newEnv(t)
	alice, bob := env.saveBoth(t) // logged in as bob, both saved
	// A binary is on disk in real builds (extracted from the embedded copy);
	// the fake mirrors that so the proxy toggle stays clickable.
	env.px.binaryExists = true
	if err := env.app.SwitchTo(alice.ID); err != nil { // alice becomes current
		t.Fatal(err)
	}
	_ = bob
	got := renderMenu(t, env.tray().buildMenu())

	want := []string{
		"· 当前：alice（" + env.app.Accounts()[0].MaskedKey + "）",
		"---",
		"· [•] alice — 未探测",
		"✓ bob — 未探测",
		"---",
		"✓ 保存当前登录凭证",
		"✓ 停用当前登录",
		"✓ 刷新套餐额度",
		"---",
		"✓ 删除保管库副本：bob",
		"---",
		"✓ [•] 本地代理：运行中（http://127.0.0.1:8787）",
		"✓ 打开仪表盘 /admin",
		"---",
		"✓ 打开保管库目录",
		"✓ 退出",
	}
	if len(got) != len(want) {
		t.Fatalf("menu shape changed:\n got:\n%s\nwant:\n%s", join(got), join(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("menu line %d:\n got: %q\nwant: %q\nfull:\n%s", i, got[i], want[i], join(got))
		}
	}
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "  " + l + "\n"
	}
	return out
}

// fakeTray records what the tray is told and in which order.
type fakeTray struct {
	events  []string
	menu    *application.Menu
	tooltip string
}

func (f *fakeTray) SetMenu(m *application.Menu) *application.SystemTray {
	f.events = append(f.events, "setMenu")
	f.menu = m
	return nil
}
func (f *fakeTray) SetTooltip(s string) {
	f.events = append(f.events, "setTooltip")
	f.tooltip = s
}
func (f *fakeTray) OpenMenu() { f.events = append(f.events, "openMenu") }
func (f *fakeTray) ShowMenu() { f.events = append(f.events, "showMenu") }

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

func headerOf(t *testing.T, menu *application.Menu) string {
	t.Helper()
	if menu == nil {
		t.Fatal("no menu was set on the tray")
	}
	labels := menuLabels(t, menu)
	if len(labels) == 0 {
		t.Fatal("menu has no items")
	}
	return labels[0]
}

// TestMenuRefreshesWhenOpenedAfterManualLogin covers the reported gap: a login
// performed in a terminal (no action of ours) must be visible the next time
// the menu is opened.
func TestMenuRefreshesWhenOpenedAfterManualLogin(t *testing.T) {
	env := newEnv(t)
	tr := &fakeTray{}
	tc := env.tray()
	tc.tray = tr

	// Nothing logged in: the menu says so.
	tc.handleClick()
	if got := headerOf(t, tr.menu); !strings.Contains(got, "未登录") {
		t.Fatalf("header before login = %q", got)
	}
	if indexOf(tr.events, "setMenu") > indexOf(tr.events, "openMenu") {
		t.Fatalf("menu must be rebuilt before it opens: %v", tr.events)
	}
	if item := menuItemByLabel(t, tr.menu, "保存当前登录凭证"); item.Enabled() {
		t.Fatal("保存 must be disabled while nothing is logged in")
	}

	// The user logs in OUTSIDE the app (terminal `cmdc login`).
	env.loginAs(t, "u-a", "alice", "user_alice_key_aaaaaaaaa")

	// Opening the menu again must reflect it — no app action was involved.
	tr.events = nil
	tc.handleRightClick()
	if got := headerOf(t, tr.menu); !strings.Contains(got, "alice") {
		t.Fatalf("header after a manual login = %q", got)
	}
	if got := indexOf(tr.events, "setMenu"); got < 0 || got > indexOf(tr.events, "showMenu") {
		t.Fatalf("right-click must rebuild before showing: %v", tr.events)
	}
	if item := menuItemByLabel(t, tr.menu, "保存当前登录凭证"); item == nil || !item.Enabled() {
		t.Fatal("保存 must become clickable once a credential exists")
	}
}

// TestTooltipFollowsExternalLogin covers the hover text, which is visible
// without opening the menu at all.
func TestTooltipFollowsExternalLogin(t *testing.T) {
	env := newEnv(t)
	tr := &fakeTray{}
	tc := env.tray()
	tc.tray = tr

	tc.refreshTooltip()
	if !strings.Contains(tr.tooltip, "未登录") {
		t.Fatalf("tooltip before login = %q", tr.tooltip)
	}
	env.loginAs(t, "u-a", "alice", "user_alice_key_aaaaaaaaa")
	tc.refreshTooltip()
	if !strings.Contains(tr.tooltip, "alice") {
		t.Fatalf("tooltip after a manual login = %q", tr.tooltip)
	}
	if indexOf(tr.events, "setMenu") >= 0 {
		t.Fatalf("the tooltip tick must not touch the menu: %v", tr.events)
	}
}
