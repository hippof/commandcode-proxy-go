// Package creds parses and validates Command Code CLI credential files.
//
// The CLI writes ~/.commandcode/auth.json containing one account's API key
// plus identity fields. It also keeps telemetry-install-id and updates.json in
// the same directory, which are install-level (device) state, not account
// state — they survive login and are deliberately not managed here.
package creds

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Auth is a Command Code credential set: what the CLI persists in auth.json.
type Auth struct {
	APIKey          string `json:"apiKey"`
	UserID          string `json:"userId"`
	UserName        string `json:"userName"`
	KeyName         string `json:"keyName"`
	AuthenticatedAt string `json:"authenticatedAt"`
}

// Parse decodes and validates an auth.json payload.
func Parse(data []byte) (*Auth, error) {
	var a Auth
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("auth.json is not valid JSON: %w", err)
	}
	a.APIKey = strings.TrimSpace(a.APIKey)
	a.UserID = strings.TrimSpace(a.UserID)
	if a.APIKey == "" {
		return nil, fmt.Errorf("auth.json has no apiKey")
	}
	if a.UserID == "" {
		return nil, fmt.Errorf("auth.json has no userId")
	}
	return &a, nil
}

// SafeID sanitizes a userId for use as a single path segment, so a malformed
// or hostile credential file cannot escape its vault directory.
func SafeID(userID string) string {
	var b strings.Builder
	for _, r := range userID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
}

// MaskKey renders a key for display without revealing it, e.g. "user_4Nt…ZLn".
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 12 {
		return strings.Repeat("•", len(r))
	}
	return string(r[:8]) + "…" + string(r[len(r)-4:])
}

// RandomName generates a collision-resistant suffix for swap backups.
func RandomName() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "x"
	}
	return hex.EncodeToString(buf[:])
}

// SwapEntry names a file captured from the credentials directory during a
// login swap (the path within the directory is always a base name — never a
// subpath).
type SwapEntry struct {
	Name       string `json:"name"`
	BackupName string `json:"backupName,omitempty"`
}

// IsSafeBaseName rejects anything that isn't a bare file name.
func IsSafeBaseName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`) && name != "." && name != ".." && utf8.ValidString(name)
}
