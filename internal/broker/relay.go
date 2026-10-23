package broker

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const relayPermitLifetime = 2 * time.Hour

func (a *Application) RelaySnapshot(ctx context.Context, relayID string) (relayprotocol.Snapshot, error) {
	now := time.Now().UnixMilli()
	reserveID, permit, permitExpiry, err := a.reserveForRelay(ctx, relayID, now)
	if err != nil {
		return relayprotocol.Snapshot{}, err
	}
	rows, err := a.Store.DB.QueryContext(ctx, "SELECT k.key_id,k.name,k.key_prefix,k.secret_hash,EXISTS(SELECT 1 FROM client_key_labels kl WHERE kl.key_id=k.key_id) FROM client_api_keys k WHERE k.revoked_at_ms IS NULL ORDER BY k.created_at_ms,k.key_id")
	if err != nil {
		return relayprotocol.Snapshot{}, err
	}
	var clients []relayprotocol.Client
	for rows.Next() {
		var item relayprotocol.Client
		if err := rows.Scan(&item.ID, &item.Name, &item.Prefix, &item.Digest, &item.Restricted); err != nil {
			rows.Close()
			return relayprotocol.Snapshot{}, err
		}
		clients = append(clients, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return relayprotocol.Snapshot{}, err
	}
	if err := rows.Close(); err != nil {
		return relayprotocol.Snapshot{}, err
	}
	for i := range clients {
		if clients[i].Restricted {
			clients[i].AllowedAccountIDs, err = a.ClientKeys.AllowedAccountIDs(ctx, clients[i].ID)
			if err != nil {
				return relayprotocol.Snapshot{}, err
			}
		}
	}
	accounts, err := a.Router.accounts(ctx, "", now)
	if err != nil {
		return relayprotocol.Snapshot{}, err
	}
	leasing := make([]relayprotocol.Lease, 0, len(accounts))
	for _, account := range accounts {
		if exhausted(account, now) {
			continue
		}
		reserve := account.ID == reserveID
		if account.CycleState != "IN_CYCLE" && !reserve {
			continue
		}
		lease, leaseErr := a.Service.Lease(ctx, account, now, false)
		if leaseErr != nil {
			continue
		}
		item := routeLease(account, lease, now)
		leasing = append(leasing, relayprotocol.Lease{
			Lease:      gateway.Lease{AccountID: item.AccountID, AccountLabel: item.AccountLabel, AccessToken: item.AccessToken, ChatGPTAccountID: item.ChatGPTAccountID, ExpiresAtMS: item.ExpiresAtMS, UsageSnapshotAtMS: item.UsageSnapshotAtMS, ShortRemainingPercent: item.ShortRemainingPercent, WeeklyRemainingPercent: item.WeeklyRemainingPercent, ShortResetsAtMS: item.ShortResetsAtMS, WeeklyResetsAtMS: item.WeeklyResetsAtMS},
			CycleState: account.CycleState, CreatedAtMS: account.CreatedAtMS, Reserve: reserve, PermitEpoch: permit, PermitExpiresAtMS: permitExpiry,
		})
	}
	var stateVersion int64
	_ = a.Store.DB.QueryRowContext(ctx, "SELECT COALESCE(sum(state_version),0) FROM account_state").Scan(&stateVersion)
	return relayprotocol.Snapshot{ProtocolVersion: relayprotocol.Version, SnapshotID: fmt.Sprintf("%d-%s", now, relayID), RelayID: relayID, PoolEpoch: fmt.Sprintf("%d", stateVersion), GeneratedAtMS: now, Clients: clients, Leases: leasing}, nil
}

func (a *Application) reserveForRelay(ctx context.Context, relayID string, now int64) (accountID, epoch string, expires int64, err error) {
	expires = now + relayPermitLifetime.Milliseconds()
	err = a.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "DELETE FROM relay_reservations WHERE expires_at_ms<=?", now); err != nil {
			return err
		}
		err := db.QueryRowContext(ctx, `SELECT rr.account_id,rr.permit_epoch FROM relay_reservations rr JOIN window_pulse_state p USING(account_id) JOIN accounts a USING(account_id) JOIN account_state s USING(account_id) JOIN usage_current u USING(account_id) JOIN credential_bundles b ON b.account_id=a.account_id AND b.state='ACTIVE' WHERE rr.relay_id=? AND rr.expires_at_ms>? AND p.cycle_state='HELD' AND p.idle_observation_count>=2 AND a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state='STOPPED' AND NOT EXISTS (SELECT 1 FROM account_quota_blocks q WHERE q.account_id=a.account_id AND q.retry_at_ms>?) AND NOT (COALESCE(u.short_used_percent_raw,0)>=100 AND COALESCE(u.short_resets_at_s*1000,?)>?) AND NOT (COALESCE(u.weekly_used_percent_raw,0)>=100 AND COALESCE(u.weekly_resets_at_s*1000,?)>?)`, relayID, now, now, now+1, now, now+1, now).Scan(&accountID, &epoch)
		if err == sql.ErrNoRows {
			err = db.QueryRowContext(ctx, `SELECT a.account_id FROM accounts a JOIN account_state s USING(account_id) JOIN usage_current u USING(account_id) JOIN window_pulse_state p USING(account_id) JOIN credential_bundles b ON b.account_id=a.account_id AND b.state='ACTIVE' LEFT JOIN relay_reservations rr USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state='STOPPED' AND p.cycle_state='HELD' AND p.idle_observation_count>=2 AND rr.account_id IS NULL AND NOT EXISTS (SELECT 1 FROM account_quota_blocks q WHERE q.account_id=a.account_id AND q.retry_at_ms>?) AND NOT (COALESCE(u.short_used_percent_raw,0)>=100 AND COALESCE(u.short_resets_at_s*1000,?)>?) AND NOT (COALESCE(u.weekly_used_percent_raw,0)>=100 AND COALESCE(u.weekly_resets_at_s*1000,?)>?) ORDER BY random() LIMIT 1`, now, now+1, now, now+1, now).Scan(&accountID)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		epoch = fmt.Sprintf("%d-%s", now, accountID)
		_, err = db.ExecContext(ctx, `INSERT INTO relay_reservations(relay_id,account_id,permit_epoch,expires_at_ms,updated_at_ms) VALUES(?,?,?,?,?) ON CONFLICT(relay_id) DO UPDATE SET account_id=excluded.account_id,permit_epoch=excluded.permit_epoch,expires_at_ms=excluded.expires_at_ms,updated_at_ms=excluded.updated_at_ms`, relayID, accountID, epoch, expires, now)
		return err
	})
	return
}

func (a *Application) RelayRoute(ctx context.Context, clientID string, request gateway.RouteRequest) (*gateway.Lease, *gateway.PoolWait, error) {
	var active bool
	if err := a.Store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_api_keys WHERE key_id=? AND revoked_at_ms IS NULL)", clientID).Scan(&active); err != nil {
		return nil, nil, err
	}
	if !active {
		return nil, nil, core.NewError("CLIENT_KEY_INVALID", "Client authentication failed", 401)
	}
	_, _ = a.Store.DB.ExecContext(ctx, "UPDATE client_api_keys SET last_used_at_ms=? WHERE key_id=?", time.Now().UnixMilli(), clientID)
	selected, wait, err := a.Router.Route(ctx, clientID, RouteRequest{SessionID: request.SessionID, TurnID: request.TurnID, PreferredAccountID: request.PreferredAccountID, FailedAccountID: request.FailedAccountID, FailureKind: request.FailureKind})
	if err != nil || wait != nil {
		if wait == nil {
			return nil, nil, err
		}
		return nil, &gateway.PoolWait{NextRetryAtMS: wait.NextRetryAtMS, RetryAfterSeconds: wait.RetryAfterSeconds}, nil
	}
	return &gateway.Lease{AccountID: selected.AccountID, AccountLabel: selected.AccountLabel, AccessToken: selected.AccessToken, ChatGPTAccountID: selected.ChatGPTAccountID, ExpiresAtMS: selected.ExpiresAtMS, UsageSnapshotAtMS: selected.UsageSnapshotAtMS, ShortRemainingPercent: selected.ShortRemainingPercent, WeeklyRemainingPercent: selected.WeeklyRemainingPercent, ShortResetsAtMS: selected.ShortResetsAtMS, WeeklyResetsAtMS: selected.WeeklyResetsAtMS}, nil, nil
}

func (r Router) ActivateRelayReserve(ctx context.Context, relayID, publicID, epoch string, occurredAt int64) error {
	var accountID string
	err := r.Store.Write(ctx, func(db store.Executor) error {
		var expires, issued int64
		if err := db.QueryRowContext(ctx, `SELECT rr.account_id,rr.expires_at_ms,rr.updated_at_ms FROM relay_reservations rr JOIN accounts a ON a.account_id=rr.account_id WHERE rr.relay_id=? AND rr.permit_epoch=? AND a.public_token=?`, relayID, epoch, publicID).Scan(&accountID, &expires, &issued); err != nil {
			if err != sql.ErrNoRows {
				return err
			}
			var state string
			checkErr := db.QueryRowContext(ctx, `SELECT a.account_id,p.cycle_state FROM accounts a JOIN window_pulse_state p USING(account_id) WHERE a.public_token=?`, publicID).Scan(&accountID, &state)
			if checkErr == sql.ErrNoRows {
				return nil
			}
			if checkErr != nil {
				return checkErr
			}
			if state == "IN_CYCLE" {
				return nil
			}
			issuedText, permitAccount, ok := strings.Cut(epoch, "-")
			issued, err = strconv.ParseInt(issuedText, 10, 64)
			if !ok || err != nil || permitAccount != accountID {
				return fmt.Errorf("relay reserve permit is invalid")
			}
			expires = issued + relayPermitLifetime.Milliseconds()
		}
		if occurredAt < issued-5*time.Minute.Milliseconds() || occurredAt > expires || occurredAt > time.Now().Add(5*time.Minute).UnixMilli() {
			return fmt.Errorf("relay reserve permit expired")
		}
		if _, err := db.ExecContext(ctx, "UPDATE window_pulse_state SET cycle_state='IN_CYCLE',weekly_started_at_ms=?,next_pulse_at_ms=?,idle_observation_count=0,last_error_code=NULL WHERE account_id=? AND cycle_state='HELD'", occurredAt, occurredAt+weeklyCycleMS, accountID); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "UPDATE account_state SET last_routed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=?", occurredAt, time.Now().UnixMilli(), accountID); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "DELETE FROM relay_reservations WHERE relay_id=? AND permit_epoch=?", relayID, epoch)
		return err
	})
	if err == nil && accountID != "" {
		_, _ = r.Service.Refresh(context.WithoutCancel(ctx), publicID, "RELAY_RECONCILE")
	}
	return err
}

func (r Router) ReportRelayFailure(ctx context.Context, keyID, publicID, kind string, occurredAt ...int64) error {
	now := time.Now().UnixMilli()
	var keyExists bool
	if err := r.Store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_api_keys WHERE key_id=?)", keyID).Scan(&keyExists); err != nil {
		return err
	}
	if !keyExists {
		return nil
	}
	accounts, err := r.accounts(ctx, keyID, now)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.PublicID != publicID {
			continue
		}
		if kind == "quota" {
			if len(occurredAt) > 0 {
				// A delayed offline event must not override a newer complete usage read.
				now = min(now, occurredAt[0])
				if account.UsageReadAt.Valid && account.UsageReadAt.Int64 > now {
					return nil
				}
			}
			return r.blockQuota(ctx, account, now)
		}
		if kind == "auth" {
			if _, leaseErr := r.Service.Lease(ctx, account, now, true); leaseErr == nil {
				return nil
			}
			_ = r.markAuth(ctx, account.ID, now)
		}
		return r.exclude(ctx, keyID, account, kind, now)
	}
	return nil
}
