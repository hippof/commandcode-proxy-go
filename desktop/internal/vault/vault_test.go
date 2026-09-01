package vault

import (
	"encoding/json"
	"testing"
	"time"

	"commandcode-desktop/internal/creds"
)

func authData(t *testing.T, userID, userName string) (*creds.Auth, []byte) {
	t.Helper()
	a := &creds.Auth{
		APIKey:          "user_secretkey_1234567890",
		UserID:          userID,
		UserName:        userName,
		KeyName:         "cli-test",
		AuthenticatedAt: "2026-01-01T00:00:00.000Z",
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return a, raw
}

func TestPutListActivate(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	a1, raw1 := authData(t, "u-one", "alice")
	a2, raw2 := authData(t, "u-two", "bob")
	if _, err := v.Put(a1, raw1); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Put(a2, raw2); err != nil {
		t.Fatal(err)
	}

	got, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserName != "alice" {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got[0].MaskedKey == "user_secretkey_1234567890" || got[0].MaskedKey == "" {
		t.Fatalf("key not masked: %q", got[0].MaskedKey)
	}

	// Re-Put same user refreshes without duplicating, keeps AddedAt.
	before, _ := v.Account("u-one")
	time.Sleep(2 * time.Millisecond)
	if _, err := v.Put(a1, raw1); err != nil {
		t.Fatal(err)
	}
	after, _ := v.Account("u-one")
	if before.AddedAt != after.AddedAt {
		t.Fatalf("AddedAt changed on refresh: %q -> %q", before.AddedAt, after.AddedAt)
	}
	got, _ = v.List()
	if len(got) != 2 {
		t.Fatalf("refresh duplicated account: %d", len(got))
	}

	// Stored auth.json must be verbatim.
	stored, err := v.Auth("u-one")
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(raw1) {
		t.Fatalf("stored auth.json altered")
	}

	// Active marker round-trips.
	if v.ActiveID() != "" {
		t.Fatalf("active should start empty")
	}
	if err := v.SetActive("u-one"); err != nil {
		t.Fatal(err)
	}
	if v.ActiveID() != "u-one" {
		t.Fatalf("active id = %q, want u-one", v.ActiveID())
	}

	// Remove.
	if err := v.Remove("u-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Account("u-one"); err == nil {
		t.Fatalf("account still readable after Remove")
	}
	got, _ = v.List()
	if len(got) != 1 {
		t.Fatalf("list after remove: %d", len(got))
	}
}

func TestNoteRoundTrip(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, raw := authData(t, "u-one", "alice")
	if _, err := v.Put(a, raw); err != nil {
		t.Fatal(err)
	}
	if err := v.SetNote("u-one", "包年 Pro 套餐"); err != nil {
		t.Fatal(err)
	}
	got, _ := v.Account("u-one")
	if got.Note != "包年 Pro 套餐" {
		t.Fatalf("note = %q", got.Note)
	}
	// Refreshing the credential must keep the note.
	if _, err := v.Put(a, raw); err != nil {
		t.Fatal(err)
	}
	got, _ = v.Account("u-one")
	if got.Note != "包年 Pro 套餐" {
		t.Fatalf("note lost on refresh: %q", got.Note)
	}
}

func TestUnsafeUserIDSandboxed(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, raw := authData(t, "../../escape", "eve")
	if _, err := v.Put(a, raw); err != nil {
		t.Fatal(err)
	}
	got, _ := v.List()
	if len(got) != 1 || got[0].ID == "" || got[0].ID == "." {
		t.Fatalf("escape not neutralized: %+v", got)
	}
}

func TestPlanCache(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, raw := authData(t, "u-one", "alice")
	if _, err := v.Put(a, raw); err != nil {
		t.Fatal(err)
	}
	if v.IsPlanFresh("u-one") {
		t.Fatal("fresh without cache")
	}
	plan := &PlanInfo{ProbeAt: time.Now(), Allowed: []string{"m1"}}
	if err := v.PutPlan("u-one", plan); err != nil {
		t.Fatal(err)
	}
	if !v.IsPlanFresh("u-one") {
		t.Fatal("plan should be fresh")
	}
	list, _ := v.List()
	if list[0].Plan == nil || len(list[0].Plan.Allowed) != 1 {
		t.Fatalf("plan not attached to list: %+v", list[0])
	}
}
