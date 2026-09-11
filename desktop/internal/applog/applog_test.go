package applog

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetState clears the package-level logger so tests are order-independent.
func resetState() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
	}
	dir, file, fileDay, minLvl, keep = "", nil, "", LevelInfo, 0
}

// withClock fixes "now" so rotation and pruning are testable.
func withClock(t *testing.T, day string) {
	t.Helper()
	prev := nowFn
	base, err := time.ParseInLocation("2006-01-02 15:04:05", day+" 12:00:00", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	nowFn = func() time.Time { return base }
	t.Cleanup(func() {
		nowFn = prev
		resetState()
	})
}

func readLog(t *testing.T, dir, day string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, day+".log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func TestWritesDailyFileWithLevels(t *testing.T) {
	dir := t.TempDir()
	withClock(t, "2026-09-11")
	if err := Init(dir, "info", 0); err != nil {
		t.Fatal(err)
	}

	Info("tray", "已切换账号", "account", "alice")
	Warn("gateway", "没有激活账号，已返回 401", "path", "/v1/messages")
	Error("proxy", errors.New("listen tcp 127.0.0.1:8787: bind failed"), "addr", "127.0.0.1:8787")
	Debug("proxy", "这条不该出现")

	got := readLog(t, dir, "2026-09-11")
	if !strings.Contains(got, "[INFO] tray: 已切换账号 account=alice") {
		t.Fatalf("info line missing:\n%s", got)
	}
	if !strings.Contains(got, "[WARN] gateway:") || !strings.Contains(got, "[ERROR] proxy:") {
		t.Fatalf("warn/error lines missing:\n%s", got)
	}
	if strings.Contains(got, "这条不该出现") {
		t.Fatalf("debug line written at info level:\n%s", got)
	}
	if n := strings.Count(strings.TrimSpace(got), "\n"); n != 2 {
		t.Fatalf("want exactly 3 lines, got %d extra:\n%s", n, got)
	}
}

func TestDebugLevelShowsEverything(t *testing.T) {
	dir := t.TempDir()
	withClock(t, "2026-09-11")
	if err := Init(dir, "debug", 0); err != nil {
		t.Fatal(err)
	}
	Debug("app", "启动")
	if !strings.Contains(readLog(t, dir, "2026-09-11"), "[DEBUG] app: 启动") {
		t.Fatal("debug line missing at debug level")
	}
}

// TestRotatesByDayAndPrunes: a new day starts a new file and old files beyond
// the retention window disappear.
func TestRotatesByDayAndPrunes(t *testing.T) {
	dir := t.TempDir()
	// Pre-existing old files.
	for _, day := range []string{"2026-08-01", "2026-09-01", "2026-09-10"} {
		if err := os.WriteFile(filepath.Join(dir, day+".log"), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	withClock(t, "2026-09-11")
	if err := Init(dir, "info", 3); err != nil { // keeps 09-08 onward
		t.Fatal(err)
	}
	Info("app", "第一天")

	// Next day: a new file appears without restarting the app.
	prev := nowFn
	nowFn = func() time.Time { return prev().Add(24 * time.Hour) }
	Info("app", "第二天")
	nowFn = prev

	if _, err := os.Stat(filepath.Join(dir, "2026-09-12.log")); err != nil {
		t.Fatalf("next day's file missing: %v", err)
	}
	if got := readLog(t, dir, "2026-09-12"); !strings.Contains(got, "第二天") {
		t.Fatalf("new file does not contain its entry:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-09-01.log")); !os.IsNotExist(err) {
		t.Fatal("file older than the retention window was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08-01.log")); !os.IsNotExist(err) {
		t.Fatal("file older than the retention window was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-09-10.log")); err != nil {
		t.Fatalf("file inside the window was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("non-log files must never be touched")
	}

	files := Files()
	if len(files) == 0 || !strings.HasSuffix(files[0], "2026-09-12.log") {
		t.Fatalf("Files() should list newest first, got %v", files)
	}
}

// TestInitIsInertUntilCalled: library/test use must not touch the disk.
func TestInitIsInertUntilCalled(t *testing.T) {
	resetState()
	dir := t.TempDir()
	// No Init: writes go nowhere (only stderr for warn+).
	Error("x", errors.New("boom"))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %v before Init", entries)
	}
	if Enabled() || Path() != "" {
		t.Fatal("logging should be disabled before Init")
	}
}

// TestConcurrentWritesStayLineAligned guards the mutex + single handle.
func TestConcurrentWritesStayLineAligned(t *testing.T) {
	dir := t.TempDir()
	withClock(t, "2026-09-11")
	if err := Init(dir, "debug", 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			Info("concurrent", "line", "i", i)
			Error("concurrent", errors.New("failure"), "i", i)
		}(i)
	}
	wg.Wait()

	got := readLog(t, dir, "2026-09-11")
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 80 {
		t.Fatalf("want 80 lines, got %d", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "2026-09-11 ") || !strings.Contains(l, "[") {
			t.Fatalf("malformed line: %q", l)
		}
	}
}

// TestMultilineMessagesStayOneLine: a stack-trace-ish message must not break
// the one-entry-per-line contract.
func TestMultilineMessagesStayOneLine(t *testing.T) {
	dir := t.TempDir()
	withClock(t, "2026-09-11")
	if err := Init(dir, "info", 0); err != nil {
		t.Fatal(err)
	}
	Error("startup", errors.New("line one\nline two\r\nline three"))
	got := strings.TrimSpace(readLog(t, dir, "2026-09-11"))
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("entry spans multiple lines:\n%q", got)
	}
	if !strings.Contains(got, "line one\\nline two") {
		t.Fatalf("newlines should be escaped, got %q", got)
	}
}

// TestInstallRoutesStandardLogger: main's log.Printf output must land in the
// daily file too.
func TestInstallRoutesStandardLogger(t *testing.T) {
	dir := t.TempDir()
	withClock(t, "2026-09-11")
	if err := Init(dir, "info", 0); err != nil {
		t.Fatal(err)
	}
	Install()
	log.Printf("代理已就绪")

	if got := readLog(t, dir, "2026-09-11"); !strings.Contains(got, "代理已就绪") {
		t.Fatalf("standard logger output missing from the file:\n%s", got)
	}
}
