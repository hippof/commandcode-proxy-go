package login

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"commandcode-desktop/internal/creds"
)

// setTerminal replaces openTerminalFn for the duration of a test. The fake
// runs the given "terminal" function (simulating the CLI writing auth.json)
// and returns a no-op started command.
func setTerminal(t *testing.T, fn func()) {
	t.Helper()
	prev := openTerminalFn
	openTerminalFn = func(string) (*exec.Cmd, error) {
		if fn != nil {
			go fn()
		}
		return exec.Command("cmd", "/c", "exit 0"), nil
	}
	t.Cleanup(func() { openTerminalFn = prev })
}

func withTempCLI(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".commandcode")
	t.Setenv("COMMANDCODE_HOME", dir)
	return dir
}

func waitFor(t *testing.T, s *Session, phase Phase) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.State(); st.Phase == phase {
			return st
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("never reached %q; stuck at %q", phase, s.State().Phase)
	return Status{}
}

const origJSON = `{"apiKey":"user_orig","userId":"u-orig","userName":"orig","keyName":"k","authenticatedAt":"x"}`
const newJSON = `{"apiKey":"user_new","userId":"u-new","userName":"newbie","keyName":"k","authenticatedAt":"x"}`

func TestLoginSucceedsAndArchives(t *testing.T) {
	dir := withTempCLI(t)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "auth.json"), []byte(origJSON), 0o600)

	setTerminal(t, func() {
		time.Sleep(50 * time.Millisecond)
		os.MkdirAll(dir, 0o700) // CLI recreates its home dir on login
		os.WriteFile(filepath.Join(dir, "auth.json"), []byte(newJSON), 0o600)
	})

	var mu sync.Mutex
	var archived *creds.Auth
	s := NewSession(nil)
	s.SetSuccessHook(func(a *creds.Auth, raw []byte) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		archived = a
		return true, nil
	})
	if err := s.Begin(""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, PhaseDone)

	mu.Lock()
	defer mu.Unlock()
	if archived == nil || archived.UserID != "u-new" {
		t.Fatalf("not archived: %+v", archived)
	}
	got, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil || string(got) != origJSON {
		t.Fatalf("original not restored: %q %v", got, err)
	}
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	if _, err := os.Stat(filepath.Join(parent, "."+base+".ccdesktop-tmp")); !os.IsNotExist(err) {
		t.Fatal("temp dir not cleaned")
	}
}

func TestNoOriginalAccountSucceeds(t *testing.T) {
	dir := withTempCLI(t) // nothing yet
	setTerminal(t, func() {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "auth.json"), []byte(newJSON), 0o600)
	})
	s := NewSession(nil)
	var isNew bool
	s.SetSuccessHook(func(*creds.Auth, []byte) (bool, error) { isNew = true; return true, nil })
	if err := s.Begin(""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, PhaseDone)
	if !isNew {
		t.Fatal("expected isNew=true")
	}
}

func TestLoginAbandonedRestoresOriginal(t *testing.T) {
	dir := withTempCLI(t)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "auth.json"), []byte(origJSON), 0o600)
	setTerminal(t, nil) // never writes

	s := NewSession(nil)
	s.SetSuccessHook(func(*creds.Auth, []byte) (bool, error) { return true, nil })
	if err := s.Begin(""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	s.Abort()
	waitFor(t, s, PhaseRestored)
	got, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	if string(got) != origJSON {
		t.Fatalf("original not restored: %q", got)
	}
}

func TestLoginExistingAccountNotReArchived(t *testing.T) {
	dir := withTempCLI(t)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "auth.json"), []byte(origJSON), 0o600)
	setTerminal(t, func() {
		time.Sleep(50 * time.Millisecond)
		os.MkdirAll(dir, 0o700)
		// user logs back into the SAME (already-vaulted) account
		os.WriteFile(filepath.Join(dir, "auth.json"), []byte(origJSON), 0o600)
	})
	s := NewSession(nil)
	var isNew = true
	s.SetSuccessHook(func(*creds.Auth, []byte) (bool, error) { isNew = false; return false, nil })
	if err := s.Begin(""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, PhaseRestored) // known account → restored, not done
	if isNew {
		t.Fatal("expected isNew=false path")
	}
}

func TestRecoverAfterRestart(t *testing.T) {
	dir := withTempCLI(t)
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	temp := filepath.Join(parent, "."+base+".ccdesktop-tmp")
	os.MkdirAll(temp, 0o700)
	os.WriteFile(filepath.Join(temp, "auth.json"), []byte(origJSON), 0o600)
	if !RecoverAfterRestart() {
		t.Fatal("RecoverAfterRestart false")
	}
	got, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil || string(got) != origJSON {
		t.Fatalf("not recovered: %q %v", got, err)
	}
}
