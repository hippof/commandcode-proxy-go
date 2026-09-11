//go:build !windows

package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
)

// wailsNotifier uses wails' native dialogs (macOS/Linux). Those platforms wire
// callbacks by button index rather than by label, so localized button captions
// are safe there; Windows uses its own MessageBox-based notifier instead.
type wailsNotifier struct{ app *application.App }

func newNotifier(app *application.App) notifier { return wailsNotifier{app: app} }

func (n wailsNotifier) Info(title, msg string) {
	if n.app == nil || n.app.Dialog == nil {
		return
	}
	dlg := n.app.Dialog.Info().SetTitle(title).SetMessage(msg)
	dlg.AddButton("确定").SetAsDefault()
	dlg.Show()
}

func (n wailsNotifier) Fail(title string, err error) {
	if n.app == nil || n.app.Dialog == nil {
		return
	}
	dlg := n.app.Dialog.Error().SetTitle(title).SetMessage(err.Error())
	dlg.AddButton("确定").SetAsDefault()
	dlg.Show()
}

func (n wailsNotifier) Ask(title, msg, yesLabel string, onYes func()) {
	if n.app == nil || n.app.Dialog == nil {
		return
	}
	dlg := n.app.Dialog.Question().SetTitle(title).SetMessage(msg)
	dlg.AddButton(yesLabel).SetAsDefault().OnClick(func() { go onYes() })
	dlg.AddButton("取消").SetAsCancel()
	dlg.Show()
}
