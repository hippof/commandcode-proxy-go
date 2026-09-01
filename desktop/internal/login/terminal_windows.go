//go:build windows

package login

import (
	"os"
	"os/exec"
	"syscall"
)

// openTerminal launches `<cmd> login` in its OWN visible, interactive console
// via CREATE_NEW_CONSOLE. `/k` keeps the window open after the command so the
// user can read the browser link / any error; the CLI (cmdc) is an Ink TUI
// that needs a real tty and opens the browser itself. This avoids the fragile
// `start "title" …` quoting that a GUI (console-less) parent gets wrong.
func openTerminal(cmd string) (*exec.Cmd, error) {
	c := exec.Command("cmd.exe", "/k", cmd+" login")
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
	c.Env = os.Environ()
	if err := c.Start(); err != nil {
		return nil, err
	}
	return c, nil
}
