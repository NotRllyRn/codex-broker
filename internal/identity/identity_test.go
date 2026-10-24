package identity

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

func TestEnrollmentAndLedger(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, code, err := Create(ctx, db, "Tim")
	if err != nil {
		t.Fatal(err)
	}
	b, other, err := Create(ctx, db, "Other")
	if err != nil {
		t.Fatal(err)
	}
	if code == other {
		t.Fatal("codes must be unique")
	}
	if _, _, err := Create(ctx, db, "tim"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	token := "cbu_" + core.RandomToken(32)
	install := "0123456789abcdef0123456789abcdef"
	if _, err := Enroll(ctx, db, "Tim", install, "Laptop", token); err == nil {
		t.Fatal("name accepted as secret")
	}
	d, err := Enroll(ctx, db, code, install, "Laptop", token)
	if err != nil || d.IdentityID != a.ID {
		t.Fatalf("device=%+v err=%v", d, err)
	}
	retry, err := Enroll(ctx, db, code, install, "Laptop", token)
	if err != nil || retry.ID != d.ID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	if _, err := Enroll(ctx, db, code, install, "Stolen", "cbu_"+core.RandomToken(32)); err == nil {
		t.Fatal("enrollment replaced device")
	}
	if _, err := Authenticate(ctx, db.DB, code); err == nil {
		t.Fatal("identity code authorized uploads")
	}
	e := Event{ID: EventID("original-event"), Source: "codex", TimestampMS: time.Now().UnixMilli(), Model: "gpt-test", InputTokens: 100, CachedInputTokens: 80, OutputTokens: 20, ReasoningTokens: 5, UsageKnown: true}
	if n, err := Ingest(ctx, db, token, []Event{e, e}); err != nil || n != 1 {
		t.Fatalf("ingest=%d err=%v", n, err)
	}
	secondToken := "cbu_" + core.RandomToken(32)
	if _, err := Enroll(ctx, db, code, "1123456789abcdef0123456789abcdef", "Desktop", secondToken); err != nil {
		t.Fatal(err)
	}
	if n, err := Ingest(ctx, db, secondToken, []Event{e}); err != nil || n != 0 {
		t.Fatalf("cross-device copy=%d err=%v", n, err)
	}
	thirdToken := "cbu_" + core.RandomToken(32)
	if _, err := Enroll(ctx, db, other, install, "Other laptop", thirdToken); err != nil {
		t.Fatal(err)
	}
	if n, err := Ingest(ctx, db, thirdToken, []Event{e}); err != nil || n != 1 {
		t.Fatalf("other identity=%d err=%v", n, err)
	}
	fresh := e
	fresh.ID = EventID("new-event")
	conflict := e
	conflict.OutputTokens++
	if _, err := Ingest(ctx, db, token, []Event{fresh, conflict}); err == nil {
		t.Fatal("conflicting batch accepted")
	}
	var count int
	if err := db.DB.QueryRow("SELECT count(*) FROM identity_usage WHERE identity_id=?", a.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	newCode, err := Rotate(ctx, db, a.ID)
	if err != nil || newCode == code {
		t.Fatal("rotation failed", err)
	}
	if _, err := Enroll(ctx, db, code, "2123456789abcdef0123456789abcdef", "New", "cbu_"+core.RandomToken(32)); err == nil {
		t.Fatal("old code accepted")
	}
	if _, err := Ingest(ctx, db, token, []Event{}); err != nil {
		t.Fatal("rotation disconnected enrolled device", err)
	}
	if _, err := db.DB.Exec("UPDATE usage_devices SET revoked_at_ms=1 WHERE device_id=?", d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(ctx, db, token, []Event{fresh}); err == nil {
		t.Fatal("revoked device uploaded")
	}
	if _, err := db.DB.Exec("DELETE FROM usage_identities WHERE identity_id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT count(*) FROM identity_usage WHERE identity_id=?", b.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("deletion affected other identity", err)
	}
}

func TestEventValidation(t *testing.T) {
	now := time.Now()
	valid := Event{ID: EventID("valid"), Source: "pi", TimestampMS: now.UnixMilli(), Model: "model", InputTokens: 100, OutputTokens: 10, UsageKnown: true}
	for _, change := range []func(*Event){func(e *Event) { e.InputTokens = -1 }, func(e *Event) { e.CachedInputTokens = 101 }, func(e *Event) { e.CacheWriteTokens = 101 }, func(e *Event) { e.ReasoningTokens = 11 }, func(e *Event) { e.Source = "proxy" }, func(e *Event) { e.TimestampMS = now.Add(48 * time.Hour).UnixMilli() }, func(e *Event) { e.ID = "invalid" }, func(e *Event) { e.UsageKnown = false }} {
		e := valid
		change(&e)
		if err := e.Validate(now); err == nil {
			t.Fatalf("invalid event accepted: %+v", e)
		}
	}
}
