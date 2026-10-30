package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func TestCycleSnapshotOrdersActiveAccountsAndMeasuresGaps(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	spacing := weeklyCycleMS / 3
	for index, id := range []string{"a", "b", "c"} {
		started := int64(index) * spacing
		if _, err := database.DB.ExecContext(ctx, "INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'ACTIVE',?,1)", id, id, id, index+1); err != nil {
			t.Fatal(err)
		}
		if _, err := database.DB.ExecContext(ctx, "INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES(?,'VERIFIED','STOPPED','HEALTHY','FRESH',1,1)", id); err != nil {
			t.Fatal(err)
		}
		if _, err := database.DB.ExecContext(ctx, "INSERT INTO usage_current(account_id) VALUES(?)", id); err != nil {
			t.Fatal(err)
		}
		if _, err := database.DB.ExecContext(ctx, "INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES(?,0,'IN_CYCLE',?)", id, started); err != nil {
			t.Fatal(err)
		}
	}
	items, status, err := cycleSnapshot(ctx, database.DB)
	if err != nil || status.MemberCount != 3 || status.ActiveCount != 3 || status.SpacingMS != spacing {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	for index, item := range items {
		if item.position != index+1 || item.size != 3 || item.gapMS == nil || *item.gapMS != spacing {
			t.Fatalf("item[%d]=%+v", index, item)
		}
	}
}

func TestRouterActivatesHeldAccount(t *testing.T) {
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
		INSERT INTO client_api_keys(key_id,name,key_prefix,secret_hash,created_at_ms) VALUES('key','test','test',X'00',1);
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
	if err := service.replaceActive(ctx, "account", payload); err != nil {
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
	if err := service.reconcileCycle(ctx, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var state string
	var started, routed sql.NullInt64
	if err := database.DB.QueryRowContext(ctx, "SELECT p.cycle_state,p.weekly_started_at_ms,s.last_routed_at_ms FROM window_pulse_state p JOIN account_state s USING(account_id) WHERE account_id='account'").Scan(&state, &started, &routed); err != nil {
		t.Fatal(err)
	}
	if state != "IN_CYCLE" || !started.Valid || !routed.Valid {
		t.Fatalf("held account was not activated: state=%s started=%v routed=%v", state, started, routed)
	}
}

func TestCycleReconciliationHoldsExpiredAccount(t *testing.T) {
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

func TestCycleReconciliationConfirmsIdleAndDetectsExternalActivation(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().Unix() * 1000
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('account','public','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES('account','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);`)
	if err == nil {
		_, err = database.DB.ExecContext(ctx, "INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw,weekly_duration_minutes,weekly_resets_at_s,complete_read_at_ms) VALUES('account',0,0,10080,?,?)", (now+weeklyCycleMS)/1000, now)
	}
	if err == nil {
		_, err = database.DB.ExecContext(ctx, "INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES('account',0,'IN_CYCLE',?)", now-time.Hour.Milliseconds())
	}
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true}}
	if err := service.reconcileCycle(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileCycle(ctx, now+1000); err != nil {
		t.Fatal(err)
	}
	var state string
	var observations int
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,idle_observation_count FROM window_pulse_state WHERE account_id='account'").Scan(&state, &observations); err != nil || state != "IN_CYCLE" || observations != 1 {
		t.Fatalf("same observation: state=%s observations=%d err=%v", state, observations, err)
	}
	second := now + 5*time.Minute.Milliseconds()
	if _, err := database.DB.ExecContext(ctx, "UPDATE usage_current SET weekly_resets_at_s=?,complete_read_at_ms=? WHERE account_id='account'", (second+weeklyCycleMS)/1000, second); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileCycle(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,idle_observation_count FROM window_pulse_state WHERE account_id='account'").Scan(&state, &observations); err != nil || state != "HELD" || observations != 2 {
		t.Fatalf("confirmed idle: state=%s observations=%d err=%v", state, observations, err)
	}
	externalStart := second - 2*time.Minute.Milliseconds()
	if _, err := database.DB.ExecContext(ctx, "UPDATE usage_current SET short_used_percent_raw=1,weekly_resets_at_s=?,complete_read_at_ms=? WHERE account_id='account'", (externalStart+weeklyCycleMS)/1000, second+1000); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileCycle(ctx, second+1000); err != nil {
		t.Fatal(err)
	}
	var started, anchor int64
	if err := database.DB.QueryRowContext(ctx, "SELECT p.cycle_state,p.weekly_started_at_ms,c.cadence_anchor_at_ms FROM window_pulse_state p CROSS JOIN weekly_cycle_state c WHERE p.account_id='account'").Scan(&state, &started, &anchor); err != nil || state != "IN_CYCLE" || started != externalStart || anchor != externalStart {
		t.Fatalf("external activation: state=%s start=%d anchor=%d err=%v", state, started, anchor, err)
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
	`, (now+weeklyCycleMS)/1000)
	if err == nil {
		_, err = database.DB.ExecContext(ctx, "INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES('account',0,'IN_CYCLE',?)", now-weeklyCycleMS-1)
	}
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
	if state != "HELD" || !started.Valid || started.Int64 != now-weeklyCycleMS-1 {
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
	raw := map[string]any{"windows": []any{map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(300), "resetsAt": reset}, map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(weekMinutes), "resetsAt": reset}}}
	account := Account{ID: "account", PublicID: "public"}
	if err := service.commitUsage(ctx, account, "refresh", raw, time.Second, "Usage refreshed", nil, false, false); err != nil {
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
	if err := service.commitUsage(ctx, account, "pulse", raw, time.Second, "Usage windows kept active", &nextPulse, true, false); err != nil {
		t.Fatal(err)
	}
	var cadenceAnchor sql.NullInt64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms,next_pulse_at_ms FROM window_pulse_state WHERE account_id='account'").Scan(&state, &started, &next); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.QueryRowContext(ctx, "SELECT cadence_anchor_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&cadenceAnchor); err != nil {
		t.Fatal(err)
	}
	if state != "IN_CYCLE" || !started.Valid || next.Int64 != nextPulse || !cadenceAnchor.Valid || cadenceAnchor.Int64 != started.Int64 || started.Int64 != reset*1000-weeklyCycleMS {
		t.Fatalf("release state=%s started=%#v next=%#v anchor=%#v", state, started, next, cadenceAnchor)
	}
}

func TestCyclePulsesStaySpacedAcrossWeeks(t *testing.T) {
	for _, count := range []int{7, 8} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true, WindowPulseRetrySeconds: 900}}
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("account%d", i)
				for _, query := range []string{
					"INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'ACTIVE',1,1)",
					"INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES(?,'VERIFIED','STOPPED','HEALTHY','FRESH',1,1)",
					"INSERT INTO usage_current(account_id,last_attempt_at_ms) VALUES(?,0)",
					"INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,idle_observation_count) VALUES(?,0,'HELD',2)",
				} {
					args := []any{id}
					if strings.HasPrefix(query, "INSERT INTO accounts(") {
						args = []any{id, id, id}
					}
					if _, err := database.DB.ExecContext(ctx, query, args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			interval := weeklyCycleMS / int64(count)
			now := weeklyCycleMS * 10
			seen := map[string]bool{}
			for turn := 0; turn < count; turn++ {
				if err := service.reconcileCycle(ctx, now); err != nil {
					t.Fatal(err)
				}
				public, operation, err := service.claimCyclePulse(ctx, now)
				if err != nil || public == "" || operation == "" || seen[public] {
					t.Fatalf("turn %d: public=%s operation=%s err=%v", turn, public, operation, err)
				}
				seen[public] = true
				// Even after a long runtime, no second account may start concurrently.
				if _, next, err := service.claimCyclePulse(ctx, now+interval); err != nil || next != "" {
					t.Fatalf("concurrent pulse=%s err=%v", next, err)
				}
				for _, statement := range []string{
					"UPDATE operations SET state='SUCCEEDED' WHERE account_id=?",
					"UPDATE window_pulse_state SET cycle_state='IN_CYCLE',weekly_started_at_ms=?,last_success_at_ms=?,next_pulse_at_ms=? WHERE account_id=?",
					"UPDATE weekly_cycle_state SET cadence_anchor_at_ms=? WHERE singleton_id=1",
				} {
					args := []any{public}
					if strings.HasPrefix(statement, "UPDATE window_pulse_state") {
						args = []any{now, now, now + 1000, public}
					}
					if strings.HasPrefix(statement, "UPDATE weekly_cycle_state") {
						args = []any{now}
					}
					if _, err := database.DB.ExecContext(ctx, statement, args...); err != nil {
						t.Fatal(err)
					}
				}
				// An expired short window must not trigger an extra pulse.
				if _, early, err := service.claimCyclePulse(ctx, now+interval-1); err != nil || early != "" {
					t.Fatalf("early pulse=%s err=%v", early, err)
				}
				now += interval
			}
		})
	}
}

func TestCycleClaimsAreAtomic(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','a','a',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES('a','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id) VALUES('a');`)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true, WindowPulseRetrySeconds: 900}}
	now := weeklyCycleMS * 10
	if err := service.reconcileCycle(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, "UPDATE window_pulse_state SET idle_observation_count=2"); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, _, err := service.claimCyclePulse(ctx, now); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	var claims int
	if err := database.DB.QueryRowContext(ctx, "SELECT count(*) FROM operations").Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims=%d err=%v", claims, err)
	}
	if _, err := database.DB.ExecContext(ctx, "UPDATE operations SET state='FAILED'; UPDATE window_pulse_state SET cycle_state='HELD';"); err != nil {
		t.Fatal(err)
	}
	if _, operation, err := service.claimCyclePulse(ctx, now+899000); err != nil || operation != "" {
		t.Fatalf("retry too early: %s %v", operation, err)
	}
	if _, operation, err := service.claimCyclePulse(ctx, now+900000); err != nil || operation == "" {
		t.Fatalf("retry missing: %s %v", operation, err)
	}
}

func TestCycleDoesNotClaimRelayReserve(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, id := range []string{"reserved", "available"} {
		if _, err := database.DB.ExecContext(ctx, `
			INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'ACTIVE',1,1);
			INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES(?,'VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
			INSERT INTO usage_current(account_id) VALUES(?);
			INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,idle_observation_count) VALUES(?,0,'HELD',2);`, id, id, id, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	now := weeklyCycleMS * 10
	if _, err := database.DB.ExecContext(ctx, "INSERT INTO relay_reservations VALUES('relay','reserved','permit',?,?)", now+time.Hour.Milliseconds(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, "UPDATE weekly_cycle_state SET cadence_anchor_at_ms=?", now-cycleInterval(2)); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true, WindowPulseRetrySeconds: 900}}
	public, operation, err := service.claimCyclePulse(ctx, now)
	if err != nil || public != "available" || operation == "" {
		t.Fatalf("public=%q operation=%q err=%v", public, operation, err)
	}
}

func TestCycleReconciliationUsesUpstreamExpiry(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','a','a',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES('a','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,weekly_resets_at_s) VALUES('a',604801);
		INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES('a',0,'IN_CYCLE',1000);`)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database, Config: config.Config{WindowPulseEnabled: true}}
	if err := service.reconcileCycle(ctx, weeklyCycleMS+1000); err != nil {
		t.Fatal(err)
	}
	var state string
	var started int64
	if err := database.DB.QueryRowContext(ctx, "SELECT cycle_state,weekly_started_at_ms FROM window_pulse_state WHERE account_id='a'").Scan(&state, &started); err != nil || state != "HELD" || started != 1000 {
		t.Fatalf("state=%s started=%d err=%v", state, started, err)
	}
}

func TestUsageCycleEvidenceDistinguishesRollingAndFixedWindows(t *testing.T) {
	now := weeklyCycleMS * 10
	rolling := cycleMember{shortUsed: sql.NullInt64{Int64: 0, Valid: true}, weeklyUsed: sql.NullInt64{Int64: 0, Valid: true}, weeklyResetS: sql.NullInt64{Int64: (now + weeklyCycleMS) / 1000, Valid: true}, weeklyDurationMinutes: sql.NullInt64{Int64: weekMinutes, Valid: true}, usageReadAtMS: sql.NullInt64{Int64: now, Valid: true}}
	if _, active, idle := usageCycleEvidence(rolling, now); active || !idle {
		t.Fatalf("rolling evidence: active=%v rolling=%v", active, idle)
	}
	fixed := rolling
	fixed.weeklyResetS.Int64 -= 3600
	if started, active, idle := usageCycleEvidence(fixed, now); !active || idle || started != now-3600*1000 {
		t.Fatalf("fixed evidence: start=%d active=%v rolling=%v", started, active, idle)
	}
	rolling.shortUsed.Int64 = 1
	if _, active, idle := usageCycleEvidence(rolling, now); !active || idle {
		t.Fatalf("used evidence: active=%v rolling=%v", active, idle)
	}
}
