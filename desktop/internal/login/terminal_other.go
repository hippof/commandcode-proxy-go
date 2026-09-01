//go:build !windows && !darwin

package login

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// openTerminal launches the login command in the first available graphical
// terminal emulator (Linux/BSD), or a plain detached process as a last resort.
func openTerminal(cmd string) (*exec.Cmd, error) {
	terminals := [][]string{
		{"gnome-terminal", "--"},
		{"konsole", "-e"},
		{"xfce4-terminal", "-e"},
		{"x-terminal-emulator", "-e"},
	}
	for _, t := range terminals {
		if _, err := exec.LookPath(t[0]); err == nil {
			args := append(t[1:], "sh", "-c", cmd+" login; exec bash")
			c := exec.Command(t[0], args...)
			c.Env = os.Environ()
			if err := c.Start(); err == nil {
				return c, nil
			}
		}
	}
	// Fallback: run in a detached shell writing to a log the user can tail.
	sh := exec.Command("sh", "-c", cmd+" login")
	sh.Env = os.Environ()
	if err := sh.Start(); err != nil {
		return nil, fmt.Errorf("no terminal emulator found and login failed to start (%s): %w", strings.TrimSpace(cmd), err)
	}
	return sh, nil
}
