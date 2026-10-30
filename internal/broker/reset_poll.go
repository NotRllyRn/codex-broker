package broker

import (
	"context"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const usageResetGrace = 45 * time.Second
const usageRefreshInterval = 36 * time.Second
const usageRetryDelay = 5 * time.Minute

type resetRefresh struct {
	account   Account
	operation string
}

// Preserve a reset that has already happened until its delayed read is handled,
// even if another read reports the next window before the grace period ends.
func scheduleUsageResets(ctx context.Context, db store.Executor, accountID string, now int64) error {
	for _, window := range []struct{ kind, column string }{
		{"short", "short_resets_at_s"}, {"weekly", "weekly_resets_at_s"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO usage_reset_polls(account_id,window_kind,reset_at_s)
			SELECT account_id,?,`+window.column+` FROM usage_current WHERE (?='' OR account_id=?) AND `+window.column+`>0
			ON CONFLICT(account_id,window_kind) DO UPDATE SET reset_at_s=excluded.reset_at_s
			WHERE excluded.reset_at_s>usage_reset_polls.last_polled_reset_at_s
			AND excluded.reset_at_s<>usage_reset_polls.reset_at_s
			AND (usage_reset_polls.reset_at_s<=usage_reset_polls.last_polled_reset_at_s OR usage_reset_polls.reset_at_s>?)`,
			window.kind, accountID, accountID, now/1000)
		if err != nil {
			return err
		}
	}
	return nil
}

func settleUsageResets(ctx context.Context, db store.Executor, accountID string, readStartedAtMS int64) error {
	_, err := db.ExecContext(ctx, `UPDATE usage_reset_polls SET last_polled_reset_at_s=reset_at_s
		WHERE account_id=? AND reset_at_s<=? AND reset_at_s>last_polled_reset_at_s`,
		accountID, (readStartedAtMS-usageResetGrace.Milliseconds())/1000)
	return err
}

func (s *Service) claimResetRefreshes(ctx context.Context, now int64) (claimed []resetRefresh, err error) {
	err = s.Store.Write(ctx, func(db store.Executor) error {
		if err := scheduleUsageResets(ctx, db, "", now); err != nil {
			return err
		}
		// Held accounts with real usage still need their reset observation; idle
		// rolling windows do not generate background reads.
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO usage_refresh_requests(account_id,due_at_ms,reason)
            SELECT p.account_id,min(p.reset_at_s)*1000+?,'WINDOW_RESET' FROM usage_reset_polls p
            JOIN usage_current u USING(account_id) LEFT JOIN window_pulse_state w USING(account_id)
            WHERE p.reset_at_s<=? AND p.reset_at_s>p.last_polled_reset_at_s
            AND (? OR w.cycle_state='IN_CYCLE' OR u.short_used_percent_raw>0 OR u.weekly_used_percent_raw>0
                OR EXISTS(SELECT 1 FROM account_quota_blocks q WHERE q.account_id=p.account_id)) GROUP BY p.account_id`,
			usageResetGrace.Milliseconds(), (now-usageResetGrace.Milliseconds())/1000, !s.Config.WindowPulseEnabled); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT a.account_id,a.public_token,r.reason FROM usage_refresh_requests r
            JOIN accounts a USING(account_id) JOIN account_state s USING(account_id)
            WHERE r.due_at_ms<=? AND r.next_attempt_at_ms<=? AND a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state='STOPPED'
            AND EXISTS(SELECT 1 FROM credential_bundles b WHERE b.account_id=a.account_id AND b.state='ACTIVE')
            AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED'))`, now, now)
		if err != nil {
			return err
		}
		type candidate struct {
			account Account
			reason  string
		}
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.account.ID, &item.account.PublicID, &item.reason); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, item := range candidates {
			refresh, err := claimUsageRefresh(ctx, db, item.account, item.reason, now)
			if err != nil {
				return err
			}
			// Keep the retry deadline independent of successful-read timestamps:
			// partial responses cannot satisfy the request or cause rapid retries.
			if _, err := db.ExecContext(ctx, "UPDATE usage_refresh_requests SET next_attempt_at_ms=? WHERE account_id=?", now+usageRetryDelay.Milliseconds(), item.account.ID); err != nil {
				return err
			}
			// The durable request owns retries after the reset watermark is claimed.
			if err := settleUsageResets(ctx, db, item.account.ID, now); err != nil {
				return err
			}
			claimed = append(claimed, refresh)
		}
		return nil
	})
	return claimed, err
}

func (s *Service) resetUsageLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if refreshes, err := s.claimResetRefreshes(ctx, core.NowMS()); err == nil {
			s.startUsageRefreshes(ctx, refreshes)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) startUsageRefreshes(ctx context.Context, refreshes []resetRefresh) {
	for _, refresh := range refreshes {
		account, err := s.account(ctx, refresh.account.PublicID)
		if err != nil {
			_ = s.FailOperation(ctx, refresh.operation, "USAGE_REFRESH_FAILED", "Account unavailable")
			continue
		}
		if !s.runAsync(func() { s.runRefresh(s.workerCtx, account, refresh.operation, false) }) {
			_ = s.FailOperation(context.Background(), refresh.operation, "SERVICE_STOPPING", "Service is stopping")
		}
	}
}
