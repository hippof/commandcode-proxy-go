//go:build darwin

package login

import (
	"os"
	"os/exec"
)

// openTerminal asks Terminal.app to run `<cmd> login`, then returns. The
// `do script` idiom opens a new window in the user's default terminal.
func openTerminal(cmd string) (*exec.Cmd, error) {
	script := "tell application \"Terminal\"\r\nactivate\r\ndo script \"" + cmd + " login\"\r\nend tell"
	c := exec.Command("osascript", "-e", script)
	c.Env = os.Environ()
	if err := c.Run(); err != nil {
		return nil, err
	}
	return c, nil
}
