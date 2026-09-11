// System tray: the entire user interface of this app. The menu offers the two
// core actions (save the credential the CLI is currently logged in with, and
// switch between saved credentials) plus proxy control and maintenance. All
// feedback goes through native dialogs, so there is no window at all.
//
// Each menu action is a method on trayController (act*) rather than an inline
// closure, so the behaviours — including what they tell the user — are
// testable with a recording notifier and a fake proxy.
package main

import (
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"commandcode-desktop/internal/vault"
)

//go:embed tray.png
var trayIcon []byte

// notifier surfaces action results to the user. Production shows native
// dialogs; tests record calls instead.
type notifier interface {
	Info(title, msg string)
	Fail(title string, err error)
	Ask(title, msg, yesLabel string, onYes func())
}

// traySurface is the slice of the system tray this app drives. It exists so
// tests can observe what the tray is told (and when) without a real icon.
type traySurface interface {
	SetMenu(*application.Menu) *application.SystemTray
	SetTooltip(string)
	OpenMenu()
	ShowMenu()
}

type trayController struct {
	app  *application.App
	svc  *App
	tray traySurface
	n    notifier

	mu sync.Mutex

	// tooltipEvery is how often the hover text is refreshed so that logins
	// performed outside this app show up without touching the menu.
	tooltipEvery time.Duration
}

func setupTray(app *application.App, svc *App) *trayController {
	t := &trayController{app: app, svc: svc, n: newNotifier(app), tooltipEvery: 5 * time.Second}
	tr := app.SystemTray.New()
	tr.SetIcon(trayIcon)
	t.tray = tr
	t.refresh()

	// Rebuild right before the menu is shown: the credential directory can
	// change behind our back (a manual `cmdc login`, a file edit), and a stale
	// menu is worse than no menu. Right-click must be intercepted too,
	// because wails otherwise shows the previously built menu as-is.
	tr.OnClick(func() { t.handleClick() })
	tr.OnRightClick(func() { t.handleRightClick() })
	go t.tooltipLoop()
	return t
}

// handleClick refreshes the menu and opens it (left click).
func (t *trayController) handleClick() {
	t.refresh()
	if t.tray != nil {
		t.tray.OpenMenu()
	}
}

// handleRightClick behaves like handleClick but shows the context menu; the
// refresh is what keeps manual logins visible.
func (t *trayController) handleRightClick() {
	t.refresh()
	if t.tray != nil {
		t.tray.ShowMenu()
	}
}

// tooltipLoop keeps the hover text current between menu opens. It only writes
// the tooltip — never the menu — so it cannot disturb a menu being displayed.
func (t *trayController) tooltipLoop() {
	every := t.tooltipEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for range tick.C {
		t.refreshTooltip()
	}
}

// refreshTooltip updates only the hover text.
func (t *trayController) refreshTooltip() {
	if t.tray == nil {
		return
	}
	t.tray.SetTooltip(t.svc.Tooltip())
}

// refresh rebuilds the menu and tooltip from current state. Items carry live
// state (header, radio marks, proxy check), so it runs after every action and
// immediately before the menu is shown.
func (t *trayController) refresh() {
	if t.tray == nil {
		return // headless (tests): nothing to update
	}
	menu := t.buildMenu()
	tooltip := t.svc.Tooltip()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tray.SetMenu(menu)
	t.tray.SetTooltip(tooltip)
}

// buildMenu lays the actions out flat (no submenus) in separator-separated
// groups, and disables every entry whose action could only fail: an item you
// cannot use right now should not be clickable.
func (t *trayController) buildMenu() *application.Menu {
	menu := application.NewMenu()
	st := t.svc.TrayState()

	// --- state (informational, never clickable) --------------------------
	if st.LoggedIn {
		menu.Add("当前：" + displayNameOf(st.CurrentName, st.CurrentID) + "（" + st.CurrentMasked + "）").
			SetEnabled(false)
	} else {
		menu.Add("当前未登录 — 先在终端运行 cmdc login 后回到这里").SetEnabled(false)
	}
	menu.AddSeparator()

	// --- switch: one radio per saved credential -------------------------
	if len(st.Accounts) == 0 {
		menu.Add("（保管库为空，先「保存当前登录凭证」）").SetEnabled(false)
	} else {
		for _, ac := range st.Accounts {
			acct := ac
			current := acct.ID == st.CurrentID
			item := menu.AddRadio(AccountLabel(acct), current)
			if current {
				// It is already installed: switching would rewrite the same
				// bytes, so offer it as a state marker only.
				item.SetEnabled(false)
				continue
			}
			item.OnClick(func(*application.Context) { t.actSwitchTo(acct) })
		}
	}
	menu.AddSeparator()

	// --- credential lifecycle -------------------------------------------
	save := menu.Add("保存当前登录凭证")
	if st.LoggedIn {
		save.OnClick(func(*application.Context) { t.actSaveCurrent() })
	} else {
		save.SetEnabled(false)
	}

	deactivate := menu.Add("停用当前登录")
	if st.LoggedIn {
		deactivate.OnClick(func(*application.Context) { t.actDeactivate() })
	} else {
		deactivate.SetEnabled(false)
	}

	refresh := menu.Add("刷新套餐额度")
	switch {
	case len(st.Accounts) == 0:
		refresh.SetEnabled(false) // nothing saved to probe
	case !st.ProxyRunning && !st.ProxyCanStart:
		refresh.SetEnabled(false) // probing needs the proxy and there is none to start
	default:
		refresh.OnClick(func(*application.Context) { go t.actRefreshPlans() })
	}
	menu.AddSeparator()

	// --- deletions: only copies that are safe to drop -------------------
	deletable := 0
	for _, ac := range st.Accounts {
		if st.Protected(ac.ID) {
			continue
		}
		deletable++
		acct := ac
		menu.Add("删除保管库副本：" + displayNameOf(acct.UserName, acct.ID)).
			OnClick(func(*application.Context) { t.actDeleteFromVault(acct) })
	}
	if deletable > 0 {
		menu.AddSeparator()
	}

	// --- proxy ----------------------------------------------------------
	proxyLabel := "本地代理：已停止（点击启动）"
	switch {
	case st.ProxyExternal:
		proxyLabel = "本地代理：外部实例运行中（" + st.ProxyBaseURL + "）"
	case st.ProxyRunning:
		proxyLabel = "本地代理：运行中（" + st.ProxyBaseURL + "）"
	}
	toggle := menu.AddCheckbox(proxyLabel, st.ProxyRunning)
	switch {
	case st.ProxyExternal:
		// Another process owns the port; neither start nor stop is ours to do.
		toggle.SetEnabled(false)
	case !st.ProxyRunning && !st.ProxyCanStart:
		toggle.SetEnabled(false) // nothing to start with
	default:
		toggle.OnClick(func(*application.Context) { t.actToggleProxy() })
	}

	dashboard := menu.Add("打开仪表盘 /admin")
	if st.ProxyRunning {
		dashboard.OnClick(func(*application.Context) { t.svc.OpenDashboard() })
	} else {
		dashboard.SetEnabled(false)
	}
	menu.AddSeparator()

	menu.Add("打开保管库目录").OnClick(func(*application.Context) {
		if err := t.svc.OpenVaultDir(); err != nil {
			t.n.Fail("打开目录失败", err)
		}
	})
	menu.Add("退出").OnClick(func(*application.Context) { t.svc.Quit() })
	return menu
}

// ------------------------------------------------------------------ actions

// actSaveCurrent stores the credential the CLI is logged in with.
func (t *trayController) actSaveCurrent() {
	ac, isNew, err := t.svc.SaveCurrentCredential()
	if err != nil {
		t.n.Fail("保存失败", err)
		return
	}
	verb := "已更新"
	if isNew {
		verb = "已新增"
	}
	t.n.Info("保存成功", fmt.Sprintf("%s账号 %s\n\n已存入保管库：\n%s",
		verb, displayNameOf(ac.UserName, ac.ID), t.svc.VaultDir()))
	t.refresh()
}

// actSwitchTo makes a saved credential the current one.
func (t *trayController) actSwitchTo(ac vault.Account) {
	name := displayNameOf(ac.UserName, ac.ID)
	if err := t.svc.SwitchTo(ac.ID); err != nil {
		t.n.Fail("切换失败", err)
		return
	}
	t.n.Info("已切换账号", fmt.Sprintf("当前登录：%s\n\nCLI 下次启动即用该账号；\n消费端已同步新密钥（%s 的 apiKeyHelper）。",
		name, t.svc.ProxyBaseURL()))
	t.refresh()
}

// actDeactivate removes the CLI credential after confirmation.
// actDeactivate removes the CLI credential. No confirmation dialog: the action
// is reversible in one click (the vault copy stays and switching back restores
// it), so it reports through a notification instead of interrupting the user.
func (t *trayController) actDeactivate() {
	if err := t.svc.Deactivate(); err != nil {
		t.n.Fail("停用失败", err)
		return
	}
	t.n.Info("已停用当前登录", "CLI 凭据与导出的密钥已清除；保管库副本仍在，选「切换」即可恢复。")
	t.refresh()
}

// actDeleteFromVault drops one saved copy after confirmation.
func (t *trayController) actDeleteFromVault(ac vault.Account) {
	name := displayNameOf(ac.UserName, ac.ID)
	t.n.Ask("删除保管库账号",
		fmt.Sprintf("确定从保管库删除 %s？\n\n只删除保存的副本；CLI 目录里正在使用的凭证不受影响。", name),
		"删除", func() {
			if err := t.svc.DeleteFromVault(ac.ID); err != nil {
				t.n.Fail("删除失败", err)
				return
			}
			t.n.Info("已删除", fmt.Sprintf("保管库中已移除 %s。", name))
			t.refresh()
		})
}

// actRefreshPlans probes every saved credential (starts the proxy if needed)
// and reports a per-account summary. Synchronous: menu handlers run it in a
// goroutine.
func (t *trayController) actRefreshPlans() {
	summary, err := t.svc.RefreshPlans()
	if err != nil {
		t.n.Fail("套餐探测失败", err)
		return
	}
	t.n.Info("套餐额度", summary)
	t.refresh()
}

// actToggleProxy starts or stops the proxy this app manages.
func (t *trayController) actToggleProxy() {
	if t.svc.ProxyRunning() {
		if err := t.svc.StopProxy(); err != nil {
			t.n.Fail("停止代理失败", err)
			return
		}
	} else {
		if err := t.svc.StartProxy(); err != nil {
			t.n.Fail("启动代理失败", err)
			return
		}
	}
	t.refresh()
}
