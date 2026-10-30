package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func quotaFixture(t *testing.T) (Router, Account) {
	t.Helper()
	ctx, root := context.Background(), t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	instance, _ := db.InstanceID(ctx)
	secrets, _ := vault.New(make([]byte, 32), instance)
	settings := config.Config{UsagePollSeconds: 3600, UsageRefreshConcurrency: 1, ProcessStartConcurrency: 1, AuthConcurrency: 1, WindowPulseConcurrency: 1}
	service := NewService(db, settings, secrets, codex.NewRuntimeManager(settings, secrets), events.New(10, 1))
	t.Cleanup(service.Close)
	now := time.Now()
	_, err = db.DB.Exec(`
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','public','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES('a','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,short_used_percent_raw,short_resets_at_s,weekly_used_percent_raw,weekly_resets_at_s) VALUES('a',80,?,40,?);
		INSERT INTO client_api_keys(key_id,name,key_prefix,secret_hash,created_at_ms) VALUES('one','one','one',X'01',1),('two','two','two',X'02',1);
	`, now.Add(2*time.Hour).Unix(), now.Add(3*24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "auth")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": testJWT(now.Add(time.Hour).Unix()), "refresh_token": "test"}})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Capture(home, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.replaceActive(ctx, "a", payload); err != nil {
		t.Fatal(err)
	}
	router := Router{Store: db, Service: service}
	accounts, err := router.accounts(ctx, "one", now.UnixMilli())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	return router, accounts[0]
}

func TestQuotaRejectionBlocksAllClientsWithoutUsageRead(t *testing.T) {
	router, account := quotaFixture(t)
	ctx := context.Background()
	lease, wait, err := router.Route(ctx, "one", RouteRequest{FailedAccountID: account.PublicID, FailureKind: "quota"})
	if err != nil || lease != nil || wait == nil {
		t.Fatalf("lease=%+v wait=%+v err=%v", lease, wait, err)
	}
	for _, key := range []string{"one", "two"} {
		lease, wait, err = router.Route(ctx, key, RouteRequest{})
		if err != nil || lease != nil || wait == nil {
			t.Fatalf("key=%s lease=%+v wait=%+v err=%v", key, lease, wait, err)
		}
	}
	var retry, reads int64
	if err := router.Store.DB.QueryRow("SELECT retry_at_ms FROM account_quota_blocks").Scan(&retry); err != nil {
		t.Fatal(err)
	}
	if retry != account.ShortReset.Int64*1000 {
		t.Fatalf("retry=%d want first reset=%d", retry, account.ShortReset.Int64*1000)
	}
	if err := router.Store.DB.QueryRow("SELECT count(*) FROM operations WHERE kind='usage.refresh'").Scan(&reads); err != nil || reads != 0 {
		t.Fatalf("reads=%d err=%v", reads, err)
	}
	views, err := router.Service.Accounts(ctx)
	if err != nil || len(views) != 1 || !views[0].QuotaBlocked || *views[0].ShortPercent != 80 || *views[0].WeeklyPercent != 40 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	if views[0].OverallState != "HEALTHY" {
		t.Fatalf("quota exhaustion requires attention: %s", views[0].OverallState)
	}
	var short, weekly int
	if err := router.Store.DB.QueryRow("SELECT short_used_percent_raw,weekly_used_percent_raw FROM usage_current").Scan(&short, &weekly); err != nil || short != 80 || weekly != 40 {
		t.Fatalf("cached usage=%d/%d err=%v", short, weekly, err)
	}
}

func TestQuotaBlockPreservesUnrelatedAccountStatus(t *testing.T) {
	router, account := quotaFixture(t)
	ctx := context.Background()
	if err := router.blockQuota(ctx, account, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"WARNING", "ACTION_REQUIRED", "ERROR", "DISABLED"} {
		t.Run(state, func(t *testing.T) {
			if _, err := router.Store.DB.Exec("UPDATE account_state SET overall_state=? WHERE account_id=?", state, account.ID); err != nil {
				t.Fatal(err)
			}
			views, err := router.Service.Accounts(ctx)
			if err != nil || len(views) != 1 || views[0].OverallState != state || !views[0].QuotaBlocked {
				t.Fatalf("views=%+v err=%v", views, err)
			}
		})
	}
}

func TestQuotaBlockWaitsForFreshReadAfterEitherReset(t *testing.T) {
	for _, first := range []string{"short", "weekly"} {
		t.Run(first, func(t *testing.T) {
			router, account := quotaFixture(t)
			now := time.Now().UnixMilli()
			soon, later := sql.NullInt64{Int64: now/1000 + 60, Valid: true}, sql.NullInt64{Int64: now/1000 + 600, Valid: true}
			account.ShortReset, account.WeeklyReset = soon, later
			if first == "weekly" {
				account.ShortReset, account.WeeklyReset = later, soon
			}
			if err := router.blockQuota(context.Background(), account, now); err != nil {
				t.Fatal(err)
			}
			accounts, err := router.accounts(context.Background(), "two", now)
			if err != nil || !exhausted(accounts[0], now) || !exhausted(accounts[0], soon.Int64*1000) {
				t.Fatalf("accounts=%+v err=%v", accounts, err)
			}
		})
	}
}

func TestFreshUsageReadUnlocksButInflightReadDoesNot(t *testing.T) {
	ctx := context.Background()
	router, account := quotaFixture(t)
	now := time.Now().UnixMilli()
	if err := router.blockQuota(ctx, account, now); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(80), "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": account.WeeklyReset.Int64}}
	if err := router.Service.commitUsage(ctx, account, "inflight", raw, time.Hour, "test", nil, false, false); err != nil {
		t.Fatal(err)
	}
	var blocks int
	if err := router.Store.DB.QueryRow("SELECT count(*) FROM account_quota_blocks").Scan(&blocks); err != nil || blocks != 1 {
		t.Fatalf("inflight cleared block: %d err=%v", blocks, err)
	}
	if _, err := router.Store.DB.Exec("UPDATE account_quota_blocks SET blocked_at_ms=blocked_at_ms-45001"); err != nil {
		t.Fatal(err)
	}
	if err := router.Service.commitUsage(ctx, account, "fresh", raw, 0, "test", nil, false, false); err != nil {
		t.Fatal(err)
	}
	lease, wait, err := router.Route(ctx, "two", RouteRequest{})
	if err != nil || lease == nil || wait != nil {
		t.Fatalf("lease=%+v wait=%+v err=%v", lease, wait, err)
	}
}

func TestUsageCommitMeasuresActualPollDeltaBeforeReplacingBaseline(t *testing.T) {
	router, account := quotaFixture(t)
	ctx, now := context.Background(), time.Now()
	start := now.Add(-time.Hour).UnixMilli()
	if _, err := router.Store.DB.Exec("UPDATE usage_current SET complete_read_at_ms=?", start); err != nil {
		t.Fatal(err)
	}
	if err := router.blockQuota(ctx, account, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_, err := proxytelemetry.Record(ctx, router.Store, gateway.Observation{RequestID: "response", AccountID: account.PublicID, Endpoint: "/v1/responses", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamHTTPStatus: 200, UpstreamStartedAtMS: now.Add(-30 * time.Minute).UnixMilli(), TerminalEventAtMS: now.Add(-10 * time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(85), "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}, "secondary": map[string]any{"usedPercent": int64(43), "windowDurationMins": int64(10080), "resetsAt": account.WeeklyReset.Int64}}
	if err := router.Service.commitUsage(ctx, account, "poll", raw, 0, "test", nil, false, false); err != nil {
		t.Fatal(err)
	}
	rows, err := proxytelemetry.ReadQuotaTotals(ctx, router.Store.DB, 0)
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		want := int64(5)
		if row.Window == "weekly" {
			want = 3
		}
		if row.UsedPercent != want || row.ActiveMS != 20*time.Minute.Milliseconds() {
			t.Fatalf("row=%+v", row)
		}
	}
}

func TestPulseConsumptionCannotBeAttributedToClientModels(t *testing.T) {
	router, account := quotaFixture(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := router.Store.DB.Exec(`UPDATE usage_current SET complete_read_at_ms=?`, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := proxytelemetry.Record(ctx, router.Store, gateway.Observation{RequestID: "concurrent", AccountID: account.PublicID, ResponseModel: "example-model", Endpoint: "/v1/responses", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamStartedAtMS: now.Add(-30 * time.Second).UnixMilli(), TerminalEventAtMS: now.Add(-10 * time.Second).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(85), "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}, "secondary": map[string]any{"usedPercent": int64(43), "windowDurationMins": int64(10080), "resetsAt": account.WeeklyReset.Int64}}
	next := account.WeeklyReset.Int64 * 1000
	if err := router.Service.commitUsage(ctx, account, "pulse", raw, 0, "test", &next, true, false); err != nil {
		t.Fatal(err)
	}
	totals, err := proxytelemetry.ReadQuotaTotals(ctx, router.Store.DB, 0)
	if err != nil || len(totals) != 0 {
		t.Fatalf("pulse consumption attributed to client: %+v %v", totals, err)
	}
	var used, read int64
	if err := router.Store.DB.QueryRow(`SELECT short_used_percent_raw,complete_read_at_ms FROM usage_current WHERE account_id='a'`).Scan(&used, &read); err != nil || used != 85 || read < now.UnixMilli() {
		t.Fatalf("pulse baseline missing: %d %d %v", used, read, err)
	}
}

func TestUsageObservationTimeExcludesCheckpointDelay(t *testing.T) {
	router, account := quotaFixture(t)
	ctx := context.Background()
	now := time.Now()
	observed := now.Add(-10 * time.Second).UnixMilli()
	if _, err := router.Store.DB.Exec(`UPDATE usage_current SET complete_read_at_ms=?`, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, response := range []struct {
		id       string
		from, to int64
	}{
		{"before", observed - 5000, observed - 1000}, {"checkpoint", observed + 1000, observed + 5000},
	} {
		if _, err := proxytelemetry.Record(ctx, router.Store, gateway.Observation{RequestID: response.id, AccountID: account.PublicID, ResponseModel: "example-model", Endpoint: "/v1/responses", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamStartedAtMS: response.from, TerminalEventAtMS: response.to}); err != nil {
			t.Fatal(err)
		}
	}
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(85), "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}, "secondary": map[string]any{"usedPercent": int64(43), "windowDurationMins": int64(10080), "resetsAt": account.WeeklyReset.Int64}}
	if err := router.Service.commitUsageAt(ctx, account, "read", raw, time.Second, "test", nil, false, false, observed); err != nil {
		t.Fatal(err)
	}
	totals, err := proxytelemetry.ReadQuotaTotals(ctx, router.Store.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) == 0 {
		t.Fatal("missing measurements")
	}
	for _, row := range totals {
		if row.ActiveMS != 4000 {
			t.Fatalf("checkpoint activity included: %+v", row)
		}
	}
	var read int64
	if err := router.Store.DB.QueryRow(`SELECT complete_read_at_ms FROM usage_current WHERE account_id='a'`).Scan(&read); err != nil || read != observed {
		t.Fatalf("read=%d want=%d err=%v", read, observed, err)
	}
}
