package broker

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

type Lease struct {
	ChatGPTAccountID, AccessToken string
	ExpiresAtMS                   int64
}

func (s *Service) credentialLock(accountID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.credentialLocks[accountID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.credentialLocks[accountID] = lock
	}
	return lock
}

func (s *Service) bundle(ctx context.Context, accountID, state string) (vault.Payload, error) {
	var envelope vault.Envelope
	err := s.Store.DB.QueryRowContext(ctx, "SELECT bundle_id,account_id,key_id,nonce,ciphertext,aad,payload_schema_version,envelope_version FROM credential_bundles WHERE account_id=? AND state=?", accountID, state).Scan(&envelope.BundleID, &envelope.AccountID, &envelope.KeyID, &envelope.Nonce, &envelope.Ciphertext, &envelope.AAD, &envelope.PayloadSchemaVersion, &envelope.EnvelopeVersion)
	if err == sql.ErrNoRows {
		return vault.Payload{}, core.NewError("AUTH_REQUIRED", "The account must be authenticated first", 409)
	}
	if err != nil {
		return vault.Payload{}, err
	}
	return s.Vault.Decrypt(envelope)
}

func (s *Service) replaceActive(ctx context.Context, accountID string, payload vault.Payload) error {
	envelope, err := s.Vault.Encrypt(accountID, payload)
	if err != nil {
		return err
	}
	now := core.NowMS()
	return s.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "UPDATE credential_bundles SET state='RETIRED',retired_at_ms=? WHERE account_id=? AND state='ACTIVE'", now, accountID); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", envelope.BundleID, envelope.AccountID, "ACTIVE", envelope.EnvelopeVersion, envelope.PayloadSchemaVersion, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, envelope.AAD, s.Config.CodexVersion, now, now, nil)
		return err
	})
}

func (s *Service) installBundle(ctx context.Context, accountID, state string, payload vault.Payload) error {
	envelope, err := s.Vault.Encrypt(accountID, payload)
	if err != nil {
		return err
	}
	now := core.NowMS()
	return s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", envelope.BundleID, envelope.AccountID, state, envelope.EnvelopeVersion, envelope.PayloadSchemaVersion, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, envelope.AAD, s.Config.CodexVersion, now, nullablePromoted(state, now), nil)
		return err
	})
}
func nullablePromoted(state string, now int64) any {
	if state == "ACTIVE" {
		return now
	}
	return nil
}

func (s *Service) setCredentialUse(ctx context.Context, accountID string, started, safe bool) error {
	now := core.NowMS()
	return s.Store.Write(ctx, func(db store.Executor) error {
		if started {
			result, err := db.ExecContext(ctx, "UPDATE account_state SET worker_state='CREDENTIAL_IN_USE',updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND worker_state='STOPPED'", now, accountID)
			if err != nil {
				return err
			}
			changed, _ := result.RowsAffected()
			if changed != 1 {
				return core.NewError("CREDENTIAL_RUNTIME_BLOCKED", "Credential recovery is required before another runtime can start", 409)
			}
			return nil
		}
		if safe {
			_, err := db.ExecContext(ctx, "UPDATE account_state SET worker_state='STOPPED',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?", now, accountID)
			return err
		}
		_, err := db.ExecContext(ctx, "UPDATE account_state SET worker_state='CREDENTIAL_QUARANTINED',auth_state='AUTH_REQUIRED',overall_state='ERROR',last_error_code='CREDENTIAL_CHECKPOINT_FAILED',last_error_summary='Credential recovery is required',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?", now, accountID)
		return err
	})
}

func (s *Service) managed(ctx context.Context, account Account, call func(context.Context, *codex.Runtime) (map[string]any, error)) (map[string]any, error) {
	lock := s.credentialLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	return s.managedLocked(ctx, account, call)
}

func (s *Service) managedLocked(ctx context.Context, account Account, call func(context.Context, *codex.Runtime) (map[string]any, error)) (map[string]any, error) {
	source, err := s.bundle(ctx, account.ID, "ACTIVE")
	if err != nil {
		return nil, err
	}
	fingerprint, err := s.Vault.AuthFingerprint(source)
	if err != nil {
		return nil, err
	}
	if err = s.setCredentialUse(context.WithoutCancel(ctx), account.ID, true, true); err != nil {
		return nil, err
	}
	workspace := pointerString(account.Workspace)
	runtime, err := s.Runtime.Start(ctx, account.ID, &source, workspace)
	if err != nil {
		_ = s.setCredentialUse(context.WithoutCancel(ctx), account.ID, false, true)
		return nil, err
	}
	result, primary := call(ctx, runtime)
	critical := context.WithoutCancel(ctx)
	runtime.Mu.Lock()
	closeErr := runtime.Client.Close()
	captured, captureErr := s.Vault.Capture(runtime.CodexHome(), s.Config.CodexVersion, workspace)
	runtime.Mu.Unlock()
	checkpointErr := errors.Join(closeErr, captureErr)
	if checkpointErr == nil {
		newFingerprint, fingerprintErr := s.Vault.AuthFingerprint(captured)
		checkpointErr = fingerprintErr
		if checkpointErr == nil && newFingerprint != fingerprint {
			checkpointErr = s.replaceActive(critical, account.ID, captured)
		}
	}
	if checkpointErr == nil {
		checkpointErr = s.Runtime.Discard(runtime)
	} else {
		s.Runtime.Preserve(runtime)
	}
	stateErr := s.setCredentialUse(critical, account.ID, false, checkpointErr == nil)
	checkpointErr = errors.Join(checkpointErr, stateErr)
	if checkpointErr != nil {
		_ = s.OpenIncident(critical, account.ID, "credential_checkpoint", "ERROR", "Codex credential state could not be safely checkpointed")
		return nil, core.NewError("CREDENTIAL_CHECKPOINT_FAILED", "Codex credential state could not be safely checkpointed", 503)
	}
	if primary != nil {
		return nil, primary
	}
	return result, nil
}

func pointerString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (s *Service) Lease(ctx context.Context, account Account, now int64, force bool) (Lease, error) {
	lock := s.credentialLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	var worker string
	if err := s.Store.DB.QueryRowContext(ctx, "SELECT worker_state FROM account_state WHERE account_id=?", account.ID).Scan(&worker); err != nil {
		return Lease{}, err
	}
	if worker != "STOPPED" {
		return Lease{}, core.NewError("CREDENTIAL_RUNTIME_BLOCKED", "Credential recovery is required before another runtime can start", 409)
	}
	payload, err := s.bundle(ctx, account.ID, "ACTIVE")
	if err == nil {
		leaseErr := error(nil)
		var lease Lease
		lease, leaseErr = leaseFromPayload(s.Vault, payload)
		if force || leaseErr != nil || lease.ExpiresAtMS <= now+5*60*1000 {
			_, err = s.managedLocked(ctx, account, func(callCtx context.Context, runtime *codex.Runtime) (map[string]any, error) {
				return runtime.Adapter.Account(callCtx, false)
			})
			if err == nil {
				payload, err = s.bundle(ctx, account.ID, "ACTIVE")
			}
		}
	}
	if err != nil {
		return Lease{}, err
	}
	lease, err := leaseFromPayload(s.Vault, payload)
	if err != nil {
		return Lease{}, err
	}
	if lease.ExpiresAtMS <= now {
		return Lease{}, core.NewError("CREDENTIAL_EXPIRED", "The active credential is expired", 503)
	}
	return lease, nil
}

func leaseFromPayload(secrets *vault.Vault, payload vault.Payload) (Lease, error) {
	content, err := secrets.AuthJSON(payload)
	if err != nil {
		return Lease{}, formatError()
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(content, &auth) != nil || auth.Tokens.AccessToken == "" {
		return Lease{}, formatError()
	}
	parts := splitToken(auth.Tokens.AccessToken)
	if len(parts) != 3 {
		return Lease{}, formatError()
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Lease{}, formatError()
	}
	var claims struct {
		Exp  int64 `json:"exp"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(body, &claims) != nil || claims.Exp <= 0 || claims.Auth.AccountID == "" {
		return Lease{}, formatError()
	}
	return Lease{claims.Auth.AccountID, auth.Tokens.AccessToken, claims.Exp * 1000}, nil
}
func formatError() error {
	return core.NewError("CREDENTIAL_FORMAT_INVALID", "The active credential cannot be leased", 503)
}
func splitToken(value string) []string {
	var result []string
	start := 0
	for i, c := range value {
		if c == '.' {
			result = append(result, value[start:i])
			start = i + 1
		}
	}
	return append(result, value[start:])
}

func (s *Service) ExportAuth(ctx context.Context, public string) ([]byte, error) {
	account, err := s.account(ctx, public)
	if err != nil {
		return nil, err
	}
	payload, err := s.bundle(ctx, account.ID, "EXPORT")
	if err != nil {
		return nil, core.NewError("AUTH_EXPORT_UNAVAILABLE", "No downloadable auth.json is available", 409)
	}
	return s.Vault.AuthJSON(payload)
}

func (s *Service) Refresh(ctx context.Context, public, trigger string) (string, error) {
	account, err := s.account(ctx, public)
	if err != nil {
		return "", err
	}
	if account.AuthState != "VERIFIED" || account.WorkerState != "STOPPED" {
		return "", core.NewError("USAGE_REFRESH_NOT_ELIGIBLE", "Credential recovery or reauthentication is required before usage refresh", 409)
	}
	operation, created := "", false
	err = s.Store.Write(ctx, func(db store.Executor) error {
		err := db.QueryRowContext(ctx, "SELECT operation_id FROM operations WHERE account_id=? AND kind='usage.refresh' AND state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED') ORDER BY created_at_ms LIMIT 1", account.ID).Scan(&operation)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		operation, created = core.NewID(), true
		_, err = db.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", operation, account.ID, "usage.refresh", trigger, "QUEUED", nil, nil, nil, nil, nil, core.NowMS(), nil, nil, nil, nil, 1)
		return err
	})
	if err != nil {
		return "", err
	}
	if created {
		if !s.runAsync(func() { s.runRefresh(s.workerCtx, account, operation) }) {
			_ = s.FailOperation(context.Background(), operation, "SERVICE_STOPPING", "Service is stopping")
		}
	}
	return operation, nil
}

func (s *Service) runRefresh(ctx context.Context, account Account, operation string) {
	start := time.Now()
	_ = s.OperationState(ctx, operation, "RUNNING", "READING_USAGE", "Reading complete rate limits")
	select {
	case s.usageSlots <- struct{}{}:
		defer func() { <-s.usageSlots }()
	case <-ctx.Done():
		return
	}
	raw, err := s.managed(ctx, account, func(callCtx context.Context, runtime *codex.Runtime) (map[string]any, error) {
		return runtime.Adapter.RateLimits(callCtx)
	})
	if err != nil {
		_ = s.recordUsageFailure(ctx, account, operation, start, err)
		s.refreshProfile(ctx, account)
		return
	}
	if err := s.commitUsage(ctx, account, operation, raw, time.Since(start), "Usage refreshed", nil); err != nil {
		return
	}
	s.refreshProfile(ctx, account)
}

func (s *Service) refreshProfile(ctx context.Context, account Account) {
	now := core.NowMS()
	lease, err := s.Lease(ctx, account, now, false)
	if err != nil {
		_ = s.recordProfileFailure(ctx, account, err)
		return
	}
	profile, err := codex.FetchProfile(ctx, codex.ProfileURL, lease.AccessToken, lease.ChatGPTAccountID)
	if err != nil {
		_ = s.recordProfileFailure(ctx, account, err)
		return
	}
	_ = s.commitProfile(ctx, account, profile)
}

func (s *Service) commitProfile(ctx context.Context, account Account, profile codex.AccountProfile) error {
	now := core.NowMS()
	daily, _ := json.Marshal(profile.Stats.DailyUsageBuckets)
	invocations, _ := json.Marshal(profile.Stats.TopInvocations)
	var displayName, username, statsAsOf, statsError any
	if profile.Profile != nil {
		displayName, username = pointerAnyString(profile.Profile.DisplayName), pointerAnyString(profile.Profile.Username)
	}
	if profile.Metadata != nil {
		statsAsOf, statsError = pointerAnyString(profile.Metadata.StatsAsOf), pointerAnyString(profile.Metadata.StatsError)
	}
	err := s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, `INSERT INTO account_profiles(account_id) VALUES(?) ON CONFLICT(account_id) DO NOTHING`, account.ID)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `UPDATE account_profiles SET profile_display_name=?,username=?,stats_as_of=?,stats_error=?,lifetime_tokens=?,peak_daily_tokens=?,longest_running_turn_sec=?,current_streak_days=?,longest_streak_days=?,daily_usage_buckets_json=?,fast_mode_usage_percentage=?,most_used_reasoning_effort=?,most_used_reasoning_effort_percentage=?,unique_skills_used=?,total_skills_used=?,total_threads=?,top_invocations_json=?,complete_read_at_ms=?,last_attempt_at_ms=?,stale=0,last_error_code=NULL,last_error_summary=NULL,state_version=state_version+1 WHERE account_id=?`, displayName, username, statsAsOf, statsError, pointerAny(profile.Stats.LifetimeTokens), pointerAny(profile.Stats.PeakDailyTokens), pointerAny(profile.Stats.LongestRunningTurnSec), pointerAny(profile.Stats.CurrentStreakDays), pointerAny(profile.Stats.LongestStreakDays), string(daily), pointerAnyFloat(profile.Stats.FastModeUsagePercentage), pointerAnyString(profile.Stats.MostUsedReasoningEffort), pointerAnyFloat(profile.Stats.MostUsedReasoningEffortPercentage), pointerAny(profile.Stats.UniqueSkillsUsed), pointerAny(profile.Stats.TotalSkillsUsed), pointerAny(profile.Stats.TotalThreads), string(invocations), now, now, account.ID)
		return err
	})
	if err == nil && s.Events != nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": account.PublicID, "profile": "FRESH"})
	}
	return err
}

func (s *Service) recordProfileFailure(ctx context.Context, account Account, failure error) error {
	summary := truncateSummary(failure.Error())
	if redacted, ok := logbook.Redact(summary).(string); ok {
		summary = redacted
	}
	err := s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, `INSERT INTO account_profiles(account_id,last_attempt_at_ms,stale,last_error_code,last_error_summary) VALUES(?,?,1,'PROFILE_REFRESH_FAILED',?) ON CONFLICT(account_id) DO UPDATE SET last_attempt_at_ms=excluded.last_attempt_at_ms,stale=1,last_error_code=excluded.last_error_code,last_error_summary=excluded.last_error_summary,state_version=account_profiles.state_version+1`, account.ID, core.NowMS(), summary)
		return err
	})
	if err == nil && s.Events != nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": account.PublicID, "profile": "STALE"})
	}
	return err
}

func pointerAnyString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func pointerAnyFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *Service) commitUsage(ctx context.Context, account Account, operation string, raw map[string]any, duration time.Duration, summary string, pulseNext *int64) error {
	usage := NormalizeUsage(raw)
	now := core.NowMS()
	snapshot := core.NewID()
	evidence, _ := json.Marshal(logbook.Redact(raw))
	shape, _ := json.Marshal(map[string]any{"window_count": 2 + len(usage.Others), "short_duration_minutes": windowDuration(usage.Short), "short_resets_at_s": windowReset(usage.Short), "weekly_duration_minutes": windowDuration(usage.Weekly), "weekly_resets_at_s": windowReset(usage.Weekly)})
	err := s.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "INSERT INTO usage_snapshots VALUES(?,?,?,?,?,?,?,?,?,?,?)", snapshot, account.ID, now-duration.Milliseconds(), now, 1, nullString(usage.SelectedLimitID), string(evidence), string(shape), nil, nil, duration.Milliseconds()); err != nil {
			return err
		}
		values := func(window *Window) (any, any, any, any, int) {
			if window == nil {
				return nil, nil, nil, nil, 0
			}
			anomaly := 0
			if window.UsedPercent != nil && (*window.UsedPercent < 0 || *window.UsedPercent > 100) {
				anomaly = 1
			}
			return window.Slot, pointerAny(window.UsedPercent), pointerAny(window.DurationMinutes), pointerAny(window.ResetsAtS), anomaly
		}
		ss, su, sd, sr, sa := values(usage.Short)
		ws, wu, wd, wr, wa := values(usage.Weekly)
		if _, err := db.ExecContext(ctx, `UPDATE usage_current SET snapshot_id=?,selected_limit_id=?,short_raw_slot=?,short_used_percent_raw=?,short_duration_minutes=?,short_resets_at_s=?,short_anomaly=?,weekly_raw_slot=?,weekly_used_percent_raw=?,weekly_duration_minutes=?,weekly_resets_at_s=?,weekly_anomaly=?,complete_read_at_ms=?,last_attempt_at_ms=?,stale=0,last_error_code=NULL,last_error_summary=NULL,source='APP_SERVER',state_version=state_version+1 WHERE account_id=?`, snapshot, nullString(usage.SelectedLimitID), ss, su, sd, sr, sa, ws, wu, wd, wr, wa, now, now, account.ID); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "UPDATE account_state SET usage_state='FRESH',overall_state=CASE WHEN auth_state='VERIFIED' THEN 'HEALTHY' ELSE overall_state END,last_error_code=NULL,last_error_summary=NULL,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", now, account.ID); err != nil {
			return err
		}
		if pulseNext != nil {
			if _, err := db.ExecContext(ctx, "UPDATE window_pulse_state SET last_success_at_ms=?,next_pulse_at_ms=?,last_error_code=NULL WHERE account_id=?", now, *pulseNext, account.ID); err != nil {
				return err
			}
		}
		if s.Config.WindowPulseEnabled {
			if err := observeWeeklyCycle(ctx, db, account.ID, usage, now); err != nil {
				return err
			}
		}
		_, err := db.ExecContext(ctx, "UPDATE operations SET state='SUCCEEDED',progress_code='COMPLETE',progress_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?", summary, now, operation)
		return err
	})
	if err == nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": account.PublicID, "state": "FRESH"})
	}
	return err
}

func windowDuration(window *Window) any {
	if window == nil {
		return nil
	}
	return pointerAny(window.DurationMinutes)
}
func windowReset(window *Window) any {
	if window == nil {
		return nil
	}
	return pointerAny(window.ResetsAtS)
}

func (s *Service) recordUsageFailure(ctx context.Context, account Account, operation string, started time.Time, failure error) error {
	code := "USAGE_REFRESH_FAILED"
	summary := "Usage could not be refreshed"
	var brokerError *core.Error
	if errors.As(failure, &brokerError) {
		code = brokerError.Code
		if redacted, ok := logbook.Redact(brokerError.Detail).(string); ok {
			summary = redacted
		}
	}
	summary = truncateSummary(summary)
	now := core.NowMS()
	duration := time.Since(started).Milliseconds()
	authFailure := code == "CODEX_AUTH_REQUIRED"
	checkpointFailure := code == "CREDENTIAL_CHECKPOINT_FAILED"
	actionRequired := code == "AUTH_IDENTITY_MISMATCH" || code == "WORKSPACE_MISMATCH"
	err := s.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "INSERT INTO usage_snapshots VALUES(?,?,?,?,?,?,?,?,?,?,?)", core.NewID(), account.ID, now-duration, nil, 0, nil, nil, nil, code, summary, duration); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "UPDATE usage_current SET last_attempt_at_ms=?,stale=1,last_error_code=?,last_error_summary=?,state_version=state_version+1 WHERE account_id=?", now, code, summary, account.ID); err != nil {
			return err
		}
		overall := "WARNING"
		if actionRequired || authFailure {
			overall = "ACTION_REQUIRED"
		}
		if checkpointFailure {
			overall = "ERROR"
		}
		if authFailure || checkpointFailure {
			_, err := db.ExecContext(ctx, "UPDATE account_state SET auth_state='AUTH_REQUIRED',usage_state='STALE',overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", overall, code, summary, now, account.ID)
			return err
		}
		_, err := db.ExecContext(ctx, "UPDATE account_state SET usage_state='STALE',overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", overall, code, summary, now, account.ID)
		return err
	})
	if err != nil {
		return err
	}
	if authFailure {
		_ = s.OpenIncident(ctx, account.ID, "authentication_failed", "ERROR", "Codex authentication must be renewed")
	}
	if err = s.FailOperation(ctx, operation, code, summary); err == nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": account.PublicID, "state": "WARNING"})
	}
	return err
}

func pointerAny(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
func (s *Service) QueuePulses(ctx context.Context) error {
	now := core.NowMS()
	if err := s.reconcileCycle(ctx, now); err != nil {
		return err
	}
	plan, err := s.cyclePlan(ctx, now)
	if err != nil {
		return err
	}
	releaseID := ""
	if len(plan) > 0 && plan[0].state == "HELD" && plan[0].releaseAtMS <= now {
		releaseID = plan[0].accountID
	}
	retryBefore := now - int64(s.Config.WindowPulseRetrySeconds)*1000
	type claimed struct {
		public, operation string
		release           bool
	}
	var claims []claimed
	err = s.Store.Write(ctx, func(db store.Executor) error {
		rows, err := db.QueryContext(ctx, `SELECT a.account_id,a.public_token,p.cycle_state FROM accounts a JOIN account_state s USING(account_id) JOIN usage_current u USING(account_id) JOIN window_pulse_state p USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state='STOPPED' AND ((a.account_id=? AND p.cycle_state='HELD') OR (p.cycle_state='IN_CYCLE' AND (p.next_pulse_at_ms<=? OR p.last_success_at_ms IS NULL))) AND p.last_attempt_at_ms<=? AND NOT (u.short_used_percent_raw>=100 AND u.short_resets_at_s*1000>?) AND NOT (u.weekly_used_percent_raw>=100 AND u.weekly_resets_at_s*1000>?) AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED'))`, releaseID, now, retryBefore, now, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		type due struct{ id, public, state string }
		var accounts []due
		for rows.Next() {
			var account due
			if err := rows.Scan(&account.id, &account.public, &account.state); err != nil {
				return err
			}
			accounts = append(accounts, account)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, account := range accounts {
			release := account.id == releaseID && account.state == "HELD"
			if release {
				result, err := db.ExecContext(ctx, "UPDATE window_pulse_state SET cycle_state='RELEASING' WHERE account_id=? AND cycle_state='HELD'", account.id)
				if err != nil {
					return err
				}
				if changed, _ := result.RowsAffected(); changed != 1 {
					continue
				}
			}
			operation := core.NewID()
			trigger := "SCHEDULED"
			if release {
				trigger = "CYCLE_RELEASE"
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", operation, account.id, "window.pulse", trigger, "QUEUED", nil, nil, nil, nil, nil, now, nil, nil, nil, nil, 1); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, "UPDATE window_pulse_state SET last_attempt_at_ms=?,last_error_code=NULL WHERE account_id=?", now, account.id); err != nil {
				return err
			}
			claims = append(claims, claimed{account.public, operation, release})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, claim := range claims {
		claim := claim
		if !s.runAsync(func() { s.runPulse(s.workerCtx, claim.public, claim.operation, claim.release) }) {
			_ = s.FailOperation(context.Background(), claim.operation, "SERVICE_STOPPING", "Service is stopping")
		}
	}
	return nil
}

func (s *Service) runPulse(ctx context.Context, public, operation string, cycleRelease bool) {
	account, err := s.account(ctx, public)
	if err != nil {
		_ = s.FailOperation(ctx, operation, "WINDOW_PULSE_FAILED", "Account unavailable")
		return
	}
	_ = s.OperationState(ctx, operation, "RUNNING", "PULSING_WINDOWS", "Keeping usage windows active")
	select {
	case s.pulseSlots <- struct{}{}:
		defer func() { <-s.pulseSlots }()
	case <-ctx.Done():
		return
	}
	started := time.Now()
	raw, err := s.managed(ctx, account, func(callCtx context.Context, runtime *codex.Runtime) (map[string]any, error) {
		return runtime.Adapter.PulseWindows(callCtx)
	})
	if err != nil {
		code := "WINDOW_PULSE_FAILED"
		var typed *core.Error
		if errors.As(err, &typed) {
			code = typed.Code
		}
		_ = s.Store.Write(ctx, func(db store.Executor) error {
			_, updateErr := db.ExecContext(ctx, "UPDATE window_pulse_state SET last_error_code=?,cycle_state=CASE WHEN ? THEN 'HELD' ELSE cycle_state END WHERE account_id=?", code, cycleRelease, account.ID)
			return updateErr
		})
		_ = s.recordUsageFailure(ctx, account, operation, started, err)
		return
	}
	usage := NormalizeUsage(raw)
	if cycleRelease && (usage.Weekly == nil || usage.Weekly.ResetsAtS == nil || *usage.Weekly.ResetsAtS*1000 <= core.NowMS()) {
		_ = s.Store.Write(ctx, func(db store.Executor) error {
			_, updateErr := db.ExecContext(ctx, "UPDATE window_pulse_state SET cycle_state='HELD',last_error_code='WINDOW_RESET_UNKNOWN' WHERE account_id=?", account.ID)
			return updateErr
		})
		_ = s.recordUsageFailure(ctx, account, operation, started, core.NewError("WINDOW_RESET_UNKNOWN", "The weekly window did not report a future reset", 503))
		return
	}
	var resets []int64
	for _, window := range []*Window{usage.Short, usage.Weekly} {
		if window != nil && window.ResetsAtS != nil && *window.ResetsAtS*1000 > core.NowMS() {
			resets = append(resets, *window.ResetsAtS*1000)
		}
	}
	next := core.NowMS() + int64(s.Config.WindowPulseRetrySeconds)*1000
	if len(resets) > 0 {
		next = resets[0]
	}
	for _, reset := range resets {
		if reset < next {
			next = reset
		}
	}
	_ = s.commitUsage(ctx, account, operation, raw, time.Since(started), "Usage windows kept active", &next)
}

func (s *Service) OpenIncident(ctx context.Context, accountID, kind, severity, summary string) error {
	if redacted, ok := logbook.Redact(summary).(string); ok {
		summary = redacted
	}
	summary = truncateSummary(summary)
	now := core.NowMS()
	id, opened := core.NewID(), false
	var details map[string]any
	err := s.Store.Write(ctx, func(db store.Executor) error {
		err := db.QueryRowContext(ctx, "SELECT incident_id FROM incidents WHERE scope_kind='account' AND scope_key=? AND problem_type=? AND state='OPEN'", accountID, kind).Scan(&id)
		if err == nil {
			if _, err = db.ExecContext(ctx, "UPDATE incidents SET occurrence_count=occurrence_count+1,last_seen_at_ms=?,summary=?,state_version=state_version+1 WHERE incident_id=?", now, summary, id); err != nil {
				return err
			}
		} else if err != sql.ErrNoRows {
			return err
		} else {
			opened = true
			if _, err = db.ExecContext(ctx, "INSERT INTO incidents VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, "account", accountID, kind, "OPEN", severity, summary, strings.ToUpper(kind), 1, now, now, nil, nil, 1); err != nil {
				return err
			}
		}
		var problem, level, text, name, public string
		var count, first, last int64
		var email, causeCode, causeSummary sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT i.problem_type,i.severity,i.summary,i.occurrence_count,i.opened_at_ms,i.last_seen_at_ms,a.display_name,s.upstream_email,a.public_token,COALESCE(s.last_error_code,i.current_error_code),COALESCE(s.last_error_summary,i.summary) FROM incidents i JOIN accounts a ON a.account_id=i.scope_key JOIN account_state s ON s.account_id=a.account_id WHERE i.incident_id=?`, id).Scan(&problem, &level, &text, &count, &first, &last, &name, &email, &public, &causeCode, &causeSummary); err != nil {
			return err
		}
		details = incidentWebhookData(id, problem, "OPEN", level, text, causeCode.String, causeSummary.String, count, first, last, name, email.String, public)
		return nil
	})
	if err != nil {
		return err
	}
	s.Events.Publish("incident.updated", map[string]any{"incident_id": id, "state": "OPEN"})
	if s.Webhooks != nil {
		event := "incident.updated"
		if opened {
			event = "incident.opened"
		}
		_, err = s.Webhooks.Emit(ctx, event, "account:"+fmt.Sprint(details["account_id"]), details, id, "")
	}
	return err
}

func (s *Service) ResolveIncident(ctx context.Context, accountID, kind string) error {
	now := core.NowMS()
	var id string
	var details map[string]any
	err := s.Store.Write(ctx, func(db store.Executor) error {
		var problem, level, summary, name, public string
		var count, first, last int64
		var email, causeCode sql.NullString
		err := db.QueryRowContext(ctx, `SELECT i.incident_id,i.problem_type,i.severity,i.summary,i.current_error_code,i.occurrence_count,i.opened_at_ms,i.last_seen_at_ms,a.display_name,s.upstream_email,a.public_token FROM incidents i JOIN accounts a ON a.account_id=i.scope_key JOIN account_state s ON s.account_id=a.account_id WHERE i.scope_kind='account' AND i.scope_key=? AND i.problem_type=? AND i.state='OPEN'`, accountID, kind).Scan(&id, &problem, &level, &summary, &causeCode, &count, &first, &last, &name, &email, &public)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, "UPDATE incidents SET state='RESOLVED',resolved_at_ms=?,resolution_reason='RECOVERED',state_version=state_version+1 WHERE incident_id=?", now, id); err != nil {
			return err
		}
		details = incidentWebhookData(id, problem, "RESOLVED", level, "Recovered from: "+summary, causeCode.String, summary, count, first, last, name, email.String, public)
		details["resolved_at_ms"] = now
		return nil
	})
	if err != nil || details == nil {
		return err
	}
	s.Events.Publish("incident.updated", map[string]any{"incident_id": id, "state": "RESOLVED"})
	if s.Webhooks != nil {
		_, err = s.Webhooks.Emit(ctx, "incident.resolved", "account:"+fmt.Sprint(details["account_id"]), details, id, "")
	}
	return err
}

func incidentWebhookData(id, kind, state, severity, summary, causeCode, causeSummary string, count, first, last int64, name, email, public string) map[string]any {
	reason := "Codex Broker detected an account condition that requires operator attention."
	action := "Open the account and Incidents pages, review the latest operation, and correct the reported condition."
	if kind == "authentication_failed" {
		reason = "Codex rejected or could not refresh the account credential, so routing cannot continue."
		action = "Open the account and use Replace or repair credentials with device code or browser sign-in."
	} else if kind == "credential_checkpoint" {
		reason = "Codex may have changed its credential, but Codex Broker could not safely persist the result."
		action = "Stop account activity and preserve the quarantined runtime before restarting or reauthenticating."
	}
	if state == "RESOLVED" {
		action = "No action required. Codex Broker closed this incident and resumed normal account processing."
	}
	return map[string]any{"incident_id": id, "problem_type": kind, "incident_status": state, "severity": severity, "summary": summary, "cause_code": emptyNil(causeCode), "cause_summary": emptyNil(causeSummary), "reason": reason, "recommended_action": action, "occurrence_count": count, "first_seen_at_ms": first, "last_seen_at_ms": last, "account_name": name, "account_email": emptyNil(email), "account_id": public}
}

func errorsText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

var _ = errorsText
