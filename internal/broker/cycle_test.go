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
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func TestCyclePlanSpacesClusteredAccounts(t *testing.T) {
	day := int64(24 * 60 * 60 * 1000)
	members := []cycleMember{
		{id: "polina", createdAtMS: 1, state: "IN_CYCLE", weeklyResetS: sql.NullInt64{Int64: 19 * 60 * 60, Valid: true}},
		{id: "alejandro", createdAtMS: 2, state: "IN_CYCLE", weeklyResetS: sql.NullInt64{Int64: 4 * day / 1000, Valid: true}},
		{id: "arina", createdAtMS: 3, state: "IN_CYCLE", weeklyResetS: sql.NullInt64{Int64: 4 * day / 1000, Valid: true}},
		{id: "mina", createdAtMS: 4, state: "IN_CYCLE", weeklyResetS: sql.NullInt64{Int64: 4 * day / 1000, Valid: true}},
		{id: "timothy", createdAtMS: 5, state: "IN_CYCLE", weeklyResetS: sql.NullInt64{Int64: 6 * day / 1000, Valid: true}},
	}
	plan := buildCyclePlan(members, sql.NullInt64{Int64: -day, Valid: true}, 0)
	want := []int64{19 * 60 * 60 * 1000, 4 * day, 4*day + weeklyCycleMS/5, 4*day + 2*weeklyCycleMS/5, 4*day + 3*weeklyCycleMS/5}
	if len(plan) != len(want) {
		t.Fatalf("plan length = %d", len(plan))
	}
	for index := range want {
		if plan[index].releaseAtMS != want[index] || plan[index].position != index+1 || plan[index].size != 5 {
			t.Fatalf("plan[%d] = %#v, want release %d", index, plan[index], want[index])
		}
	}
}

func TestRouterEmergencyReleasesHeldAccount(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	instance, _ := database.InstanceID(ctx)
	secrets, _ := vault.New(make([]byte, 32), instance)
	settings := config.Config{RuntimeDir: filepath.Join(root, "run"), WindowPulseEnabled: true, WindowPulseConcurrency: 1, UsageRefreshConcurrency: 1, AuthConcurrency: 1, ProcessStartConcurrency: 1}
	service := NewService(database, settings, secrets, codex.NewRuntimeManager(settings, secrets), events.New(10, 1))
	now := time.Now().UnixMilli()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('account','public','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('account','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,weekly_used_percent_raw,weekly_duration_minutes,weekly_resets_at_s)
		VALUES('account',10,10080,1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	authHome := filepath.Join(root, "auth")
	if err := os.MkdirAll(authHome, 0o700); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": testJWT(time.Now().Add(time.Hour).Unix()), "refresh_token": "secret"}})
	if err := os.WriteFile(filepath.Join(authHome, "auth.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Capture(authHome, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.installBundle(ctx, "account", "ACTIVE", payload); err != nil {
		t.Fatal(err)
	}
	lease, wait, err := (Router{Store: database, Service: service}).Route(ctx, "key", RouteRequest{})
	if err != nil || wait != nil || lease == nil || lease.AccountID != "public" {
		t.Fatalf("route = %#v, %#v, %v", lease, wait, err)
	}
	var state string
	var started int64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started); err != nil {
		t.Fatal(err)
	}
	if state != "IN_CYCLE" || started < now {
		t.Fatalf("cycle state = %s at %d", state, started)
	}
}

func TestCycleReconciliationHoldsExpiredAccountAndEmergencyReleaseRestartsCadence(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('account','public','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('account','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,weekly_duration_minutes,weekly_resets_at_s)
		VALUES('account',10080,1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true, UsagePollSeconds: 300, WindowPulseRetrySeconds: 900}}
	now := int64(2_000)
	if err := service.reconcileCycle(ctx, now); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state FROM window_pulse_state WHERE account_id='account'").Scan(&state); err != nil || state != "HELD" {
		t.Fatalf("cycle state = %q, %v", state, err)
	}
	released, err := service.releaseHeld(ctx, "account", now)
	if err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	var started, last int64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.QueryRowContext(ctx, "SELECT last_release_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if state != "IN_CYCLE" || started != now || last != now {
		t.Fatalf("state=%s started=%d last=%d", state, started, last)
	}
}
