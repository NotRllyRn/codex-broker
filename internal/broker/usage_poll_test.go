package broker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func TestRegularRefreshTopFourPacingAndRestart(t *testing.T) {
	for _, count := range []int{50, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			router, _ := quotaFixture(t)
			s := router.Service
			now := time.Now().UnixMilli()
			// Clone only scheduler metadata; no production credentials or runtime.
			err := s.Store.Write(context.Background(), func(db store.Executor) error {
				for i := 1; i < count; i++ {
					id := fmt.Sprint(i)
					for j, query := range []string{
						`INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'ACTIVE',1,1)`,
						`INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES(?,'VERIFIED','STOPPED','HEALTHY','FRESH',1,1)`,
						`INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw) VALUES(?,1,1)`,
						`INSERT INTO credential_bundles SELECT ?,?,state,envelope_version,payload_schema_version,key_id,nonce,ciphertext,aad,codex_version,created_at_ms,promoted_at_ms,retired_at_ms FROM credential_bundles WHERE account_id='a'`,
					} {
						args := []any{id}
						if j == 0 {
							args = append(args, id, id)
						}
						if j == 3 {
							args = append(args, id)
						}
						if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// Rotation order deliberately differs from account ID and read age.
			if _, err := s.Store.DB.Exec(`INSERT INTO window_pulse_state(account_id,cycle_state,last_attempt_at_ms,weekly_started_at_ms) SELECT account_id,'IN_CYCLE',0,CASE account_id WHEN '4' THEN 40 WHEN '3' THEN 42 WHEN '2' THEN 43 WHEN '1' THEN 44 ELSE 100 END FROM accounts`); err != nil {
				t.Fatal(err)
			}
			s.Config.WindowPulseEnabled = true
			seen := map[string]int{}
			for i := 0; i < 40; i++ {
				// Reconstructing the service cannot bypass the persisted deadline.
				s = &Service{Store: s.Store, Config: s.Config}
				claims, err := s.claimRegularRefresh(context.Background(), now+int64(i)*usageRefreshInterval.Milliseconds())
				if err != nil || len(claims) != 1 {
					t.Fatalf("slot %d: %v %+v", i, err, claims)
				}
				claim := claims[0]
				seen[claim.account.ID]++
				blocked, err := s.claimRegularRefresh(context.Background(), now+int64(i)*usageRefreshInterval.Milliseconds()+1)
				if err != nil || len(blocked) != 0 {
					t.Fatalf("deadline bypassed: %v %+v", err, blocked)
				}
				if _, err := s.Store.DB.Exec(`UPDATE operations SET state='SUCCEEDED' WHERE operation_id=?`, claim.operation); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Store.DB.Exec(`UPDATE usage_current SET complete_read_at_ms=?,last_attempt_at_ms=? WHERE account_id=?`, now+int64(i)*usageRefreshInterval.Milliseconds(), now+int64(i)*usageRefreshInterval.Milliseconds(), claim.account.ID); err != nil {
					t.Fatal(err)
				}
			}
			if len(seen) != 4 {
				t.Fatalf("starved accounts: %v", seen)
			}
			for _, id := range []string{"4", "3", "2", "1"} {
				if seen[id] != 10 {
					t.Fatalf("rotation membership: %v", seen)
				}
			}
			if _, err := s.Store.DB.Exec(`UPDATE usage_current SET weekly_used_percent_raw=100 WHERE account_id='4'`); err != nil {
				t.Fatal(err)
			}
			claims, err := s.claimRegularRefresh(context.Background(), now+40*usageRefreshInterval.Milliseconds())
			if err != nil || len(claims) != 1 || claims[0].account.ID != "10" {
				t.Fatalf("replacement: %+v %v", claims, err)
			}
			if _, err := s.Store.DB.Exec(`INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) SELECT 'busy-'||account_id,account_id,'usage.refresh','USER','RUNNING',1,1 FROM accounts WHERE account_id IN('1','2','3')`); err != nil {
				t.Fatal(err)
			}
			claims, err = s.claimRegularRefresh(context.Background(), now+41*usageRefreshInterval.Milliseconds())
			if err != nil || len(claims) != 0 {
				t.Fatalf("busy top four admitted another account: %+v %v", claims, err)
			}
			for id, n := range seen {
				if n != 10 {
					t.Fatalf("account %s got %d reads", id, n)
				}
			}
		})
	}
}

func TestRegularRefreshEligibilityAndFailureCooldown(t *testing.T) {
	router, _ := quotaFixture(t)
	s := router.Service
	s.Config.WindowPulseEnabled = true
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for _, test := range []struct {
		state   string
		used    int
		blocked bool
		want    int
	}{
		{"HELD", 1, false, 0}, {"IN_CYCLE", 100, false, 0}, {"IN_CYCLE", 1, true, 0}, {"IN_CYCLE", 1, false, 1},
	} {
		if _, err := s.Store.DB.Exec(`INSERT INTO window_pulse_state(account_id,cycle_state,last_attempt_at_ms) VALUES('a',?,0) ON CONFLICT(account_id) DO UPDATE SET cycle_state=excluded.cycle_state`, test.state); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.DB.Exec(`UPDATE usage_current SET short_used_percent_raw=?,weekly_used_percent_raw=1`, test.used); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.DB.Exec(`DELETE FROM account_quota_blocks; UPDATE usage_poll_state SET next_regular_at_ms=0`); err != nil {
			t.Fatal(err)
		}
		if test.blocked {
			if err := router.blockQuota(ctx, Account{ID: "a"}, now); err != nil {
				t.Fatal(err)
			}
		} else {
			s.Store.DB.Exec(`DELETE FROM usage_refresh_requests`)
		}
		claims, err := s.claimRegularRefresh(ctx, now)
		if err != nil || len(claims) != test.want {
			t.Fatalf("%+v: %+v %v", test, claims, err)
		}
	}
	if _, err := s.Store.DB.Exec(`UPDATE usage_current SET last_attempt_at_ms=?,complete_read_at_ms=?`, now, now-1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`UPDATE operations SET state='FAILED'`); err != nil {
		t.Fatal(err)
	}
	claims, err := s.claimRegularRefresh(ctx, now+usageRefreshInterval.Milliseconds())
	if err != nil || len(claims) != 0 {
		t.Fatalf("failure ignored backoff: %v %v", claims, err)
	}
	claims, err = s.claimRegularRefresh(ctx, now+usageRetryDelay.Milliseconds())
	if err != nil || len(claims) != 1 {
		t.Fatalf("failure never retried: %v %v", claims, err)
	}
}

func TestQuotaEventGraceCoalescingAndRetry(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := router.blockQuota(ctx, account, now); err != nil {
		t.Fatal(err)
	}
	claimResets(t, s, now+usageResetGrace.Milliseconds()-1, 0)
	// A second rejection moves the coalesced read after the latest grace.
	if err := router.blockQuota(ctx, account, now+10000); err != nil {
		t.Fatal(err)
	}
	claimResets(t, s, now+usageResetGrace.Milliseconds(), 0)
	due := now + 10000 + usageResetGrace.Milliseconds()
	claims := claimResets(t, s, due, 1)
	if _, err := s.Store.DB.Exec(`UPDATE operations SET state='FAILED' WHERE operation_id=?`, claims[0].operation); err != nil {
		t.Fatal(err)
	}
	claimResets(t, &Service{Store: s.Store}, due+usageRetryDelay.Milliseconds()-1, 0)
	claimResets(t, &Service{Store: s.Store}, due+usageRetryDelay.Milliseconds(), 1)
}

func TestEverySuccessfulReadUpdatesQueueTimestampAndFailurePreservesIt(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	now := time.Now().UnixMilli()
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(80), "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}, "secondary": map[string]any{"usedPercent": int64(40), "windowDurationMins": int64(10080), "resetsAt": account.WeeklyReset.Int64}}
	for i, reason := range []string{"USER", "SCHEDULED", "WINDOW_RESET", "QUOTA_REJECTED", "RELAY_RECONCILE", "PREFLIGHT"} {
		observed := now + int64(i)
		if err := s.commitUsageAt(ctx, account, reason, raw, 0, "test", nil, false, false, observed); err != nil {
			t.Fatal(err)
		}
		var read int64
		if err := s.Store.DB.QueryRow(`SELECT complete_read_at_ms FROM usage_current WHERE account_id='a'`).Scan(&read); err != nil || read != observed {
			t.Fatalf("%s read=%d want=%d err=%v", reason, read, observed, err)
		}
	}
	if err := s.recordUsageFailure(ctx, account, "failed", time.Now(), fmt.Errorf("temporary failure")); err != nil {
		t.Fatal(err)
	}
	var read int64
	if err := s.Store.DB.QueryRow(`SELECT complete_read_at_ms FROM usage_current WHERE account_id='a'`).Scan(&read); err != nil || read != now+5 {
		t.Fatalf("failure changed successful read: %d %v", read, err)
	}
}

func TestIncompleteEventReadRetainsRequestWithBackoff(t *testing.T) {
	router, account := quotaFixture(t)
	s := router.Service
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := router.blockQuota(ctx, account, now-usageResetGrace.Milliseconds()-1); err != nil {
		t.Fatal(err)
	}
	claims := claimResets(t, s, now, 1)
	raw := map[string]any{"primary": map[string]any{"usedPercent": int64(80), "windowDurationMins": int64(300), "resetsAt": account.ShortReset.Int64}}
	if err := s.commitUsageAt(ctx, account, claims[0].operation, raw, 0, "partial read", nil, false, false, now+1); err == nil {
		t.Fatal("partial observation accepted")
	} else if err := s.recordUsageFailure(ctx, account, claims[0].operation, time.Now(), err); err != nil {
		t.Fatal(err)
	}
	claimResets(t, s, now+15000, 0)
	claimResets(t, &Service{Store: s.Store}, now+usageRetryDelay.Milliseconds(), 1)
}

func TestConcurrentRegularClaimsConsumeOnlyOneSlot(t *testing.T) {
	router, _ := quotaFixture(t)
	s := router.Service
	now := time.Now().UnixMilli()
	var wg sync.WaitGroup
	counts := make(chan int, 8)
	for range 8 {
		wg.Go(func() {
			claims, err := s.claimRegularRefresh(context.Background(), now)
			if err != nil {
				t.Error(err)
			}
			counts <- len(claims)
		})
	}
	wg.Wait()
	close(counts)
	total := 0
	for count := range counts {
		total += count
	}
	if total != 1 {
		t.Fatalf("concurrent claims consumed %d slots", total)
	}
}

func TestDailySweepIncludesHeldAndExhaustedAndSharesReadPriority(t *testing.T) {
	router, _ := quotaFixture(t)
	s := router.Service
	s.Config.WindowPulseEnabled = true
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, err := s.Store.DB.Exec(`
 INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) SELECT 'b','b','Example user',enabled,lifecycle_state,2,2 FROM accounts WHERE account_id='a';
 INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) SELECT 'b',auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms FROM account_state WHERE account_id='a';
 INSERT INTO credential_bundles SELECT 'b','b',state,envelope_version,payload_schema_version,key_id,nonce,ciphertext,aad,codex_version,created_at_ms,promoted_at_ms,retired_at_ms FROM credential_bundles WHERE account_id='a';
 INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw,complete_read_at_ms) VALUES('b',100,100,1);
 INSERT INTO window_pulse_state(account_id,cycle_state,last_attempt_at_ms) VALUES('a','HELD',0),('b','IN_CYCLE',0);
 UPDATE usage_current SET complete_read_at_ms=2 WHERE account_id='a';`)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.claimScheduledRefresh(ctx, now, true)
	if err != nil || len(claims) != 1 || claims[0].account.ID != "b" {
		t.Fatalf("exhausted oldest: %+v %v", claims, err)
	}
	if _, err := s.Store.DB.Exec(`UPDATE operations SET state='SUCCEEDED'; UPDATE usage_current SET complete_read_at_ms=?,last_attempt_at_ms=? WHERE account_id='b'`, now, now); err != nil {
		t.Fatal(err)
	}
	s = &Service{Store: s.Store, Config: s.Config}
	claims, err = s.claimScheduledRefresh(ctx, now+(12*time.Hour).Milliseconds()-1, true)
	if err != nil || len(claims) != 0 {
		t.Fatalf("early sweep: %+v %v", claims, err)
	}
	claims, err = s.claimScheduledRefresh(ctx, now+(12*time.Hour).Milliseconds(), true)
	if err != nil || len(claims) != 1 || claims[0].account.ID != "a" {
		t.Fatalf("held oldest: %+v %v", claims, err)
	}
}
