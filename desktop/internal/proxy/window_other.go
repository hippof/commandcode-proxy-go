//go:build !windows

package proxy

import "syscall"

func hideWindow() *syscall.SysProcAttr { return nil }
