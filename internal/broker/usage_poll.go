package broker

import (
	"context"
	"database/sql"
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

func (s *Service) claimRegularRefresh(ctx context.Context, now int64) (claimed []resetRefresh, err error) {
	err = s.Store.Write(ctx, func(db store.Executor) error {
		var due int64
		if err := db.QueryRowContext(ctx, `SELECT next_regular_at_ms FROM usage_poll_state WHERE singleton_id=1`).Scan(&due); err != nil {
			return err
		}
		if due > now {
			return nil
		}
		if _, err := db.ExecContext(ctx, `UPDATE usage_poll_state SET next_regular_at_ms=? WHERE singleton_id=1`, now+usageRefreshInterval.Milliseconds()); err != nil {
			return err
		}
		var account Account
		err := db.QueryRowContext(ctx, `SELECT a.account_id,a.public_token FROM accounts a
            JOIN account_state s USING(account_id) JOIN usage_current u USING(account_id)
            LEFT JOIN window_pulse_state p USING(account_id)
            WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state='STOPPED'
            AND (? OR p.cycle_state='IN_CYCLE')
            AND COALESCE(u.short_used_percent_raw,0)<100 AND COALESCE(u.weekly_used_percent_raw,0)<100
            AND EXISTS(SELECT 1 FROM credential_bundles b WHERE b.account_id=a.account_id AND b.state='ACTIVE')
            AND NOT EXISTS(SELECT 1 FROM account_quota_blocks q WHERE q.account_id=a.account_id)
            AND NOT EXISTS(SELECT 1 FROM usage_refresh_requests r WHERE r.account_id=a.account_id)
            AND (u.last_attempt_at_ms IS NULL OR u.last_attempt_at_ms<=COALESCE(u.complete_read_at_ms,0) OR u.last_attempt_at_ms<=?)
            AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED'))
            ORDER BY COALESCE(u.complete_read_at_ms,0),a.created_at_ms,a.account_id LIMIT 1`, !s.Config.WindowPulseEnabled, now-usageRetryDelay.Milliseconds()).Scan(&account.ID, &account.PublicID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		refresh, err := claimUsageRefresh(ctx, db, account, "SCHEDULED", now)
		if err == nil {
			claimed = append(claimed, refresh)
		}
		return err
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
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}
