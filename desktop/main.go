// Command cmdc-desktop is a tray-only manager for multiple Command Code
// credentials. Logging in stays a manual terminal step (`cmdc login`); this
// app only reads the credential the CLI is logged in with, saves copies for
// safe keeping, and switches between them. There is no window and no
// frontend — the system tray menu and native dialogs are the whole UI.
package main

import (
	"log"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"commandcode-desktop/internal/applog"
	"commandcode-desktop/internal/settings"
)

// version is set via -ldflags "-X main.version=..." at build time.
var version = "dev"

func main() {
	appSvc, err := NewApp()
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}
	// Daily-file logging first, so everything below (including failures) lands
	// on disk. Never fatal: a read-only disk must not stop the app.
	if dir, err := settings.Dir(); err == nil {
		if err := applog.Init(filepath.Join(dir, "logs"), appSvc.cfg.LogLevel, appSvc.cfg.LogKeepDays); err != nil {
			log.Printf("日志文件不可用（仅输出到 stderr）：%v", err)
		} else {
			applog.Install()
			defer applog.Close()
			applog.Info("app", "启动", "version", version, "logDir", applog.Path())
		}
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

	tray := setupTray(app, appSvc)

	// Start the services once the event loop is up, in order: proxy first, then
	// the gateway (which needs a live proxy address). Failures are logged and
	// surfaced as a notification by the tray.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go tray.startServices()
	})

	// Stop the proxy child process we started (external ones are left alone).
	app.OnShutdown(func() {
		_ = appSvc.StopGateway()
		_ = appSvc.StopProxy()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
