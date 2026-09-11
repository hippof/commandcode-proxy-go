//go:build windows

package main

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// balloonRec records what would have been shown as a tray notification.
type balloonRec struct {
	cmd   uintptr
	uid   uint32
	title string
	body  string
	level uint32
}

// stubShell swaps the shell + tray-window lookups for the duration of a test.
// acceptUID decides which icon id the shell "finds" (0 = refuse everything).
func stubShell(t *testing.T, trayHWND uintptr, acceptUID uint32) (*[]balloonRec, *[]string) {
	t.Helper()
	prevNotify, prevFind := shellNotifyFn, findTrayWindowFn
	recs := []balloonRec{}
	fallbacks := []string{}

	shellNotifyFn = func(cmd uintptr, nid *w32.NOTIFYICONDATA) bool {
		if acceptUID == 0 || nid.UID != acceptUID {
			return false
		}
		recs = append(recs, balloonRec{
			cmd:   cmd,
			uid:   nid.UID,
			title: syscall.UTF16ToString(nid.SzInfoTitle[:]),
			body:  syscall.UTF16ToString(nid.SzInfo[:]),
			level: nid.DwInfoFlags,
		})
		return true
	}
	findTrayWindowFn = func() uintptr { return trayHWND }

	prevSink := messageBoxSink
	messageBoxSink = func(text, caption string, flags uint) int {
		fallbacks = append(fallbacks, caption+"|"+text)
		return w32.IDOK
	}
	t.Cleanup(func() {
		shellNotifyFn, findTrayWindowFn, messageBoxSink = prevNotify, prevFind, prevSink
	})
	return &recs, &fallbacks
}

// waitForBalloons waits until at least n notifications were recorded.
func waitForBalloons(t *testing.T, recs *[]balloonRec, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(*recs) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d balloon(s), got %d", n, len(*recs))
}

func TestInfoShowsBalloonNotDialog(t *testing.T) {
	recs, fallbacks := stubShell(t, 0x1234, 1)
	var n notifier = &nativeNotifier{}

	n.Info("保存成功", "已新增账号 alice")
	waitForBalloons(t, recs, 1)

	got := (*recs)[0]
	if got.cmd != w32.NIM_MODIFY {
		t.Fatalf("cmd = %d, want NIM_MODIFY", got.cmd)
	}
	if got.level != w32.NIIF_INFO {
		t.Fatalf("level = %#x, want NIIF_INFO", got.level)
	}
	if got.title != "保存成功" || got.body != "已新增账号 alice" {
		t.Fatalf("balloon = %q / %q", got.title, got.body)
	}
	if len(*fallbacks) != 0 {
		t.Fatalf("a balloon was shown, so no dialog should appear: %v", *fallbacks)
	}
}

func TestFailShowsErrorBalloon(t *testing.T) {
	recs, _ := stubShell(t, 0x1234, 1)
	var n notifier = &nativeNotifier{}

	n.Fail("切换失败", errors.New("保管库里没有这个账号"))
	waitForBalloons(t, recs, 1)

	got := (*recs)[0]
	if got.level != w32.NIIF_ERROR {
		t.Fatalf("level = %#x, want NIIF_ERROR", got.level)
	}
	if got.title != "切换失败" || !strings.Contains(got.body, "保管库里没有这个账号") {
		t.Fatalf("balloon = %q / %q", got.title, got.body)
	}
}

// TestBalloonFallsBackToDialogWhenRefused: notifications can be turned off by
// policy, and the user must not be left blind.
func TestBalloonFallsBackToDialogWhenRefused(t *testing.T) {
	_, fallbacks := stubShell(t, 0x1234, 0) // shell refuses every balloon
	var n notifier = &nativeNotifier{}

	n.Fail("切换失败", errors.New("凭证无效"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(*fallbacks) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*fallbacks) != 1 {
		t.Fatalf("want one fallback dialog, got %v", *fallbacks)
	}
	if !strings.Contains((*fallbacks)[0], "切换失败") || !strings.Contains((*fallbacks)[0], "凭证无效") {
		t.Fatalf("fallback dialog = %q", (*fallbacks)[0])
	}
}

// TestBalloonFallsBackWhenNoTrayWindow: if the tray window cannot be found,
// notifications degrade to a dialog instead of vanishing.
func TestBalloonFallsBackWhenNoTrayWindow(t *testing.T) {
	_, fallbacks := stubShell(t, 0, 1)
	var n notifier = &nativeNotifier{}

	n.Info("已切换账号", "当前登录：alice")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(*fallbacks) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*fallbacks) != 1 {
		t.Fatalf("want one fallback dialog, got %v", *fallbacks)
	}
}

// TestBalloonProbesIconID covers the icon id wails assigns internally: the
// first id that the shell accepts is remembered and reused.
func TestBalloonProbesIconID(t *testing.T) {
	recs, _ := stubShell(t, 0x1234, 3) // only the third candidate exists
	n := &nativeNotifier{}

	n.Info("一", "第一次")
	waitForBalloons(t, recs, 1)
	if (*recs)[0].uid != 3 {
		t.Fatalf("uid = %d, want the accepted 3", (*recs)[0].uid)
	}
	if n.uid != 3 {
		t.Fatalf("probed uid not cached: %d", n.uid)
	}

	n.Info("二", "第二次")
	waitForBalloons(t, recs, 2)
	if (*recs)[1].uid != 3 {
		t.Fatalf("second balloon should reuse uid 3, got %d", (*recs)[1].uid)
	}
}

// TestBalloonClipsLongText: the balloon fields are fixed-size buffers, so long
// summaries must be clipped rather than silently truncated mid-rune.
func TestBalloonClipsLongText(t *testing.T) {
	recs, _ := stubShell(t, 0x1234, 1)
	var n notifier = &nativeNotifier{}

	long := strings.Repeat("套餐额度明细-", 200)
	n.Info("套餐额度", long)
	waitForBalloons(t, recs, 1)

	got := (*recs)[0]
	if n := len([]rune(got.body)); n > 255 {
		t.Fatalf("body has %d runes, exceeds the buffer", n)
	}
	if !strings.HasSuffix(got.body, "…") {
		t.Fatalf("clipped body should end with an ellipsis: %q", got.body)
	}
	if len([]rune(got.title)) > 63 {
		t.Fatalf("title too long: %q", got.title)
	}
}

// TestAskRunsCallbackOnlyOnYes is the regression guard for why this notifier
// exists at all: wails' Windows dialog matched callbacks by English button
// labels, so a localized confirm button did nothing.
func TestAskRunsCallbackOnlyOnYes(t *testing.T) {
	prev := messageBoxFunc
	t.Cleanup(func() { messageBoxFunc = prev })

	calls := []uint{}
	texts := []string{}
	answer := w32.IDYES
	messageBoxFunc = func(text, caption string, flags uint) int {
		texts = append(texts, text)
		calls = append(calls, flags)
		return answer
	}

	var n notifier = &nativeNotifier{}
	done := make(chan struct{})
	n.Ask("删除保管库账号", "只删除保存的副本。", "删除", func() { close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("confirming the dialog did not run the action")
	}
	if calls[0]&w32.MB_YESNO == 0 || calls[0]&w32.MB_ICONQUESTION == 0 {
		t.Fatalf("want a Yes/No question box, flags = %#x", calls[0])
	}
	if !strings.Contains(texts[0], "删除") {
		t.Fatalf("question text should name the action: %q", texts[0])
	}

	// Declining must leave everything untouched.
	answer = w32.IDNO
	ran := make(chan struct{})
	n.Ask("删除保管库账号", "只删除保存的副本。", "删除", func() { close(ran) })
	select {
	case <-ran:
		t.Fatal("declining must not run the action")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestConcurrentNotificationsAreSerialised guards the balloon state (hwnd/uid)
// against races: menu actions and the tooltip loop can both notify.
func TestConcurrentNotificationsAreSerialised(t *testing.T) {
	recs, _ := stubShell(t, 0x1234, 1)
	n := &nativeNotifier{}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.Info("并发", "通知")
		}()
	}
	wg.Wait()
	waitForBalloons(t, recs, 8)
}
