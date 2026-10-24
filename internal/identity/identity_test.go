package identity

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/usagereport"
)

func report(output int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"period":"2026-09-01","agent":"all","inputTokens":10,"outputTokens":%d,"cacheReadTokens":20,"cacheCreationTokens":3,"totalTokens":%d,"totalCost":0.5,"agents":[{"agent":"any-future-agent","inputTokens":10,"outputTokens":%d}],"futureField":{"preserved":true}}`, output, 33+output, output))
}
func TestEnrollmentAndReports(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, code, err := Create(ctx, db, "Tim")
	if err != nil {
		t.Fatal(err)
	}
	other, otherCode, err := Create(ctx, db, "Other")
	if err != nil || otherCode == code {
		t.Fatal(err)
	}
	if _, _, err := Create(ctx, db, "tim"); err == nil {
		t.Fatal("duplicate name")
	}
	token := "cbu_" + core.RandomToken(32)
	installation := "0123456789abcdef0123456789abcdef"
	if _, err := Enroll(ctx, db, "Tim", installation, "Laptop", token); err == nil {
		t.Fatal("name authorized enrollment")
	}
	device, err := Enroll(ctx, db, code, installation, "Laptop", token)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := Enroll(ctx, db, code, installation, "Laptop", token)
	if err != nil || retry.ID != device.ID {
		t.Fatal("enrollment retry", err)
	}
	if _, err := Enroll(ctx, db, code, installation, "Stolen", "cbu_"+core.RandomToken(32)); err == nil {
		t.Fatal("credential replaced")
	}
	if _, err := Authenticate(ctx, db.DB, code); err == nil {
		t.Fatal("code authorized upload")
	}
	batch := ReportBatch{Version: 2, CCUsageVersion: usagereport.Version, Snapshots: []Snapshot{{Revision: 1, Report: report(5)}}}
	ingest := func(want int) {
		t.Helper()
		n, err := IngestReports(ctx, db, token, batch)
		if err != nil || n != want {
			t.Fatalf("updated=%d want=%d err=%v", n, want, err)
		}
	}
	ingest(1)
	ingest(0)
	batch.Snapshots[0] = Snapshot{Revision: 2, Report: report(8)}
	ingest(1)
	batch.Snapshots[0] = Snapshot{Revision: 1, Report: report(5)}
	if _, err := IngestReports(ctx, db, token, batch); err == nil {
		t.Fatal("stale client state accepted")
	}
	batch.Snapshots[0] = Snapshot{Revision: 2, Report: report(9)}
	if _, err := IngestReports(ctx, db, token, batch); err == nil {
		t.Fatal("revision conflict accepted")
	}
	batch.Snapshots[0] = Snapshot{Revision: 3, Report: report(4)}
	if _, err := IngestReports(ctx, db, token, batch); err == nil {
		t.Fatal("decrease accepted")
	}
	batch.Snapshots[0].AllowDecrease = true
	ingest(1)
	// Whole-batch atomicity, including repeated days in one batch.
	batch.Snapshots = []Snapshot{{Revision: 4, Report: report(20)}, {Revision: 4, Report: report(21)}}
	if _, err := IngestReports(ctx, db, token, batch); err == nil {
		t.Fatal("conflict did not roll back")
	}
	var revision, count int
	var payload string
	if err := db.DB.QueryRow("SELECT revision,payload FROM usage_reports WHERE device_id=?", device.ID).Scan(&revision, &payload); err != nil || revision != 3 {
		t.Fatal(revision, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded["futureField"] == nil {
		t.Fatal("unknown ccusage data lost")
	}
	second := "cbu_" + core.RandomToken(32)
	if _, err := Enroll(ctx, db, code, "1123456789abcdef0123456789abcdef", "Desktop", second); err != nil {
		t.Fatal(err)
	}
	batch.Snapshots = []Snapshot{{Revision: 1, Report: report(5)}}
	if _, err := IngestReports(ctx, db, second, batch); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT count(*) FROM usage_reports").Scan(&count); err != nil || count != 2 {
		t.Fatal("devices did not stay separate", err)
	}
	third := "cbu_" + core.RandomToken(32)
	if _, err := Enroll(ctx, db, otherCode, installation, "Other", third); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestReports(ctx, db, third, batch); err != nil {
		t.Fatal(err)
	}
	rotated, err := Rotate(ctx, db, owner.ID)
	if err != nil || rotated == code {
		t.Fatal(err)
	}
	if _, err := Enroll(ctx, db, code, "2123456789abcdef0123456789abcdef", "New", "cbu_"+core.RandomToken(32)); err == nil {
		t.Fatal("old code accepted")
	}
	if _, err := IngestReports(ctx, db, token, ReportBatch{Version: 2, CCUsageVersion: usagereport.Version}); err != nil {
		t.Fatal("rotation disconnected device", err)
	}
	if _, err := db.DB.Exec("UPDATE usage_devices SET revoked_at_ms=1 WHERE device_id=?", device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestReports(ctx, db, token, batch); err == nil {
		t.Fatal("revoked upload accepted")
	}
	if _, err := db.DB.Exec("DELETE FROM usage_identities WHERE identity_id=?", owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT count(*) FROM usage_reports r JOIN usage_devices d USING(device_id) WHERE d.identity_id=?", other.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("identity isolation", err)
	}
}
