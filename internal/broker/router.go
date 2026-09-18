package broker

import (
	"context"
	"database/sql"
	"math"
	"sort"

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
			_, _ = r.Service.Refresh(ctx, failed.PublicID, "CLIENT_FAILURE")
		}
		if request.FailureKind == "auth" {
			if lease, error := r.Service.Lease(ctx, *failed, now, true); error == nil {
				result := routeLease(*failed, lease, now)
				return &result, nil, nil
			}
			_ = r.markAuth(ctx, failed.ID, now)
		}
		if err := r.exclude(ctx, keyID, *failed, request.FailureKind, now); err != nil {
			return nil, nil, err
		}
		accounts, err = r.accounts(ctx, keyID, now)
		if err != nil {
			return nil, nil, err
		}
	}
	var usable []Account
	for _, account := range accounts {
		if !exhausted(account, now) && (!account.ExcludedUntil.Valid || account.ExcludedUntil.Int64 <= now) {
			usable = append(usable, account)
		}
	}
	if len(usable) > 0 {
		sort.Slice(usable, func(i, j int) bool { return rankLess(usable[i], usable[j], request.PreferredAccountID, now) })
		selected := usable[0]
		lease, err := r.Service.Lease(ctx, selected, now, false)
		if err != nil {
			return nil, nil, err
		}
		result := routeLease(selected, lease, now)
		return &result, nil, nil
	}
	retry := nextRetry(accounts, now)
	if retry == 0 {
		return nil, nil, core.NewError("POOL_RESET_UNKNOWN", "No routable account has a reliable retry time", 503)
	}
	retry += int64(r.PaddingSeconds) * 1000
	wait := PoolWait{retry, int(math.Ceil(float64(retry-now) / 1000))}
	return nil, &wait, nil
}

func (r Router) accounts(ctx context.Context, keyID string, now int64) ([]Account, error) {
	_ = r.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "DELETE FROM account_exclusions WHERE expires_at_ms<=?", now)
		return err
	})
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT a.account_id,a.public_token,a.display_name,COALESCE(a.workspace_constraint,''),a.enabled,a.created_at_ms,s.auth_state,s.worker_state,s.upstream_email,s.upstream_plan,u.short_used_percent_raw,u.short_resets_at_s,u.weekly_used_percent_raw,u.weekly_resets_at_s,e.expires_at_ms FROM accounts a JOIN account_state s USING(account_id) JOIN credential_bundles b ON b.account_id=a.account_id AND b.state='ACTIVE' LEFT JOIN usage_current u USING(account_id) LEFT JOIN account_exclusions e ON e.account_id=a.account_id AND e.key_id=? WHERE a.deleted_at_ms IS NULL AND a.enabled=1 AND s.auth_state='VERIFIED' AND s.worker_state IN('STOPPED','CREDENTIAL_IN_USE') ORDER BY a.created_at_ms,a.account_id`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var account Account
		var enabled int
		if err := rows.Scan(&account.ID, &account.PublicID, &account.DisplayName, &account.Workspace, &enabled, &account.CreatedAtMS, &account.AuthState, &account.WorkerState, &account.Email, &account.Plan, &account.ShortUsed, &account.ShortReset, &account.WeeklyUsed, &account.WeeklyReset, &account.ExcludedUntil); err != nil {
			return nil, err
		}
		account.Enabled = enabled == 1
		result = append(result, account)
	}
	return result, rows.Err()
}

func exhausted(a Account, now int64) bool {
	return a.ShortUsed.Valid && a.ShortUsed.Int64 >= 100 && (!a.ShortReset.Valid || a.ShortReset.Int64*1000 > now) || a.WeeklyUsed.Valid && a.WeeklyUsed.Int64 >= 100 && (!a.WeeklyReset.Valid || a.WeeklyReset.Int64*1000 > now)
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
	return RouteLease{account.PublicID, account.DisplayName, lease.AccessToken, lease.ChatGPTAccountID, lease.ExpiresAtMS, remainingPointer(account.ShortUsed), remainingPointer(account.WeeklyUsed), resetPointer(account.ShortReset, now), resetPointer(account.WeeklyReset, now)}
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
		for _, candidate := range []int64{accountReset(account, now), nullableTime(account.ExcludedUntil)} {
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
