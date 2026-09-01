// Package vault stores archived Command Code credentials (auth.json copies)
// so several accounts can coexist and be activated one at a time.
//
// Layout under the vault root (~/.commandcode-accounts by default):
//
//	active.json               — {"userId": "..."} last activated account
//	accounts/<safeId>/auth.json — the stored credential file, verbatim
//	accounts/<safeId>/meta.json — display metadata
//	accounts/<safeId>/plan.json — cached plan probe (optional)
package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"commandcode-desktop/internal/creds"
)

// Account is vault metadata for one stored credential.
type Account struct {
	ID              string    `json:"id"` // sanitized userId (directory name)
	UserID          string    `json:"userId"`
	UserName        string    `json:"userName"`
	KeyName         string    `json:"keyName"`
	AuthenticatedAt string    `json:"authenticatedAt"`
	AddedAt         string    `json:"addedAt"`
	MaskedKey       string    `json:"maskedKey"`
	Note            string    `json:"note,omitempty"`
	Plan            *PlanInfo `json:"plan,omitempty"`
}

// PlanInfo caches the per-account plan probe (which models its key can use).
type PlanInfo struct {
	ProbeAt      time.Time         `json:"probeAt"`
	Allowed      []string          `json:"allowed"`
	Denied       []string          `json:"denied"`
	QuotaHeaders map[string]string `json:"quotaHeaders,omitempty"`
	// Blocked = the account cannot make any request (no credits/purchased plan).
	Blocked bool   `json:"blocked,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// PlanTTL is how long a cached probe is considered fresh.
const PlanTTL = 6 * time.Hour

// Vault is a credential archive rooted at a directory.
type Vault struct {
	root string
}

// Open prepares the vault directory and returns the Vault.
func Open(root string) (*Vault, error) {
	if err := os.MkdirAll(filepath.Join(root, "accounts"), 0o700); err != nil {
		return nil, err
	}
	return &Vault{root: root}, nil
}

// Root is the vault's base directory.
func (v *Vault) Root() string { return v.root }

func (v *Vault) accountDir(id string) string {
	return filepath.Join(v.root, "accounts", creds.SafeID(id))
}

// SetNote stores a free-form remark for an account (e.g. which plan, phone
// number, billing cycle).
func (v *Vault) SetNote(id, note string) error {
	a, err := v.readMeta(id)
	if err != nil {
		return err
	}
	a.Note = note
	return v.writeMeta(id, a)
}

// Put stores (or refreshes) one credential set, keyed by its userId, and
// returns the metadata. The auth.json payload is stored verbatim.
func (v *Vault) Put(auth *creds.Auth, raw []byte) (*Account, error) {
	if auth == nil || len(raw) == 0 {
		return nil, fmt.Errorf("empty credential")
	}
	id := creds.SafeID(auth.UserID)
	dir := v.accountDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
		return nil, err
	}
	acct := &Account{
		ID:              id,
		UserID:          auth.UserID,
		UserName:        auth.UserName,
		KeyName:         auth.KeyName,
		AuthenticatedAt: auth.AuthenticatedAt,
		AddedAt:         time.Now().UTC().Format(time.RFC3339),
		MaskedKey:       creds.MaskKey(auth.APIKey),
	}
	// Preserve AddedAt on refresh.
	if old, err := v.readMeta(id); err == nil && old.AddedAt != "" {
		acct.AddedAt = old.AddedAt
		acct.Note = old.Note // notes survive credential refreshes
	}
	if err := v.writeMeta(id, acct); err != nil {
		return nil, err
	}
	return acct, nil
}

// List returns all stored accounts sorted by userName.
func (v *Vault) List() ([]Account, error) {
	ents, err := os.ReadDir(filepath.Join(v.root, "accounts"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Account
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		a, err := v.readMeta(e.Name())
		if err != nil {
			continue // unreadable entries are invisible but never break List
		}
		if a.Plan, err = v.readPlan(e.Name()); err != nil {
			a.Plan = nil
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserName != out[j].UserName {
			return out[i].UserName < out[j].UserName
		}
		return out[i].AddedAt < out[j].AddedAt
	})
	return out, nil
}

// Auth returns the stored raw auth.json bytes for one account.
func (v *Vault) Auth(id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(v.accountDir(id), "auth.json"))
}

// Account looks up stored metadata by id.
func (v *Vault) Account(id string) (*Account, error) {
	return v.readMeta(creds.SafeID(id))
}

// Remove deletes an account's directory.
func (v *Vault) Remove(id string) error {
	return os.RemoveAll(v.accountDir(id))
}

// ActiveID returns the last activated account id, or "" when unset/stale.
func (v *Vault) ActiveID() string {
	data, err := os.ReadFile(filepath.Join(v.root, "active.json"))
	if err != nil {
		return ""
	}
	var m struct {
		UserID string `json:"userId"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return creds.SafeID(m.UserID)
}

// SetActive records the activated account id.
func (v *Vault) SetActive(id string) error {
	b, _ := json.MarshalIndent(map[string]string{"userId": id}, "", "  ")
	return os.WriteFile(filepath.Join(v.root, "active.json"), b, 0o600)
}

// ClearActive forgets the activated marker (used when auth.json is deleted).
func (v *Vault) ClearActive() error {
	return os.Remove(filepath.Join(v.root, "active.json"))
}

// PutPlan caches a probe result for one account.
func (v *Vault) PutPlan(id string, plan *PlanInfo) error {
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(v.accountDir(id), "plan.json"), b, 0o600)
}

// IsPlanFresh reports whether the cached probe for id is still valid.
func (v *Vault) IsPlanFresh(id string) bool {
	plan, err := v.readPlan(id)
	return err == nil && time.Since(plan.ProbeAt) < PlanTTL
}

func (v *Vault) readMeta(id string) (*Account, error) {
	data, err := os.ReadFile(filepath.Join(v.accountDir(id), "meta.json"))
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (v *Vault) writeMeta(id string, a *Account) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(v.accountDir(id), "meta.json"), b, 0o600)
}

func (v *Vault) readPlan(id string) (*PlanInfo, error) {
	data, err := os.ReadFile(filepath.Join(v.accountDir(id), "plan.json"))
	if err != nil {
		return nil, err
	}
	var p PlanInfo
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
