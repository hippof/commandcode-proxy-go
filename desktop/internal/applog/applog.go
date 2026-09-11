// Package applog writes the desktop app's own log, one file per day, so every
// failure anywhere in the app leaves a trace even when the user dismissed the
// notification.
//
// It is a leaf package (stdlib only) that stays inert until Init is called:
// tests and library use never touch the filesystem. Credentials must never be
// passed to it — helpers here only ever see names and masked identifiers.
package applog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level is the minimum severity that gets written.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel maps a config string to a level ("info" when unrecognized).
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

var (
	mu      sync.Mutex
	dir     string
	minLvl  Level = LevelInfo
	keep    int
	file    *os.File
	fileDay string
	nowFn   = time.Now
)

// TimeFormat is the daily file's name pattern (also how rotation is decided).
const TimeFormat = "2006-01-02"

// Init starts logging into dir, keeping at most keepDays day-files (<=0 keeps
// everything). It prunes old files once, up front.
func Init(logDir string, level string, keepDays int) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	dir, minLvl, keep = logDir, ParseLevel(level), keepDays
	pruneLocked(nowFn())
	return openLocked(nowFn())
}

// Path is the log directory ("" when logging is disabled).
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	return dir
}

// Enabled reports whether file logging is active.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return dir != ""
}

// Close flushes and closes the current file.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file, fileDay = nil, ""
	}
}

func Debug(component, msg string, kv ...any) { write(LevelDebug, component, msg, kv...) }
func Info(component, msg string, kv ...any)  { write(LevelInfo, component, msg, kv...) }
func Warn(component, msg string, kv ...any)  { write(LevelWarn, component, msg, kv...) }

// Error records a failure; err may be nil (the message alone is then logged).
func Error(component string, err error, kv ...any) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	write(LevelError, component, msg, kv...)
}

func write(lvl Level, component, msg string, kv ...any) {
	line := format(lvl, component, msg, kv...)
	mu.Lock()
	defer mu.Unlock()
	if dir == "" {
		// No file logging configured (tests, library use): only warn+ reaches
		// stderr so a dev run still shows problems.
		if lvl >= LevelWarn {
			fmt.Fprintln(os.Stderr, line)
		}
		return
	}
	if lvl < minLvl {
		return
	}
	now := nowFn()
	if fileDay != now.Format(TimeFormat) {
		if err := openLocked(now); err != nil {
			fmt.Fprintln(os.Stderr, line)
			return
		}
	}
	if file != nil {
		_, _ = file.WriteString(line + "\n")
	}
}

func format(lvl Level, component, msg string, kv ...any) string {
	ts := nowFn().Format("2006-01-02 15:04:05.000")
	sanitize := func(s string) string {
		s = strings.ReplaceAll(s, "\r", " ")
		s = strings.ReplaceAll(s, "\n", "\\n")
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s: %s", ts, lvl, component, sanitize(msg))
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %v=%v", kv[i], sanitize(fmt.Sprint(kv[i+1])))
	}
	return b.String()
}

// openLocked (re)opens today's file; the caller holds mu.
func openLocked(now time.Time) error {
	day := now.Format(TimeFormat)
	if file != nil && fileDay == day {
		return nil
	}
	if file != nil {
		_ = file.Close()
		file = nil
	}
	path := filepath.Join(dir, day+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	file, fileDay = f, day
	return nil
}

// pruneLocked deletes day-files older than the retention window.
func pruneLocked(now time.Time) {
	if keep <= 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -keep)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".log") {
			continue
		}
		day, err := time.ParseInLocation(TimeFormat, strings.TrimSuffix(name, ".log"), time.Local)
		if err != nil {
			continue // not one of ours
		}
		if day.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// Files lists the day-files, newest first (useful for tests and diagnostics).
func Files() []string {
	mu.Lock()
	defer mu.Unlock()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// Install routes the standard library logger (used by main and by any
// dependency that logs through it) into the same daily file, keeping stderr
// for development runs.
func Install() {
	mu.Lock()
	defer mu.Unlock()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if dir == "" {
		return
	}
	log.SetOutput(io.MultiWriter(stdWriter{}, os.Stderr))
}

// stdWriter appends standard-library log lines to today's file.
type stdWriter struct{}

func (stdWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	now := nowFn()
	if fileDay != now.Format(TimeFormat) {
		if err := openLocked(now); err != nil {
			return len(p), nil
		}
	}
	if file == nil {
		return len(p), nil
	}
	if _, err := file.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
