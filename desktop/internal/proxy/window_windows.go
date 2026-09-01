//go:build windows

package proxy

import "syscall"

// hideWindow prevents console windows from flashing when the GUI app spawns
// child processes (go build, the proxy itself).
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
