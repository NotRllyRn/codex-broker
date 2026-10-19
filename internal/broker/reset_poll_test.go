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

func TestResetRefreshWaitsTwoMinutesAndCoalescesWindows(t *testing.T) {
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
			claims := claimResets(t, service, (reset+120)*1000, 1)
			if _, err := service.Store.DB.Exec("UPDATE operations SET state='SUCCEEDED' WHERE operation_id=?", claims[0].operation); err != nil {
				t.Fatal(err)
			}
			claimResets(t, service, (reset+719)*1000, 0)
			claimResets(t, service, (reset+720)*1000, 1)
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
	reset := time.Now().Unix() - 180
	setResetTimes(t, service, reset, reset+600)
	claimResets(t, service, reset*1000, 0)
	// An earlier read starts during grace, finishes after grace, and reports
	// the next window. It must not satisfy the delayed fresh read.
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(300), "resetsAt": reset + 18000}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": reset + 600}}
	if err := service.commitUsage(context.Background(), account, "early", raw, 2*time.Minute, "test", nil, false); err != nil {
		t.Fatal(err)
	}
	claimResets(t, service, (reset+60)*1000, 0)
	claims := claimResets(t, service, (reset+120)*1000, 1)
	if _, err := service.Store.DB.Exec("UPDATE operations SET state='SUCCEEDED' WHERE operation_id=?", claims[0].operation); err != nil {
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
	claimResets(t, service, (reset+60)*1000, 0)
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(0), "windowDurationMins": int64(300), "resetsAt": reset}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": reset + 18000}}
	if err := service.commitUsage(context.Background(), account, "hourly", raw, 0, "test", nil, false); err != nil {
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
