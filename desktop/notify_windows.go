//go:build windows

package main

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

// nativeNotifier reports outcomes as tray balloon notifications ("系统通知").
// Exactly one action still uses a modal dialog: the confirmation before
// deleting a saved credential, which must not be answerable by accident.
// Deactivating is notification-only because it is reversible (the vault copy
// stays and switching back restores it).
//
// wails' own dialog helper is avoided for callbacks: on Windows it dispatches
// a button's callback by comparing the button *label* to the English
// MessageBox result strings, which silently swallows any localized label.
type nativeNotifier struct {
	mu   sync.Mutex
	hwnd uintptr // the tray icon's message-only window
	uid  uint32  // the icon's id within that window (probed once, then cached)
}

func newNotifier(_ *application.App) notifier { return &nativeNotifier{} }

// ------------------------------------------------------------------ balloons

// Swappable indirections so tests can run without the shell or a real tray.
var (
	shellNotifyFn = func(cmd uintptr, nid *w32.NOTIFYICONDATA) bool {
		return w32.ShellNotifyIcon(cmd, nid)
	}
	findTrayWindowFn = findTrayWindow
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procFindWindowEx = user32.NewProc("FindWindowExW")
)

// trayWindowClass is wails' default window class (WindowsOptions.WndClass):
// the system tray owns a message-only window of this class, and this app
// creates no other windows.
const trayWindowClass = "WailsWebviewWindow"

// findTrayWindow locates the tray icon's message-only window.
func findTrayWindow() uintptr {
	hwndMessage := ^uintptr(2) // HWND_MESSAGE == (HWND)-3
	class, err := syscall.UTF16PtrFromString(trayWindowClass)
	if err != nil {
		return 0
	}
	h, _, _ := procFindWindowEx.Call(hwndMessage, 0, uintptr(unsafe.Pointer(class)), 0)
	return h
}

// clip shortens a string to at most max UTF-16 units (the balloon fields are
// fixed-size buffers).
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// balloon shows a notification attached to the tray icon. Returns false when
// the shell refused it (notifications can be disabled by policy), so callers
// can fall back to something visible.
func (n *nativeNotifier) balloon(title, body string, level uint32) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.hwnd == 0 {
		n.hwnd = findTrayWindowFn()
		if n.hwnd == 0 {
			return false
		}
	}
	nid := w32.NOTIFYICONDATA{
		CbSize:      uint32(unsafe.Sizeof(w32.NOTIFYICONDATA{})),
		HWnd:        w32.HWND(n.hwnd),
		UFlags:      w32.NIF_INFO,
		DwInfoFlags: level,
	}
	copy(nid.SzInfoTitle[:], syscall.StringToUTF16(clip(title, len(nid.SzInfoTitle)-1)))
	copy(nid.SzInfo[:], syscall.StringToUTF16(clip(body, len(nid.SzInfo)-1)))

	// The icon belongs to wails; probe a few ids so a change in how it numbers
	// them degrades into "no balloon" rather than "wrong icon".
	if n.uid != 0 {
		nid.UID = n.uid
		if shellNotifyFn(w32.NIM_MODIFY, &nid) {
			return true
		}
	}
	for uid := uint32(1); uid <= 4; uid++ {
		if uid == n.uid {
			continue
		}
		nid.UID = uid
		if shellNotifyFn(w32.NIM_MODIFY, &nid) {
			n.uid = uid
			return true
		}
	}
	return false
}

func iconForLevel(level uint32) uint {
	switch level {
	case w32.NIIF_ERROR:
		return w32.MB_ICONERROR
	case w32.NIIF_WARNING:
		return w32.MB_ICONWARNING
	default:
		return w32.MB_ICONINFORMATION
	}
}

// notify shows a balloon, falling back to a message box if the shell will not
// take one (disabled notifications, missing tray window, …).
func (n *nativeNotifier) notify(title, body string, level uint32) {
	go func() {
		if n.balloon(title, body, level) {
			return
		}
		_ = messageBoxSink(body, title, uint(iconForLevel(level))|w32.MB_OK|mbSetForeground)
	}()
}

func (n *nativeNotifier) Info(title, msg string) { n.notify(title, msg, w32.NIIF_INFO) }

func (n *nativeNotifier) Fail(title string, err error) {
	n.notify(title, fmt.Sprintf("%v", err), w32.NIIF_ERROR)
}

// ------------------------------------------------------------------- dialogs

// messageBoxFunc is swappable so tests can simulate the user's answer.
var messageBoxFunc = func(text, caption string, flags uint) int {
	return w32.MessageBox(0, text, caption, flags)
}

// messageBoxSink lets tests observe fallback dialogs as well.
var messageBoxSink = func(text, caption string, flags uint) int {
	return messageBoxFunc(text, caption, flags)
}

// mbSetForeground brings the box to the front (w32 does not define it).
const mbSetForeground = 0x00010000

// Ask shows the one modal question left: deleting a saved credential. The
// affirmative label is folded into the text because MessageBox captions come
// from the OS locale.
func (n *nativeNotifier) Ask(title, msg, yesLabel string, onYes func()) {
	go func() {
		text := msg
		if yesLabel != "" {
			text += "\n\n是否" + yesLabel + "？"
		}
		if messageBoxFunc(text, title, w32.MB_YESNO|w32.MB_ICONQUESTION|mbSetForeground) == w32.IDYES {
			if onYes != nil {
				onYes()
			}
		}
	}()
}
