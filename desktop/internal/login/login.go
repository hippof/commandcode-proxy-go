// Package login drives the Command Code CLI sign-in while keeping the
// currently active account untouched.
//
// The CLI login wizard is an interactive terminal program (an Ink TUI that
// needs a real tty and opens the browser itself). Rather than fight the GUI
// process's lack of a console, we launch it inside the user's OWN terminal
// app (Windows Terminal / cmd, macOS Terminal, Linux x-terminal-emulator)
// running `<cli> login`. The user sees the familiar prompt and finishes the
// browser authorization there.
//
// To avoid clobbering the active account, the whole credential directory is
// swapped aside for the duration (move ~/.commandcode → a hidden temp dir),
// so the CLI starts logged-out and the browser actually opens. When a fresh
// auth.json appears (the CLI writes it, then the terminal closes), we archive
// it into the vault and restore the original directory. The temp dir doubles
// as a swap marker; RecoverAfterRestart puts it back if the app died mid-login.
package login

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"commandcode-desktop/internal/ccdir"
	"commandcode-desktop/internal/creds"
)

// loginCommand resolves the shell token that starts the sign-in wizard:
// COMMANDCODE_CLI env > config override > platform default (cmdc on Windows
// — `cmd` there is the shell itself; cmd/cmdc/command-code/commandcode are all
// registered bins of the CLI package). When the name is not on this process's
// PATH (common when launched from Explorer without a shell profile), known
// npm/vfox global locations are probed and the full path returned.
func loginCommand(override string) string {
	for _, v := range []string{os.Getenv("COMMANDCODE_CLI"), override} {
		if v != "" {
			return v
		}
	}
	names := []string{"cmdc"}
	if runtime.GOOS == "windows" {
		names = []string{"cmdc", "commandcode", "command-code"}
	} else {
		names = []string{"cmd", "cmdc", "commandcode", "command-code"}
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil && !isWindowsShell(p) {
			return quote(p)
		}
	}
	if runtime.GOOS == "windows" {
		home, _ := os.UserHomeDir()
		for _, dir := range []string{
			filepath.Join(home, ".vfox", "sdks", "nodejs"),
			filepath.Join(home, "AppData", "Roaming", "npm"),
		} {
			for _, name := range []string{"cmdc.cmd", "commandcode.cmd"} {
				if p := filepath.Join(dir, name); fileExists(p) {
					return quote(p)
				}
			}
		}
	}
	return names[0] // last resort: bare token, the terminal shows a clear error
}

// isWindowsShell guards against LookPath("cmd") resolving to cmd.exe itself.
func isWindowsShell(p string) bool {
	return runtime.GOOS == "windows" && strings.EqualFold(filepath.Base(p), "cmd.exe")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// quote wraps a path for embedding in a cmd/sh command line.
func quote(p string) string {
	if strings.ContainsAny(p, " &()") {
		return `"` + p + `"`
	}
	return p
}

// openTerminalFn is the launch hook (tests override it); the real
// implementations are platform-specific (terminal_*.go).
var openTerminalFn = openTerminal

// Phase enumerates the login state machine.
type Phase string

const (
	PhaseIdle      Phase = "idle"
	PhaseSwapping  Phase = "swapping"
	PhaseWaiting   Phase = "waiting" // terminal spawned; awaiting auth.json
	PhaseFinishing Phase = "finishing"
	PhaseDone      Phase = "done"     // new/known account handled, original restored
	PhaseRestored  Phase = "restored" // aborted/failed; original restored
	PhaseError     Phase = "error"
)

// Status is the UI-facing progress snapshot, emitted as an app event and
// returned from State().
type Status struct {
	Phase   Phase  `json:"phase"`
	Message string `json:"message"`
}

// Session is one login flow (at most one active per app).
type Session struct {
	mu        sync.Mutex
	phase     Phase
	message   string
	emit      func(Status)
	onSuccess func(*creds.Auth, []byte) (bool, error)
	terminal  *exec.Cmd
	aborted   chan struct{}
	done      chan struct{} // closed to end the watcher after a result
}

// NewSession creates an idle session. emit may be nil.
func NewSession(emit func(Status)) *Session {
	return &Session{emit: emit, phase: PhaseIdle, aborted: make(chan struct{}), done: make(chan struct{})}
}

// SetSuccessHook wires the archive callback: returns (isNew, error).
func (s *Session) SetSuccessHook(fn func(a *creds.Auth, raw []byte) (bool, error)) {
	s.mu.Lock()
	s.onSuccess = fn
	s.mu.Unlock()
}

func (s *Session) setStatus(p Phase, msg string) {
	s.mu.Lock()
	s.phase, s.message = p, msg
	emit := s.emit
	s.mu.Unlock()
	if emit != nil {
		emit(Status{Phase: p, Message: msg})
	}
}

// State returns the current status.
func (s *Session) State() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Phase: s.phase, Message: s.message}
}

// Begin swaps the credential directory aside and opens the user's terminal
// running `<cli> login`. It returns once the terminal is launched; the result
// is delivered asynchronously via the success hook + status events.
func (s *Session) Begin(cliOverride string) error {
	s.mu.Lock()
	busy := s.phase == PhaseWaiting || s.phase == PhaseSwapping || s.phase == PhaseFinishing
	if !busy {
		s.aborted = make(chan struct{})
		s.done = make(chan struct{})
	}
	s.mu.Unlock()
	if busy {
		return fmt.Errorf("a login is already in progress")
	}

	dir, err := ccdir.Dir()
	if err != nil {
		s.setStatus(PhaseError, err.Error())
		return err
	}
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	temp := filepath.Join(parent, "."+base+".ccdesktop-tmp")

	s.setStatus(PhaseSwapping, "正在准备干净的登录环境（当前账号会被临时挪开）…")

	hadDir := true
	if err := os.Rename(dir, temp); err != nil {
		if !os.IsNotExist(err) {
			s.setStatus(PhaseError, fmt.Sprintf("无法临时挪走当前凭据目录：%v", err))
			return err
		}
		hadDir = false
	}
	if !hadDir {
		temp = ""
	}

	cmd, err := openTerminalFn(loginCommand(cliOverride))
	if err != nil {
		if temp != "" {
			_ = os.Rename(temp, dir)
		}
		s.setStatus(PhaseError, err.Error())
		return err
	}
	s.mu.Lock()
	s.terminal = cmd
	s.mu.Unlock()

	s.setStatus(PhaseWaiting, fmt.Sprintf("已打开终端窗口并执行 `%s login` — 请在其中完成浏览器授权。检测到新 auth.json 后，程序会自动入库并恢复原账号（终端窗口留着看结果或手动关闭均可）。", loginCommand(cliOverride)))
	go s.watch(dir, temp)
	return nil
}

// watch polls for a fresh auth.json in the (emptied) original dir. It does
// NOT depend on the terminal process (which detaches), only on the credential
// file appearing, with a generous timeout.
func (s *Session) watch(dir, temp string) {
	const timeout = 15 * time.Minute
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	s.mu.Lock()
	aborted := s.aborted
	done := s.done
	s.mu.Unlock()

	for {
		select {
		case <-tick.C:
		case <-aborted:
			s.restore(PhaseRestored, temp, dir, "登录已取消 — 原账号已恢复。")
			return
		case <-done:
			return
		}
		if a, raw, err := readFresh(dir); err == nil {
			s.finish(a, raw, dir, temp)
			return
		}
		if time.Now().After(deadline) {
			s.restore(PhaseRestored, temp, dir, "等待登录超时（15 分钟）— 原账号已恢复，终端窗口可手动关闭。")
			return
		}
	}
}

func readFresh(dir string) (*creds.Auth, []byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return nil, nil, err
	}
	a, err := creds.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return a, data, nil
}

func (s *Session) finish(a *creds.Auth, raw []byte, dir, temp string) {
	s.setStatus(PhaseFinishing, fmt.Sprintf("已以 %s 登录 — 正在存入保管库…", displayName(a)))
	isNew := true
	var err error
	s.mu.Lock()
	hook := s.onSuccess
	s.mu.Unlock()
	if hook != nil {
		isNew, err = hook(a, raw)
	}
	switch {
	case err != nil:
		s.restore(PhaseRestored, temp, dir, fmt.Sprintf("保存账号失败（%v）— 已恢复原账号。", err))
	case !isNew:
		s.restore(PhaseRestored, temp, dir, fmt.Sprintf("%s 已在保管库中，未重复添加。当前账号未受影响。", displayName(a)))
	default:
		s.restore(PhaseDone, temp, dir, fmt.Sprintf("已添加 %s 到保管库，当前账号保持不变 — 在列表里点「切换」才会激活它。", displayName(a)))
	}
}

func displayName(a *creds.Auth) string {
	if a.UserName != "" {
		return a.UserName
	}
	return a.UserID
}

// restore puts the original directory back (if any), ends the watcher, and
// reports the final state.
func (s *Session) restore(phase Phase, temp, dir, msg string) {
	if temp != "" {
		if _, err := os.Stat(dir); err == nil {
			_ = os.RemoveAll(dir)
		}
		_ = os.Rename(temp, dir)
	}
	s.mu.Lock()
	s.terminal = nil
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.mu.Unlock()
	s.setStatus(phase, msg)
}

// Abort stops a running login; the watcher restores the original dir.
func (s *Session) Abort() {
	s.mu.Lock()
	ch := s.aborted
	s.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// RecoverAfterRestart restores the original credential directory if the app
// died mid-login. Returns true when something was put back.
func RecoverAfterRestart() bool {
	dir, err := ccdir.Dir()
	if err != nil {
		return false
	}
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	temp := filepath.Join(parent, "."+base+".ccdesktop-tmp")
	if _, err := os.Stat(temp); err != nil {
		return false
	}
	if _, err := os.Stat(dir); err == nil {
		if _, _, verr := readFresh(dir); verr != nil {
			_ = os.RemoveAll(dir)
			return os.Rename(temp, dir) == nil
		}
		return false
	}
	return os.Rename(temp, dir) == nil
}
