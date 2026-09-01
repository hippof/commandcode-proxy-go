// Command commandcode-desktop is a Wails v3 desktop app that manages multiple
// Command Code accounts (auth.json vault, browser login flow) and runs the
// commandcode-proxy binary locally — without modifying the proxy repo, which
// lives one directory up and keeps its own module.
package main

import (
	"embed"
	"log"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"commandcode-desktop/internal/login"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	application.RegisterEvent[login.Status]("login:status")
	application.RegisterEvent[bool]("app:changed")
	application.RegisterEvent[string]("app:error")
}

func main() {
	appSvc, err := NewApp()
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}

	app := application.New(application.Options{
		Name:        "Command Code Accounts",
		Description: "Multi-account manager for commandcode-proxy",
		Services: []application.Service{
			application.NewService(appSvc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Icon: trayIcon,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})
	appSvc.SetRuntime(app)

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Command Code 账号管理器",
		Width:            1080,
		Height:           760,
		BackgroundColour: application.NewRGB(16, 18, 23),
		URL:              "/",
	})
	appSvc.SetMainWindow(win)

	// Quitting via the tray sets the flag so the close hook below lets the
	// window actually close during shutdown.
	var quitting atomic.Bool

	// Window X: hide to tray (configurable) instead of closing. Hooks run
	// before the built-in close listener; Cancel() stops it.
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if quitting.Load() || !appSvc.CloseActionIsTray() {
			return
		}
		e.Cancel()
		win.Hide()
	})

	// System tray: quick account switching, proxy toggle, quit.
	setupTray(app, appSvc, &quitting)

	// Stop the managed proxy child when the app quits (external proxies are
	// untouched by design).
	app.OnShutdown(func() {
		_ = appSvc.StopProxy()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
