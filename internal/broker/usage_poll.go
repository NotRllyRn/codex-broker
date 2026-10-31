package broker

import (
	"context"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

// Event reads are coalesced and survive restarts. A newer rejection must settle
// before a read can satisfy it; successful reads delete only requests they cover.
func requestUsageRefresh(ctx context.Context, db store.Executor, accountID, reason string, due int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO usage_refresh_requests(account_id,due_at_ms,reason) VALUES(?,?,?)
        ON CONFLICT(account_id) DO UPDATE SET due_at_ms=max(due_at_ms,excluded.due_at_ms),reason=excluded.reason,next_attempt_at_ms=0`, accountID, due, reason)
	return err
}

func claimUsageRefresh(ctx context.Context, db store.Executor, account Account, reason string, now int64) (resetRefresh, error) {
	operation := core.NewID()
	if _, err := db.ExecContext(ctx, `INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version)
        VALUES(?,?,'usage.refresh',?,'QUEUED',?,1)`, operation, account.ID, reason, now); err != nil {
		return resetRefresh{}, err
	}
	_, err := db.ExecContext(ctx, `UPDATE usage_current SET last_attempt_at_ms=? WHERE account_id=?`, now, account.ID)
	return resetRefresh{account: account, operation: operation}, err
}

// Both lanes share successful-read timestamps, but only the daily sweep uses
// them for priority. The regular lane walks the first four rotation members.
func (s *Service) claimRegularRefresh(ctx context.Context, now int64) ([]resetRefresh, error) {
	return s.claimScheduledRefresh(ctx, now, false)
}

func (s *Service) claimScheduledRefresh(ctx context.Context, now int64, daily bool) (claimed []resetRefresh, err error) {
	err = s.Store.Write(ctx, func(db store.Executor) error {
		column, reason := "next_regular_at_ms", "SCHEDULED"
		interval := usageRefreshInterval.Milliseconds()
		if daily {
			column, reason = "next_sweep_at_ms", "DAILY_SWEEP"
		}
		var due int64
		var cursor string
		if err := db.QueryRowContext(ctx, "SELECT "+column+",regular_cursor FROM usage_poll_state WHERE singleton_id=1").Scan(&due, &cursor); err != nil {
			return err
		}
		if due > now {
			return nil
		}
		const healthy = ` FROM accounts a JOIN account_state s USING(account_id) JOIN usage_current u USING(account_id)
   LEFT JOIN window_pulse_state p USING(account_id)
   WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED'
   AND EXISTS(SELECT 1 FROM credential_bundles b WHERE b.account_id=a.account_id AND b.state='ACTIVE')`
		if daily {
			var count int64
			if err := db.QueryRowContext(ctx, "SELECT count(*)"+healthy).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return nil
			}
			interval = max(int64(1000), (24*time.Hour).Milliseconds()/count)
		}
		if _, err := db.ExecContext(ctx, "UPDATE usage_poll_state SET "+column+"=? WHERE singleton_id=1", now+interval); err != nil {
			return err
		}
		query := "SELECT a.account_id,a.public_token" + healthy
		args := []any{}
		if !daily {
			query += ` AND (? OR p.cycle_state='IN_CYCLE')
    AND COALESCE(u.short_used_percent_raw,0)<100 AND COALESCE(u.weekly_used_percent_raw,0)<100
    AND NOT EXISTS(SELECT 1 FROM account_quota_blocks q WHERE q.account_id=a.account_id)`
			// Freeze membership before skipping busy/backing-off members: those
			// temporary states must not pull a fifth account into this lane.
			query = "WITH candidates AS (" + query + ` ORDER BY p.weekly_started_at_ms IS NULL,p.weekly_started_at_ms,a.account_id LIMIT 4) SELECT a.account_id,a.public_token` + healthy + ` AND a.account_id IN(SELECT account_id FROM candidates)`
			args = append(args, !s.Config.WindowPulseEnabled)
		}
		query += ` AND s.worker_state='STOPPED'
   AND (u.last_attempt_at_ms IS NULL OR u.last_attempt_at_ms<=COALESCE(u.complete_read_at_ms,0) OR u.last_attempt_at_ms<=?)
   AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED'))`
		args = append(args, now-usageRetryDelay.Milliseconds())
		if daily {
			query += ` ORDER BY COALESCE(u.complete_read_at_ms,0),a.created_at_ms,a.account_id LIMIT 1`
		} else {
			query += ` ORDER BY p.weekly_started_at_ms IS NULL,p.weekly_started_at_ms,a.account_id LIMIT 4`
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		var accounts []Account
		for rows.Next() {
			var account Account
			if err := rows.Scan(&account.ID, &account.PublicID); err != nil {
				rows.Close()
				return err
			}
			accounts = append(accounts, account)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(accounts) == 0 {
			return err
		}
		index := 0
		if !daily {
			for i, account := range accounts {
				if account.ID == cursor {
					index = (i + 1) % len(accounts)
					break
				}
			}
		}
		account := accounts[index]
		refresh, err := claimUsageRefresh(ctx, db, account, reason, now)
		if err != nil {
			return err
		}
		if !daily {
			if _, err := db.ExecContext(ctx, "UPDATE usage_poll_state SET regular_cursor=? WHERE singleton_id=1", account.ID); err != nil {
				return err
			}
		}
		claimed = append(claimed, refresh)
		return nil
	})
	return
}

func (s *Service) usageLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if refreshes, err := s.claimRegularRefresh(ctx, core.NowMS()); err == nil {
			s.startUsageRefreshes(ctx, refreshes)
		}
		if refreshes, err := s.claimScheduledRefresh(ctx, core.NowMS(), true); err == nil {
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
