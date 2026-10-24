package broker

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

type Router struct {
	Store          *store.Store
	Service        *Service
	PaddingSeconds int
}

func (r Router) Route(ctx context.Context, keyID string, request RouteRequest) (*RouteLease, *PoolWait, error) {
	now := core.NowMS()
	if err := r.Service.reconcileCycle(ctx, now); err != nil {
		return nil, nil, err
	}
	accounts, err := r.accounts(ctx, keyID, now)
	if err != nil {
		return nil, nil, err
	}
	var failed *Account
	for index := range accounts {
		if accounts[index].PublicID == request.FailedAccountID {
			failed = &accounts[index]
		}
	}
	if request.FailedAccountID != "" && failed == nil {
		return nil, nil, core.NewError("FAILED_ACCOUNT_INVALID", "Failed account is not routable", 422)
	}
	if failed != nil && request.FailureKind != "" {
		if request.FailureKind == "quota" {
			if err := r.blockQuota(ctx, *failed, now); err != nil {
				return nil, nil, err
			}
		}
		if request.FailureKind == "auth" && !quotaBlocked(*failed, now) && (!r.Service.Config.WindowPulseEnabled || failed.CycleState == "IN_CYCLE") {
			lease, leaseErr := r.Service.Lease(ctx, *failed, now, true)
			if leaseErr == nil {
				if err := r.markRouted(ctx, *failed, now); err != nil {
					return nil, nil, err
				}
				result := routeLease(*failed, lease, now)
				return &result, nil, nil
			}
			_ = r.markLeaseAuthFailure(ctx, failed.ID, leaseErr, now)
		}
		if request.FailureKind != "quota" {
			if err := r.exclude(ctx, keyID, *failed, request.FailureKind, now); err != nil {
				return nil, nil, err
			}
		}
		accounts, err = r.accounts(ctx, keyID, now)
		if err != nil {
			return nil, nil, err
		}
	}
	var usable []Account
	var held []Account
	releasing := false
	for _, account := range accounts {
		if exhausted(account, now) || account.ExcludedUntil.Valid && account.ExcludedUntil.Int64 > now {
			continue
		}
		if r.Service.Config.WindowPulseEnabled {
			switch account.CycleState {
			case "HELD":
				if account.RelayReservedUntil.Valid && account.RelayReservedUntil.Int64 > now {
					continue
				}
				held = append(held, account)
				continue
			case "RELEASING":
				releasing = true
				continue
			}
		}
		usable = append(usable, account)
	}
	if len(usable) == 0 && len(held) > 0 {
		usable = []Account{held[rand.N(len(held))]}
	}
	if len(usable) > 0 {
		sort.Slice(usable, func(i, j int) bool { return rankLess(usable[i], usable[j], request.PreferredAccountID, now) })
		selected := usable[0]
		lease, err := r.Service.Lease(ctx, selected, now, false)
		if err != nil {
			return nil, nil, err
		}
		if err := r.markRouted(ctx, selected, now); err != nil {
			return nil, nil, err
		}
		result := routeLease(selected, lease, now)
		return &result, nil, nil
	}
	if releasing {
		return nil, &PoolWait{now + 10_000, 10}, nil
	}
	retry := nextRetry(accounts, now)
	for _, account := range accounts {
		if quotaBlocked(account, now) {
			poll := max(60, r.Service.Config.UsagePollSeconds)
			if poll == 60 && r.Service.Config.UsagePollSeconds == 0 {
				poll = 3600
			}
			pollAt := now + int64(poll+30)*1000
			if retry == 0 || pollAt < retry {
				retry = pollAt
			}
		}
	}
	if retry == 0 {
		return nil, nil, core.NewError("POOL_RESET_UNKNOWN", "No routable account has a reliable retry time", 503)
	}
	retry += int64(r.PaddingSeconds) * 1000
	wait := PoolWait{retry, int(math.Ceil(float64(retry-now) / 1000))}
	return nil, &wait, nil
}

func (r Router) markRouted(ctx context.Context, account Account, now int64) error {
	return r.Store.Write(ctx, func(db store.Executor) error {
		if r.Service.Config.WindowPulseEnabled {
			started := now
			result, err := db.ExecContext(ctx, "UPDATE window_pulse_state SET cycle_state='IN_CYCLE',weekly_started_at_ms=?,next_pulse_at_ms=?,last_error_code=NULL,idle_observation_count=0,last_usage_observed_at_ms=(SELECT complete_read_at_ms FROM usage_current WHERE account_id=?) WHERE account_id=? AND cycle_state='HELD'", started, started+weeklyCycleMS, account.ID, account.ID)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed > 0 {
				if _, err := db.ExecContext(ctx, "UPDATE weekly_cycle_state SET cadence_anchor_at_ms=?,state_version=state_version+1 WHERE singleton_id=1", started); err != nil {
					return err
				}
			}
		}
		_, err := db.ExecContext(ctx, "UPDATE account_state SET last_routed_at_ms=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", now, now, account.ID)
		return err
	})
}

func (r Router) accounts(ctx context.Context, keyID string, now int64) ([]Account, error) {
	_ = r.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "DELETE FROM account_exclusions WHERE expires_at_ms<=?", now)
		return err
	})
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT a.account_id,a.public_token,a.display_name,COALESCE(a.workspace_constraint,''),a.enabled,a.created_at_ms,s.auth_state,s.worker_state,s.upstream_email,s.upstream_plan,u.complete_read_at_ms,u.short_used_percent_raw,u.short_resets_at_s,u.weekly_used_percent_raw,u.weekly_resets_at_s,e.expires_at_ms,COALESCE(p.cycle_state,CASE WHEN u.weekly_used_percent_raw>0 AND u.weekly_resets_at_s*1000>? THEN 'IN_CYCLE' ELSE 'HELD' END),p.weekly_started_at_ms,rr.expires_at_ms,q.retry_at_ms FROM accounts a JOIN account_state s USING(account_id) JOIN credential_bundles b ON b.account_id=a.account_id AND b.state='ACTIVE' LEFT JOIN account_quota_blocks q USING(account_id) LEFT JOIN usage_current u USING(account_id) LEFT JOIN window_pulse_state p USING(account_id) LEFT JOIN account_exclusions e ON e.account_id=a.account_id AND e.key_id=? LEFT JOIN relay_reservations rr ON rr.account_id=a.account_id AND rr.expires_at_ms>? WHERE a.deleted_at_ms IS NULL AND a.enabled=1 AND s.auth_state='VERIFIED' AND s.worker_state IN('STOPPED','CREDENTIAL_IN_USE') AND `+auth.AccountScopeSQL+` ORDER BY a.created_at_ms,a.account_id`, now, keyID, now, sql.Named("scope_key_id", keyID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var account Account
		var enabled int
		if err := rows.Scan(&account.ID, &account.PublicID, &account.DisplayName, &account.Workspace, &enabled, &account.CreatedAtMS, &account.AuthState, &account.WorkerState, &account.Email, &account.Plan, &account.UsageReadAt, &account.ShortUsed, &account.ShortReset, &account.WeeklyUsed, &account.WeeklyReset, &account.ExcludedUntil, &account.CycleState, &account.CycleStarted, &account.RelayReservedUntil, &account.QuotaBlockedUntil); err != nil {
			return nil, err
		}
		account.Enabled = enabled == 1
		result = append(result, account)
	}
	return result, rows.Err()
}

func exhausted(a Account, now int64) bool {
	return quotaBlocked(a, now) || a.ShortUsed.Valid && a.ShortUsed.Int64 >= 100 && (!a.ShortReset.Valid || a.ShortReset.Int64*1000 > now) || a.WeeklyUsed.Valid && a.WeeklyUsed.Int64 >= 100 && (!a.WeeklyReset.Valid || a.WeeklyReset.Int64*1000 > now)
}

func quotaBlocked(a Account, now int64) bool {
	return a.QuotaBlockedUntil.Valid && a.QuotaBlockedUntil.Int64 > now
}

// A quota rejection is authoritative across every client, even when cached
// usage percentages say there is capacity. The first known reset allows a probe.
func (r Router) blockQuota(ctx context.Context, account Account, now int64) error {
	retry := int64(math.MaxInt64)
	for _, reset := range []sql.NullInt64{account.ShortReset, account.WeeklyReset} {
		if reset.Valid && reset.Int64 > now/1000 && reset.Int64*1000 < retry {
			retry = reset.Int64 * 1000
		}
	}
	err := r.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, `INSERT INTO account_quota_blocks(account_id,blocked_at_ms,retry_at_ms) VALUES(?,?,?)
			ON CONFLICT(account_id) DO UPDATE SET blocked_at_ms=excluded.blocked_at_ms,retry_at_ms=excluded.retry_at_ms
			WHERE excluded.blocked_at_ms>=account_quota_blocks.blocked_at_ms`, account.ID, now, retry)
		return err
	})
	if err == nil && r.Service.Events != nil {
		r.Service.Events.Publish("account.updated", map[string]any{"resource_id": account.PublicID, "state": "QUOTA_BLOCKED"})
	}
	return err
}
func futureReset(value sql.NullInt64, now int64) int64 {
	if value.Valid && value.Int64*1000 > now {
		return value.Int64
	}
	return math.MaxInt64
}
func rankLess(a, b Account, preferred string, now int64) bool {
	aw, bw := futureReset(a.WeeklyReset, now), futureReset(b.WeeklyReset, now)
	if aw != bw {
		return aw < bw
	}
	as, bs := futureReset(a.ShortReset, now), futureReset(b.ShortReset, now)
	if as != bs {
		return as < bs
	}
	ap, bp := a.PublicID != preferred, b.PublicID != preferred
	if ap != bp {
		return !ap
	}
	if a.CreatedAtMS != b.CreatedAtMS {
		return a.CreatedAtMS < b.CreatedAtMS
	}
	return a.ID < b.ID
}
func resetPointer(value sql.NullInt64, now int64) *int64 {
	if !value.Valid || value.Int64*1000 <= now {
		return nil
	}
	result := value.Int64 * 1000
	return &result
}
func remainingPointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := int64(100) - value.Int64
	if result < 0 {
		result = 0
	}
	if result > 100 {
		result = 100
	}
	return &result
}
func routeLease(account Account, lease Lease, now int64) RouteLease {
	return RouteLease{
		AccountID: account.PublicID, AccountLabel: account.DisplayName, AccessToken: lease.AccessToken,
		ChatGPTAccountID: lease.ChatGPTAccountID,
		ExpiresAtMS:      lease.ExpiresAtMS, UsageSnapshotAtMS: account.UsageReadAt.Int64,
		ShortRemainingPercent: remainingPointer(account.ShortUsed), WeeklyRemainingPercent: remainingPointer(account.WeeklyUsed),
		ShortResetsAtMS: resetPointer(account.ShortReset, now), WeeklyResetsAtMS: resetPointer(account.WeeklyReset, now),
	}
}

func accountReset(account Account, now int64) int64 {
	var values []int64
	if account.ShortUsed.Valid && account.ShortUsed.Int64 >= 100 && account.ShortReset.Valid && account.ShortReset.Int64*1000 > now {
		values = append(values, account.ShortReset.Int64*1000)
	}
	if account.WeeklyUsed.Valid && account.WeeklyUsed.Int64 >= 100 && account.WeeklyReset.Valid && account.WeeklyReset.Int64*1000 > now {
		values = append(values, account.WeeklyReset.Int64*1000)
	}
	var result int64
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}
func nextRetry(accounts []Account, now int64) int64 {
	result := int64(math.MaxInt64)
	for _, account := range accounts {
		for _, candidate := range []int64{accountReset(account, now), nullableTime(account.ExcludedUntil), nullableTime(account.QuotaBlockedUntil)} {
			if candidate > now && candidate < result {
				result = candidate
			}
		}
	}
	if result == math.MaxInt64 {
		return 0
	}
	return result
}
func nullableTime(value sql.NullInt64) int64 {
	if value.Valid {
		return value.Int64
	}
	return 0
}
func (r Router) exclude(ctx context.Context, keyID string, account Account, failure string, now int64) error {
	expires := accountReset(account, now)
	if expires == 0 {
		expires = now + 300000
		if failure == "rate_limit" {
			expires = now + 60000
		}
	}
	return r.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "INSERT INTO account_exclusions VALUES(?,?,?,?,?) ON CONFLICT(key_id,account_id) DO UPDATE SET failure_kind=excluded.failure_kind,expires_at_ms=excluded.expires_at_ms,created_at_ms=excluded.created_at_ms", keyID, account.ID, failure, expires, now)
		return err
	})
}
func (r Router) markAuth(ctx context.Context, accountID string, now int64) error {
	return r.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "UPDATE account_state SET auth_state='AUTH_REQUIRED',overall_state='ACTION_REQUIRED',last_error_code='CODEX_AUTH_REQUIRED',last_error_summary='Client request authentication failed',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?", now, accountID)
		return err
	})
}

// Temporary runtime, storage, and transport failures do not prove invalid auth.
func (r Router) markLeaseAuthFailure(ctx context.Context, accountID string, failure error, now int64) error {
	var typed *core.Error
	if errors.As(failure, &typed) && typed.Code == "CODEX_AUTH_REQUIRED" {
		return r.markAuth(ctx, accountID, now)
	}
	return nil
}
