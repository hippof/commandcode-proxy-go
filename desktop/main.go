// Command commandcode-desktop is a tray-only manager for multiple Command Code
// credentials. Logging in stays a manual terminal step (`cmdc login`); this
// app only reads the credential the CLI is logged in with, saves copies for
// safe keeping, and switches between them. There is no window and no
// frontend — the system tray menu and native dialogs are the whole UI.
package main

import (
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func main() {
	appSvc, err := NewApp()
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}
	if bin, err := appSvc.PrepareProxy(); err != nil {
		log.Printf("内置代理不可用：%v", err)
	} else {
		log.Printf("代理已就绪：%s", bin)
	}
	if appSvc.RecoverInterruptedSwap() {
		log.Println("restored credential directory left by an interrupted login swap")
	}

	app := application.New(application.Options{
		Name:        "Command Code 账号",
		Description: "多账号凭证管理与切换（托盘常驻，无窗口）",
		Icon:        trayIcon,
		Windows: application.WindowsOptions{
			// No window is ever created; never quit on "last window closed".
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})
	appSvc.SetRuntime(app)

	setupTray(app, appSvc)

	// Stop the proxy child process we started (external ones are left alone).
	app.OnShutdown(func() {
		_ = appSvc.StopProxy()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
