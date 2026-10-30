package broker

import (
	"context"
	"math"
	"testing"
	"time"
)

func usageEvidence(account Account, short, weekly int64) map[string]any {
	return map[string]any{"primary": map[string]any{"usedPercent": short, "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}, "secondary": map[string]any{"usedPercent": weekly, "windowDurationMins": int64(10080), "resetsAt": account.WeeklyReset.Int64}}
}

func TestInvalidUsageCannotEraseExhaustionOrBaseline(t *testing.T) {
	for _, kind := range []string{"missing weekly", "missing percent", "negative", "over 100", "negative fraction", "over 100 fraction", "NaN", "empty"} {
		t.Run(kind, func(t *testing.T) {
			router, account := quotaFixture(t)
			s := router.Service
			ctx := context.Background()
			now := time.Now().UnixMilli()
			if _, err := s.Store.DB.Exec(`UPDATE usage_current SET weekly_used_percent_raw=100,complete_read_at_ms=?`, now-60000); err != nil {
				t.Fatal(err)
			}
			if err := router.blockQuota(ctx, account, now-60000); err != nil {
				t.Fatal(err)
			}
			raw := usageEvidence(account, 80, 100)
			switch kind {
			case "missing weekly":
				delete(raw, "secondary")
			case "missing percent":
				delete(raw["secondary"].(map[string]any), "usedPercent")
			case "negative":
				raw["secondary"].(map[string]any)["usedPercent"] = int64(-1)
			case "over 100":
				raw["secondary"].(map[string]any)["usedPercent"] = int64(101)
			case "negative fraction":
				raw["secondary"].(map[string]any)["usedPercent"] = -0.5
			case "over 100 fraction":
				raw["secondary"].(map[string]any)["usedPercent"] = 100.5
			case "NaN":
				raw["secondary"].(map[string]any)["usedPercent"] = math.NaN()
			case "empty":
				raw = map[string]any{}
			}
			if err := s.commitUsageAt(ctx, account, "invalid", raw, 0, "test", nil, false, false, now); err == nil {
				t.Fatal("invalid observation accepted")
			}
			var weekly, read, blocks, pending, snapshots int64
			if err := s.Store.DB.QueryRow(`SELECT weekly_used_percent_raw,complete_read_at_ms,(SELECT count(*) FROM account_quota_blocks),(SELECT count(*) FROM usage_refresh_requests),(SELECT count(*) FROM usage_snapshots) FROM usage_current WHERE account_id='a'`).Scan(&weekly, &read, &blocks, &pending, &snapshots); err != nil {
				t.Fatal(err)
			}
			if weekly != 100 || read != now-60000 || blocks != 1 || pending != 1 || snapshots != 0 {
				t.Fatalf("baseline changed: %d %d %d %d %d", weekly, read, blocks, pending, snapshots)
			}
		})
	}
}

func TestOlderUsageCannotOverwriteNewerObservation(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := s.commitUsageAt(ctx, account, "new", usageEvidence(account, 90, 50), 0, "test", nil, false, false, now); err != nil {
		t.Fatal(err)
	}
	if err := s.commitUsageAt(ctx, account, "old", usageEvidence(account, 80, 40), 0, "test", nil, false, false, now-1000); err == nil {
		t.Fatal("older observation accepted")
	}
	var short, weekly, read, snapshots int64
	if err := s.Store.DB.QueryRow(`SELECT short_used_percent_raw,weekly_used_percent_raw,complete_read_at_ms,(SELECT count(*) FROM usage_snapshots) FROM usage_current WHERE account_id='a'`).Scan(&short, &weekly, &read, &snapshots); err != nil {
		t.Fatal(err)
	}
	if short != 90 || weekly != 50 || read != now || snapshots != 1 {
		t.Fatalf("new observation overwritten: %d %d %d %d", short, weekly, read, snapshots)
	}
}

func TestExhaustedResetReportingLagSchedulesRecovery(t *testing.T) {
	for _, kind := range []string{"short", "weekly", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			router, account := quotaFixture(t)
			s := router.Service
			ctx := context.Background()
			now := time.Now().UnixMilli()
			raw := usageEvidence(account, 80, 40)
			window := raw["primary"].(map[string]any)
			if kind == "weekly" {
				window = raw["secondary"].(map[string]any)
			}
			window["usedPercent"] = int64(100)
			window["resetsAt"] = now/1000 - 60
			if kind == "unknown" {
				delete(window, "resetsAt")
			}
			if err := s.commitUsageAt(ctx, account, "lagged", raw, 0, "test", nil, false, false, now); err != nil {
				t.Fatal(err)
			}
			claimResets(t, s, now+usageRetryDelay.Milliseconds()-1, 0)
			claims := claimResets(t, s, now+usageRetryDelay.Milliseconds(), 1)
			observed := now + usageRetryDelay.Milliseconds() + 1
			if err := s.commitUsageAt(ctx, account, claims[0].operation, usageEvidence(account, 0, 40), 0, "recovered", nil, false, false, observed); err != nil {
				t.Fatal(err)
			}
			var pending int
			if err := s.Store.DB.QueryRow(`SELECT count(*) FROM usage_refresh_requests`).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("recovery request remains: %d %v", pending, err)
			}
		})
	}
}

func TestManualRefreshDoesNotOverlapPulseAndStillDeduplicates(t *testing.T) {
	router, _ := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	if _, err := s.Store.DB.Exec(`INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) VALUES('busy','a','window.pulse','USER','QUEUED',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, "public", "USER"); err == nil {
		t.Fatal("manual refresh overlapped pulse")
	}
	if _, err := s.Store.DB.Exec(`UPDATE operations SET kind='usage.refresh' WHERE operation_id='busy'`); err != nil {
		t.Fatal(err)
	}
	operation, err := s.Refresh(ctx, "public", "USER")
	if err != nil || operation != "busy" {
		t.Fatalf("refresh not deduplicated: %s %v", operation, err)
	}
}

func TestPulseCannotMoveCadenceBackward(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := s.Store.DB.Exec(`UPDATE weekly_cycle_state SET cadence_anchor_at_ms=?`, now); err != nil {
		t.Fatal(err)
	}
	raw := usageEvidence(account, 1, 1)
	next := account.WeeklyReset.Int64 * 1000
	if err := s.commitUsageAt(ctx, account, "pulse", raw, 0, "test", &next, true, false, now); err != nil {
		t.Fatal(err)
	}
	var anchor int64
	if err := s.Store.DB.QueryRow(`SELECT cadence_anchor_at_ms FROM weekly_cycle_state`).Scan(&anchor); err != nil || anchor != now {
		t.Fatalf("cadence moved backward: %d want %d err=%v", anchor, now, err)
	}
}

func TestQueuedRefreshRechecksEligibilityBeforeUsingCredentials(t *testing.T) {
	for _, statement := range []string{
		`UPDATE accounts SET enabled=0`,
		`UPDATE accounts SET deleted_at_ms=1`,
		`UPDATE account_state SET auth_state='AUTH_REQUIRED'`,
		`UPDATE account_state SET worker_state='QUARANTINED'`,
	} {
		t.Run(statement, func(t *testing.T) {
			router, account := quotaFixture(t)
			s := router.Service
			ctx := context.Background()
			operation, err := s.CreateOperation(ctx, account.ID, "usage.refresh", "SCHEDULED")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
			s.runRefresh(ctx, account, operation, false)
			var state, code string
			if err := s.Store.DB.QueryRow(`SELECT state,error_code FROM operations WHERE operation_id=?`, operation).Scan(&state, &code); err != nil || state != "FAILED" || code != "USAGE_REFRESH_NOT_ELIGIBLE" {
				t.Fatalf("queued job used stale eligibility: %s %s %v", state, code, err)
			}
		})
	}
}

func TestRecoveryBackoffDoesNotPostponeQuotaGrace(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := router.blockQuota(ctx, account, now); err != nil {
		t.Fatal(err)
	}
	raw := usageEvidence(account, 100, 40)
	raw["primary"].(map[string]any)["resetsAt"] = now/1000 - 60
	if err := s.commitUsageAt(ctx, account, "early", raw, 0, "test", nil, false, false, now+1); err != nil {
		t.Fatal(err)
	}
	var due int64
	if err := s.Store.DB.QueryRow(`SELECT due_at_ms FROM usage_refresh_requests WHERE account_id='a'`).Scan(&due); err != nil || due != now+usageResetGrace.Milliseconds() {
		t.Fatalf("recovery postponed quota check: %d %v", due, err)
	}
	claimResets(t, s, now+usageResetGrace.Milliseconds(), 1)
}
