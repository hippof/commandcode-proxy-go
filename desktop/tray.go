// Tray integration: a system-tray icon whose menu lets you switch accounts,
// toggle the local proxy, and quit. The menu is rebuilt from the vault on
// every change (svc.SetAccountsChangedHook).
package main

import (
	_ "embed"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"

	"commandcode-desktop/internal/vault"
)

//go:embed tray.png
var trayIcon []byte

type trayController struct {
	app      *application.App
	svc      *App
	tray     *application.SystemTray
	quitting *atomic.Bool
}

func setupTray(app *application.App, svc *App, quitting *atomic.Bool) *trayController {
	t := &trayController{app: app, svc: svc, quitting: quitting}
	tr := app.SystemTray.New()
	tr.SetIcon(trayIcon)
	tr.SetTooltip("Command Code 账号管理器")
	tr.OnClick(func() { svc.ShowWindow() })
	t.tray = tr
	t.refresh()
	svc.SetAccountsChangedHook(t.refresh)
	return t
}

// rebuilds the tray menu and tooltip from current state.
func (t *trayController) refresh() {
	t.tray.SetMenu(t.buildMenu())
	t.tray.SetTooltip("Command Code 账号管理器 — " + t.activeLabel())
}

func (t *trayController) activeLabel() string {
	id := t.svc.ActiveAccountID()
	for _, a := range t.accounts() {
		if a.ID == id {
			return "当前：" + displayNameOf(a)
		}
	}
	return "未激活账号"
}

func (t *trayController) accounts() []vault.Account {
	list, err := t.svc.vl.List()
	if err != nil {
		return nil
	}
	return list
}

func displayNameOf(a vault.Account) string {
	if a.UserName != "" {
		return a.UserName
	}
	return a.ID
}

func (t *trayController) buildMenu() *application.Menu {
	menu := application.NewMenu()

	menu.Add("打开主界面").OnClick(func(*application.Context) { t.svc.ShowWindow() })
	menu.AddSeparator()

	accs := t.accounts()
	activeID := t.svc.ActiveAccountID()
	if len(accs) == 0 {
		menu.Add("（保管库为空）")
	} else {
		sub := menu.AddSubmenu("切换账号")
		for _, a := range accs {
			label := displayNameOf(a)
			if a.Note != "" {
				label += "（" + strings.TrimSpace(a.Note) + "）"
			}
			acc := a
			item := sub.AddRadio(label, acc.ID == activeID)
			item.OnClick(func(*application.Context) {
				if err := t.svc.Activate(acc.ID); err != nil {
					t.notify("切换账号失败", err.Error())
				}
			})
		}
	}
	menu.AddSeparator()

	running := t.svc.ProxyRunning()
	proxyItem := menu.AddCheckbox("本地代理运行中", running)
	proxyItem.OnClick(func(*application.Context) {
		if running {
			_ = t.svc.StopProxy()
		} else {
			if err := t.svc.StartProxy(); err != nil {
				t.notify("代理启动失败", err.Error())
			}
		}
		t.refresh()
	})
	menu.Add("打开仪表盘 /admin").OnClick(func(*application.Context) { t.svc.OpenDashboard() })
	menu.AddSeparator()

	toTray := t.svc.CloseActionIsTray()
	closeItem := menu.AddCheckbox("关闭窗口时最小化到托盘", toTray)
	closeItem.OnClick(func(*application.Context) {
		next := "tray"
		if toTray {
			next = "quit"
		}
		if err := t.svc.SetCloseAction(next); err != nil {
			t.notify("设置失败", err.Error())
		}
		t.refresh()
	})

	menu.AddSeparator()
	menu.Add("退出程序").OnClick(func(*application.Context) {
		t.quitting.Store(true)
		t.app.Quit()
	})
	return menu
}

// notify surfaces tray-side errors in the main window's toast area.
func (t *trayController) notify(title, msg string) {
	t.svc.EmitError(fmt.Sprintf("%s：%s", title, msg))
}
