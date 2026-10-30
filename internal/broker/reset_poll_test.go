package broker

import (
	"context"
	"testing"
	"time"
)

func setResetTimes(t *testing.T, service *Service, short, weekly int64) {
	t.Helper()
	if _, err := service.Store.DB.Exec("UPDATE usage_current SET short_resets_at_s=?,weekly_resets_at_s=? WHERE account_id='a'", short, weekly); err != nil {
		t.Fatal(err)
	}
}

func claimResets(t *testing.T, service *Service, now int64, want int) []resetRefresh {
	t.Helper()
	claims, err := service.claimResetRefreshes(context.Background(), now)
	if err != nil || len(claims) != want {
		t.Fatalf("claims=%+v want=%d err=%v", claims, want, err)
	}
	return claims
}

func TestResetRefreshWaitsForGraceAndCoalescesWindows(t *testing.T) {
	router, _ := quotaFixture(t)
	service := router.Service
	reset := time.Now().Unix()
	setResetTimes(t, service, reset, reset)
	claimResets(t, service, reset*1000, 0)
	claimResets(t, service, reset*1000+usageResetGrace.Milliseconds()-1, 0)
	claims := claimResets(t, service, reset*1000+usageResetGrace.Milliseconds(), 1)
	var trigger string
	if err := service.Store.DB.QueryRow("SELECT trigger FROM operations WHERE operation_id=?", claims[0].operation).Scan(&trigger); err != nil || trigger != "WINDOW_RESET" {
		t.Fatalf("trigger=%s err=%v", trigger, err)
	}
	if _, err := service.Store.DB.Exec("UPDATE operations SET state='FAILED' WHERE operation_id=?", claims[0].operation); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, reset*1000+3*time.Minute.Milliseconds(), 0)
	// A new service instance shares the persisted processed-reset watermarks.
	claimResets(t, &Service{Store: service.Store}, reset*1000+4*time.Minute.Milliseconds(), 0)
	var rows int
	if err := service.Store.DB.QueryRow("SELECT count(*) FROM usage_reset_polls").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("schedule rows=%d err=%v", rows, err)
	}
}

func TestResetWindowsRefreshIndependently(t *testing.T) {
	for _, first := range []string{"short", "weekly"} {
		t.Run(first, func(t *testing.T) {
			router, _ := quotaFixture(t)
			service := router.Service
			reset := time.Now().Unix()
			short, weekly := reset, reset+600
			if first == "weekly" {
				short, weekly = weekly, short
			}
			setResetTimes(t, service, short, weekly)
			claims := claimResets(t, service, (reset+60)*1000, 1)
			if _, err := service.Store.DB.Exec("UPDATE operations SET state='SUCCEEDED' WHERE operation_id=?", claims[0].operation); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Store.DB.Exec("DELETE FROM usage_refresh_requests; UPDATE usage_current SET complete_read_at_ms=last_attempt_at_ms"); err != nil {
				t.Fatal(err)
			}
			claimResets(t, service, (reset+644)*1000, 0)
			claimResets(t, service, (reset+645)*1000, 1)
		})
	}
}

func TestResetRefreshWaitsForBusyAccount(t *testing.T) {
	router, _ := quotaFixture(t)
	service := router.Service
	now := time.Now().UnixMilli()
	setResetTimes(t, service, now/1000-300, now/1000+600)
	if _, err := service.Store.DB.Exec("UPDATE account_state SET worker_state='CREDENTIAL_IN_USE' WHERE account_id='a'"); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, now, 0)
	if _, err := service.Store.DB.Exec(`UPDATE account_state SET worker_state='STOPPED' WHERE account_id='a'; INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) VALUES('busy','a','usage.refresh','SCHEDULED','RUNNING',1,1)`); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, now, 0)
	if _, err := service.Store.DB.Exec("UPDATE operations SET state='SUCCEEDED' WHERE operation_id='busy'"); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, now, 1)
}

func TestPollDuringGraceDoesNotLosePendingReset(t *testing.T) {
	router, account := quotaFixture(t)
	service := router.Service
	reset := time.Now().Unix() - 60
	setResetTimes(t, service, reset, reset+600)
	claimResets(t, service, reset*1000, 0)
	// An earlier read starts during grace, finishes after grace, and reports
	// the next window. It must not satisfy the delayed fresh read.
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(300), "resetsAt": reset + 18000}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": reset + 600}}
	if err := service.commitUsage(context.Background(), account, "early", raw, 40*time.Second, "test", nil, false, false); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, (reset+44)*1000, 0)
	claims := claimResets(t, service, (reset+60)*1000, 1)
	if _, err := service.Store.DB.Exec("UPDATE operations SET state='SUCCEEDED' WHERE operation_id=?", claims[0].operation); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.DB.Exec("DELETE FROM usage_refresh_requests; UPDATE usage_current SET complete_read_at_ms=last_attempt_at_ms"); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, (reset+121)*1000, 0)
	var next int64
	if err := service.Store.DB.QueryRow("SELECT reset_at_s FROM usage_reset_polls WHERE window_kind='short'").Scan(&next); err != nil || next != reset+18000 {
		t.Fatalf("next short reset=%d err=%v", next, err)
	}
}

func TestFreshPollAfterGraceSatisfiesResetRefresh(t *testing.T) {
	router, account := quotaFixture(t)
	service := router.Service
	reset := time.Now().Unix() - 300
	setResetTimes(t, service, reset, reset+18000)
	claimResets(t, service, (reset+44)*1000, 0)
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(300), "resetsAt": reset}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": reset + 18000}}
	if err := service.commitUsage(context.Background(), account, "hourly", raw, 0, "test", nil, false, false); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, time.Now().UnixMilli(), 0)
}

func TestResetRefreshSkipsUnknownWindowsAndDisabledAccounts(t *testing.T) {
	router, _ := quotaFixture(t)
	service := router.Service
	now := time.Now().UnixMilli()
	setResetTimes(t, service, 0, 0)
	claimResets(t, service, now, 0)
	setResetTimes(t, service, now/1000-300, now/1000-300)
	if _, err := service.Store.DB.Exec("UPDATE accounts SET enabled=0 WHERE account_id='a'"); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, now, 0)
	if _, err := service.Store.DB.Exec("UPDATE accounts SET enabled=1 WHERE account_id='a'"); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, now, 1)
}

func TestManualReadStartedAfterGraceSatisfiesResetQueuedDuringRead(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	now := time.Now().UnixMilli()
	reset := now/1000 - 60
	setResetTimes(t, s, reset, reset+18000)
	// A manual read already owns the worker when the event loop notices reset.
	if _, err := s.Store.DB.Exec(`UPDATE account_state SET worker_state='CREDENTIAL_IN_USE'`); err != nil {
		t.Fatal(err)
	}
	claimResets(t, s, now, 0)
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(300), "resetsAt": reset + 18000}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": reset + 18000}}
	if err := s.commitUsageAt(context.Background(), account, "manual", raw, 5*time.Second, "test", nil, false, false, now+1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`UPDATE account_state SET worker_state='STOPPED'`); err != nil {
		t.Fatal(err)
	}
	claimResets(t, s, now+2000, 0)
	var pending int
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM usage_refresh_requests`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("redundant reset request: %d %v", pending, err)
	}
}
