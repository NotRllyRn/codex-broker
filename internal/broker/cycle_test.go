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
		VALUES('account',0,10080,?);
	`, (now+weeklyCycleMS)/1000)
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
	router := Router{Store: database, Service: service}
	accounts, err := router.accounts(ctx, "key", now)
	if err != nil || len(accounts) != 1 || accounts[0].CycleState != "HELD" {
		t.Fatalf("initial cycle state = %#v, %v", accounts, err)
	}
	lease, wait, err := router.Route(ctx, "key", RouteRequest{})
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
	var routed int64
	if err := database.DB.QueryRowContext(ctx, "SELECT last_routed_at_ms FROM account_state WHERE account_id='account'").Scan(&routed); err != nil || routed < now {
		t.Fatalf("last routed = %d, %v", routed, err)
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

func TestCycleReconciliationDoesNotReactivateHeldAccountFromPassiveUsage(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('account','public','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('account','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,weekly_used_percent_raw,weekly_duration_minutes,weekly_resets_at_s)
		VALUES('account',0,10080,?);
		INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state)
		VALUES('account',0,'HELD');
	`, (now+weeklyCycleMS)/1000)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true}}
	if err := service.reconcileCycle(ctx, now); err != nil {
		t.Fatal(err)
	}
	var state string
	var started sql.NullInt64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started); err != nil {
		t.Fatal(err)
	}
	if state != "HELD" || started.Valid {
		t.Fatalf("cycle state = %s, started = %#v", state, started)
	}
}

func TestCycleReconciliationHoldsExpiredCycleDespiteRolledUsageWindow(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('account','public','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('account','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,weekly_used_percent_raw,weekly_duration_minutes,weekly_resets_at_s)
		VALUES('account',0,10080,?);
		INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms)
		VALUES('account',0,'IN_CYCLE',?);
	`, (now+weeklyCycleMS)/1000, now-weeklyCycleMS-1)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true}}
	if err := service.reconcileCycle(ctx, now); err != nil {
		t.Fatal(err)
	}
	var state string
	var started sql.NullInt64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started); err != nil {
		t.Fatal(err)
	}
	if state != "HELD" || started.Valid {
		t.Fatalf("cycle state = %s, started = %#v", state, started)
	}
}

func TestUsageCommitPreservesHoldAndCompletesCycleRelease(t *testing.T) {
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
		INSERT INTO usage_current(account_id,last_attempt_at_ms) VALUES('account',0);
		INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state) VALUES('account',0,'HELD');
		INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version)
		VALUES('refresh','account','usage.refresh','SCHEDULED','RUNNING',1,1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Events: events.New(10, 1)}
	reset := time.Now().Add(7 * 24 * time.Hour).Unix()
	raw := map[string]any{"windows": []any{map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(weekMinutes), "resetsAt": reset}}}
	account := Account{ID: "account", PublicID: "public"}
	if err := service.commitUsage(ctx, account, "refresh", raw, time.Second, "Usage refreshed", nil, false); err != nil {
		t.Fatal(err)
	}
	var state string
	var started, next sql.NullInt64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms,next_pulse_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started, &next); err != nil {
		t.Fatal(err)
	}
	if state != "HELD" || started.Valid || next.Valid {
		t.Fatalf("passive refresh changed hold: state=%s started=%#v next=%#v", state, started, next)
	}
	if _, err := database.DB.ExecContext(ctx, `
		UPDATE window_pulse_state SET cycle_state='RELEASING' WHERE account_id='account';
		INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version)
		VALUES('pulse','account','window.pulse','CYCLE_RELEASE','RUNNING',2,1);
	`); err != nil {
		t.Fatal(err)
	}
	nextPulse := time.Now().Add(3 * time.Hour).UnixMilli()
	if err := service.commitUsage(ctx, account, "pulse", raw, time.Second, "Usage windows kept active", &nextPulse, true); err != nil {
		t.Fatal(err)
	}
	var lastRelease sql.NullInt64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms,next_pulse_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started, &next); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.QueryRowContext(ctx, "SELECT last_release_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&lastRelease); err != nil {
		t.Fatal(err)
	}
	if state != "IN_CYCLE" || !started.Valid || next.Int64 != nextPulse || !lastRelease.Valid || lastRelease.Int64 != started.Int64 {
		t.Fatalf("release state=%s started=%#v next=%#v last=%#v", state, started, next, lastRelease)
	}
}
