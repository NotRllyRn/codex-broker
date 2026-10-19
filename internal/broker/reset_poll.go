package broker

import (
	"context"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const usageResetGrace = 2 * time.Minute

type resetRefresh struct {
	account   Account
	operation string
}

// Preserve a reset that has already happened until its delayed read is handled,
// even if an hourly poll reports the next window before the grace period ends.
func scheduleUsageResets(ctx context.Context, db store.Executor, accountID string, now int64) error {
	for _, window := range []struct{ kind, column string }{
		{"short", "short_resets_at_s"}, {"weekly", "weekly_resets_at_s"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO usage_reset_polls(account_id,window_kind,reset_at_s)
			SELECT account_id,?,`+window.column+` FROM usage_current WHERE account_id=? AND `+window.column+`>0
			ON CONFLICT(account_id,window_kind) DO UPDATE SET reset_at_s=excluded.reset_at_s
			WHERE excluded.reset_at_s>usage_reset_polls.last_polled_reset_at_s
			AND excluded.reset_at_s<>usage_reset_polls.reset_at_s
			AND (usage_reset_polls.reset_at_s<=usage_reset_polls.last_polled_reset_at_s OR usage_reset_polls.reset_at_s>?)`,
			window.kind, accountID, now/1000)
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
		rows, err := db.QueryContext(ctx, `SELECT a.account_id,a.public_token FROM accounts a JOIN account_state s USING(account_id)
			WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED'`)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, account := range accounts {
			if err := scheduleUsageResets(ctx, db, account.ID, now); err != nil {
				return err
			}
			var due bool
			cutoff := (now - usageResetGrace.Milliseconds()) / 1000
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_reset_polls p JOIN account_state s USING(account_id)
				WHERE p.account_id=? AND p.reset_at_s<=? AND p.reset_at_s>p.last_polled_reset_at_s AND s.worker_state='STOPPED'
				AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.account_id=p.account_id AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED')))`, account.ID, cutoff).Scan(&due); err != nil {
				return err
			}
			if !due {
				continue
			}
			operation := core.NewID()
			if _, err := db.ExecContext(ctx, `INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version)
				VALUES(?,?,'usage.refresh','WINDOW_RESET','QUEUED',?,1)`, operation, account.ID, now); err != nil {
				return err
			}
			// Claim both due windows together. Failures fall back to the hourly poll,
			// rather than repeatedly calling upstream for the same reset timestamp.
			if err := settleUsageResets(ctx, db, account.ID, now); err != nil {
				return err
			}
			claimed = append(claimed, resetRefresh{account: account, operation: operation})
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
			for _, refresh := range refreshes {
				account, err := s.account(ctx, refresh.account.PublicID)
				if err != nil {
					_ = s.FailOperation(ctx, refresh.operation, "USAGE_REFRESH_FAILED", "Account unavailable")
					continue
				}
				if !s.runAsync(func() { s.runRefresh(s.workerCtx, account, refresh.operation) }) {
					_ = s.FailOperation(context.Background(), refresh.operation, "SERVICE_STOPPING", "Service is stopping")
				}
			}
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
