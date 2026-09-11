package ccdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverInterruptedSwap(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".commandcode")
	t.Setenv("COMMANDCODE_HOME", dir)
	temp := filepath.Join(home, "."+filepath.Base(dir)+".ccdesktop-tmp")

	const auth = `{"apiKey":"user_x","userId":"u-1","userName":"a","keyName":"k","authenticatedAt":"t"}`

	t.Run("no swap marker", func(t *testing.T) {
		if RecoverInterruptedSwap() {
			t.Fatal("reported recovery without a marker")
		}
	})

	t.Run("restores displaced dir", func(t *testing.T) {
		os.MkdirAll(temp, 0o700)
		os.WriteFile(filepath.Join(temp, "auth.json"), []byte(auth), 0o600)
		if !RecoverInterruptedSwap() {
			t.Fatal("expected recovery")
		}
		got, err := os.ReadFile(filepath.Join(dir, "auth.json"))
		if err != nil || string(got) != auth {
			t.Fatalf("auth.json not restored: %q %v", got, err)
		}
		if _, err := os.Stat(temp); !os.IsNotExist(err) {
			t.Fatal("temp dir left behind")
		}
	})

	t.Run("keeps both when already logged in", func(t *testing.T) {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600)
		os.MkdirAll(temp, 0o700)
		os.WriteFile(filepath.Join(temp, "auth.json"), []byte(auth), 0o600)
		if RecoverInterruptedSwap() {
			t.Fatal("must not clobber a valid current credential")
		}
		if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
			t.Fatalf("current credential vanished: %v", err)
		}
	})
}

func TestDirHonorsEnvOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom")
	t.Setenv("COMMANDCODE_HOME", custom)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Fatalf("Dir() = %q, want %q", got, custom)
	}
}
