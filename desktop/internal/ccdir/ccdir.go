// Package ccdir accesses the Command Code CLI credential directory
// (~/.commandcode on all platforms the CLI supports).
package ccdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"commandcode-desktop/internal/creds"
)

// AuthFileName is the credential file the CLI reads/writes inside Dir.
const AuthFileName = "auth.json"

// Dir returns the credential directory: $COMMANDCODE_HOME if set, else
// ~/.commandcode. The CLI is a Node program whose homedir() resolves to
// USERPROFILE on Windows (verified: %USERPROFILE%\.commandcode).
func Dir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("COMMANDCODE_HOME")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".commandcode"), nil
}

// AuthPath is the full path of auth.json.
func AuthPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, AuthFileName), nil
}

// ReadAuth loads and validates the active auth.json.
func ReadAuth() (*creds.Auth, []byte, error) {
	p, err := AuthPath()
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, err
	}
	a, err := creds.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p, err)
	}
	return a, data, nil
}

// Install atomically replaces auth.json with data, creating the directory if
// needed.
func Install(data []byte) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(d, AuthFileName), data)
}

// atomicWrite writes via a temp file + rename in the same directory.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ccwrite-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// List returns the names of plain files in the credential directory,
// excluding the *.bak-* backups this app creates.
func List() ([]string, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if strings.Contains(e.Name(), ".bak-") {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// Rename moves a file inside the credential directory (within Dir) and
// returns its new name. Used to set the CLI's credential aside during login.
func Rename(from, to string) error {
	if !creds.IsSafeBaseName(from) || !creds.IsSafeBaseName(to) {
		return fmt.Errorf("unsafe file name")
	}
	d, err := Dir()
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(d, from), filepath.Join(d, to))
}

// Remove deletes a file inside the credential directory (within Dir).
func Remove(name string) error {
	if !creds.IsSafeBaseName(name) {
		return fmt.Errorf("unsafe file name")
	}
	d, err := Dir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(d, name))
}

// StatAuth reports whether auth.json currently exists.
func AuthExists() bool {
	p, err := AuthPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}
