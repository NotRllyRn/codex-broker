package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

type Service struct {
	Store            *store.Store
	Config           config.Config
	Vault            *vault.Vault
	Runtime          *codex.RuntimeManager
	Events           *events.Bus
	Webhooks         *webhook.Dispatcher
	mu               sync.Mutex
	browserLogin     sync.Mutex
	publicEnrollment sync.Mutex
	credentialLocks  map[string]*sync.Mutex
	interactions     map[string]Interaction
	loginCancels     map[string]context.CancelFunc
	workers          sync.WaitGroup
	closing          bool
	workerCtx        context.Context
	cancelWorkers    context.CancelFunc
	stop             chan struct{}
	pulseSlots       chan struct{}
	usageSlots       chan struct{}
	authSlots        chan struct{}
}

type Interaction struct {
	AttemptID              string
	SessionHash, NonceHash []byte
	Login                  codex.LoginInteraction
	Contract               *BrowserContract
	Consumed               bool
}

func NewService(database *store.Store, settings config.Config, secrets *vault.Vault, runtime *codex.RuntimeManager, bus *events.Bus) *Service {
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	return &Service{Store: database, Config: settings, Vault: secrets, Runtime: runtime, Events: bus, credentialLocks: map[string]*sync.Mutex{}, interactions: map[string]Interaction{}, loginCancels: map[string]context.CancelFunc{}, workerCtx: workerCtx, cancelWorkers: cancelWorkers, stop: make(chan struct{}), pulseSlots: make(chan struct{}, settings.WindowPulseConcurrency), usageSlots: make(chan struct{}, settings.UsageRefreshConcurrency), authSlots: make(chan struct{}, settings.AuthConcurrency)}
}

func (s *Service) Reconcile(ctx context.Context) error {
	now := core.NowMS()
	var checkpointAccounts []string
	err := s.Store.Write(ctx, func(db store.Executor) error {
		rows, err := db.QueryContext(ctx, "SELECT account_id FROM account_state WHERE worker_state IN('CREDENTIAL_IN_USE','CREDENTIAL_QUARANTINED')")
		if err != nil {
			return err
		}
		for rows.Next() {
			var accountID string
			if err := rows.Scan(&accountID); err != nil {
				rows.Close()
				return err
			}
			checkpointAccounts = append(checkpointAccounts, accountID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		statements := []struct {
			query string
			args  []any
		}{
			{"UPDATE account_state SET worker_state='CREDENTIAL_QUARANTINED',auth_state='AUTH_REQUIRED',overall_state='ERROR',last_error_code='CREDENTIAL_CHECKPOINT_UNCERTAIN',last_error_summary='Service restarted before credential checkpoint completion',updated_at_ms=?,state_version=state_version+1 WHERE worker_state IN('CREDENTIAL_IN_USE','CREDENTIAL_QUARANTINED')", []any{now}},
			{"UPDATE login_attempts SET state='RESTART_REQUIRED',error_code='LOGIN_RESTART_REQUIRED',updated_at_ms=? WHERE state NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED_RETRYABLE','FAILED_ACTION_REQUIRED','RESTART_REQUIRED','SUPERSEDED')", []any{now}},
			{"UPDATE operations SET state='FAILED',error_code='SERVICE_RESTARTED',error_summary='Operation was interrupted by service restart',completed_at_ms=?,state_version=state_version+1 WHERE state IN('QUEUED','RUNNING','WAITING_FOR_USER')", []any{now}},
			{"UPDATE public_enrollments SET state='FAILED',completed_at_ms=? WHERE state='ACTIVE' AND login_attempt_id IN (SELECT login_attempt_id FROM login_attempts WHERE state='RESTART_REQUIRED')", []any{now}},
			{"UPDATE accounts SET enabled=0,lifecycle_state='DELETED',deleted_at_ms=?,updated_at_ms=? WHERE account_id IN (SELECT account_id FROM public_enrollments WHERE state='FAILED') AND NOT EXISTS (SELECT 1 FROM credential_bundles b WHERE b.account_id=accounts.account_id AND b.state='ACTIVE')", []any{now, now}},
			{"UPDATE webhook_deliveries SET state='RETRY_SCHEDULED',lease_token=NULL,lease_expires_at_ms=NULL,next_attempt_at_ms=? WHERE state='LEASED'", []any{now}},
		}
		for _, statement := range statements {
			if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, accountID := range checkpointAccounts {
		if err := s.OpenIncident(ctx, accountID, "credential_checkpoint", "ERROR", "Service restarted before credential checkpoint completion"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) StartBackground(ctx context.Context) {
	s.runAsync(func() { s.usageLoop(ctx) })
	if s.Config.WindowPulseEnabled {
		s.runAsync(func() { s.pulseLoop(ctx) })
	}
	s.runAsync(func() { s.maintenanceLoop(ctx) })
}

func (s *Service) runAsync(work func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		work()
	}()
	return true
}

func (s *Service) Close() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	cancels := make([]context.CancelFunc, 0, len(s.loginCancels))
	for _, cancel := range s.loginCancels {
		cancels = append(cancels, cancel)
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	s.cancelWorkers()
	for _, cancel := range cancels {
		cancel()
	}
	s.workers.Wait()
	s.Runtime.Close()
}

func (s *Service) CreateAccount(ctx context.Context, name string, labels []string, workspace, method string) (map[string]any, error) {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 80 {
		return nil, core.NewError("ACCOUNT_NAME_INVALID", "Enter an account name of 1-80 characters", 422)
	}
	labels = cleanLabels(labels)
	if len(labels) > 20 {
		return nil, core.NewError("ACCOUNT_LABELS_INVALID", "Use at most 20 labels of 1-40 characters", 422)
	}
	for _, label := range labels {
		if utf8.RuneCountInString(label) > 40 {
			return nil, core.NewError("ACCOUNT_LABELS_INVALID", "Use at most 20 labels of 1-40 characters", 422)
		}
	}
	method, err := loginMethod(method)
	if err != nil {
		return nil, err
	}
	id, public, now := core.NewID(), "acct_"+core.RandomToken(18), core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		var reserved int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public_enrollments WHERE state='ACTIVE' AND lower(observed_email)=lower(?)", name).Scan(&reserved); err != nil {
			return err
		}
		if reserved > 0 {
			return core.NewError("ACCOUNT_NAME_EXISTS", "An active account already uses that name", 409)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", id, public, name, "chatgpt", method, nil, nullString(workspace), 0, "ENROLLING", now, now, nil); err != nil {
			return core.NewError("ACCOUNT_NAME_EXISTS", "An active account already uses that name", 409)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO account_state VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, "ENROLLING", "STOPPED", "STARTING", "UNKNOWN", nil, nil, nil, nil, nil, nil, nil, 1, now); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO usage_current(account_id,last_attempt_at_ms) VALUES(?,?)", id, now); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO account_profiles(account_id) VALUES(?)", id); err != nil {
			return err
		}
		for _, label := range labels {
			labelID := core.NewID()
			var existing string
			err := db.QueryRowContext(ctx, "SELECT label_id FROM labels WHERE lower(name)=lower(?)", label).Scan(&existing)
			if err == nil {
				labelID = existing
			} else if err == sql.ErrNoRows {
				if _, err = db.ExecContext(ctx, "INSERT INTO labels VALUES(?,?,?)", labelID, label, now); err != nil {
					return err
				}
			} else {
				return err
			}
			if _, err = db.ExecContext(ctx, "INSERT INTO account_labels VALUES(?,?)", id, labelID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Events.Publish("account.updated", map[string]any{"resource_id": public, "state": "STARTING"})
	return map[string]any{"account_id": id, "public_token": public, "display_name": name}, nil
}

func (s *Service) account(ctx context.Context, public string) (Account, error) {
	var a Account
	var enabled int
	var workspace sql.NullString
	err := s.Store.DB.QueryRowContext(ctx, "SELECT a.account_id,a.public_token,a.display_name,a.workspace_constraint,a.preferred_login_method,a.enabled,a.created_at_ms,s.auth_state,s.worker_state,s.upstream_email,s.upstream_plan,a.last_successful_login_method,s.last_auth_verified_at_ms FROM accounts a JOIN account_state s USING(account_id) WHERE a.public_token=? AND a.deleted_at_ms IS NULL", public).Scan(&a.ID, &a.PublicID, &a.DisplayName, &workspace, &a.PreferredMethod, &enabled, &a.CreatedAtMS, &a.AuthState, &a.WorkerState, &a.Email, &a.Plan, &a.LastMethod, &a.LastVerified)
	if err == sql.ErrNoRows {
		return a, core.NewError("ACCOUNT_NOT_FOUND", "Account not found", 404)
	}
	if err != nil {
		return a, err
	}
	a.Enabled = enabled == 1
	if workspace.Valid {
		a.Workspace = workspace.String
	}
	return a, nil
}

func (s *Service) Accounts(ctx context.Context) ([]AccountSummary, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT a.account_id,a.public_token,a.display_name,a.enabled,s.overall_state,s.auth_state,s.usage_state,u.short_used_percent_raw,u.short_resets_at_s,u.weekly_used_percent_raw,u.weekly_resets_at_s,u.complete_read_at_ms,u.last_error_summary,(SELECT group_concat(l.name, ', ') FROM account_labels al JOIN labels l USING(label_id) WHERE al.account_id=a.account_id),(SELECT kind FROM operations o WHERE o.account_id=a.account_id AND o.state NOT IN('SUCCEEDED','FAILED','CANCELLED') ORDER BY created_at_ms DESC LIMIT 1) FROM accounts a JOIN account_state s USING(account_id) LEFT JOIN usage_current u USING(account_id) WHERE a.deleted_at_ms IS NULL ORDER BY lower(a.display_name)`)
	if err != nil {
		return nil, err
	}
	var result []AccountSummary
	for rows.Next() {
		var item AccountSummary
		var enabled int
		var short, shortReset, weekly, weeklyReset, last sql.NullInt64
		var evidence, labels, active sql.NullString
		if err := rows.Scan(&item.AccountID, &item.PublicToken, &item.DisplayName, &enabled, &item.OverallState, &item.AuthState, &item.UsageState, &short, &shortReset, &weekly, &weeklyReset, &last, &evidence, &labels, &active); err != nil {
			return nil, err
		}
		item.Evidence = "No complete usage read yet"
		item.Enabled = enabled == 1
		if evidence.Valid {
			item.Evidence = evidence.String
		} else if last.Valid {
			item.Evidence = "Complete rate-limit evidence available"
		}
		item.ShortPercent = nullableInt(short)
		item.ShortResetMS = secondsPointer(shortReset)
		item.WeeklyPercent = nullableInt(weekly)
		item.WeeklyResetMS = secondsPointer(weeklyReset)
		item.LastRefreshMS = nullableInt(last)
		if labels.Valid {
			item.Labels = strings.Split(labels.String, ", ")
		}
		if active.Valid {
			item.ActiveOperation = &active.String
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	plan, err := s.cyclePlan(ctx, core.NowMS())
	if err != nil {
		return nil, err
	}
	byAccount := make(map[string]cyclePlanItem, len(plan))
	for _, item := range plan {
		byAccount[item.accountID] = item
	}
	for index := range result {
		if item, ok := byAccount[result[index].AccountID]; ok {
			result[index].CycleState = item.state
			result[index].CyclePosition = item.position
			result[index].CycleSize = item.size
			result[index].CycleReleaseMS = &item.releaseAtMS
		}
	}
	return result, nil
}

func (s *Service) AccountDetail(ctx context.Context, public string) (map[string]any, error) {
	a, err := s.account(ctx, public)
	if err != nil {
		return nil, err
	}
	account := map[string]any{"account_id": a.ID, "public_token": a.PublicID, "display_name": a.DisplayName, "enabled": a.Enabled, "auth_state": a.AuthState, "worker_state": a.WorkerState, "upstream_email": nullValue(a.Email), "upstream_plan": nullValue(a.Plan), "workspace_constraint": emptyNil(a.Workspace), "preferred_login_method": a.PreferredMethod, "last_successful_login_method": nullValue(a.LastMethod), "last_auth_verified_at_ms": nullableAny(a.LastVerified)}
	labels, err := s.labels(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	account["labels"] = labels
	usage := rowMap(s.Store.DB.QueryRowContext(ctx, "SELECT * FROM usage_current WHERE account_id=?", a.ID), usageColumns)
	operations, _ := s.OperationsFor(ctx, a.ID, 20)
	incidents, _ := s.IncidentsFor(ctx, a.ID)
	var exportAt sql.NullInt64
	_ = s.Store.DB.QueryRowContext(ctx, "SELECT created_at_ms FROM credential_bundles WHERE account_id=? AND state='EXPORT'", a.ID).Scan(&exportAt)
	return map[string]any{"account": account, "usage": usage, "operations": operations, "incidents": incidents, "auth_export": map[string]any{"available": exportAt.Valid, "created_at_ms": nullableAny(exportAt)}}, nil
}

func (s *Service) SetLabels(ctx context.Context, public string, labels []string) error {
	a, err := s.account(ctx, public)
	if err != nil {
		return err
	}
	labels = cleanLabels(labels)
	if len(labels) > 20 {
		return core.NewError("ACCOUNT_LABELS_INVALID", "Use at most 20 labels of 1-40 characters", 422)
	}
	now := core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "DELETE FROM account_labels WHERE account_id=?", a.ID); err != nil {
			return err
		}
		for _, label := range labels {
			if utf8.RuneCountInString(label) > 40 {
				return core.NewError("ACCOUNT_LABELS_INVALID", "Use at most 20 labels of 1-40 characters", 422)
			}
			id := core.NewID()
			_ = db.QueryRowContext(ctx, "SELECT label_id FROM labels WHERE lower(name)=lower(?)", label).Scan(&id)
			if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO labels VALUES(?,?,?)", id, label, now); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO account_labels VALUES(?,?)", a.ID, id); err != nil {
				return err
			}
		}
		_, err := db.ExecContext(ctx, "DELETE FROM labels WHERE NOT EXISTS (SELECT 1 FROM account_labels WHERE account_labels.label_id=labels.label_id)")
		return err
	})
	if err == nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": public, "labels": labels})
	}
	return err
}

func (s *Service) SetEnabled(ctx context.Context, public string, enabled bool) error {
	a, err := s.account(ctx, public)
	if err != nil {
		return err
	}
	now := core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		value, state := 0, "DISABLED"
		if enabled {
			value, state = 1, "STARTING"
		}
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET enabled=?,updated_at_ms=? WHERE account_id=?", value, now, a.ID); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "UPDATE account_state SET overall_state=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", state, now, a.ID)
		return err
	})
	if err == nil && !enabled {
		err = s.Runtime.Stop(a.ID)
	}
	if err == nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": public, "enabled": enabled})
	}
	return err
}

func (s *Service) DeleteAccount(ctx context.Context, public, confirmation string) error {
	a, err := s.account(ctx, public)
	if err != nil {
		return err
	}
	if confirmation != a.DisplayName {
		return core.NewError("DELETE_CONFIRMATION_MISMATCH", "Type the account name exactly", 409)
	}
	if err := s.Runtime.Stop(a.ID); err != nil {
		return err
	}
	now := core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET enabled=0,lifecycle_state='DELETED',deleted_at_ms=?,updated_at_ms=? WHERE account_id=?", now, now, a.ID); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM credential_bundles WHERE account_id=?", a.ID); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "UPDATE incidents SET state='CLOSED',resolved_at_ms=?,resolution_reason='administratively_closed' WHERE scope_key=? AND state='OPEN'", now, a.ID)
		return err
	})
	if err == nil {
		s.Events.Publish("account.updated", map[string]any{"resource_id": public, "deleted": true})
	}
	return err
}

func cleanLabels(values []string) []string {
	seen := map[string]string{}
	for _, value := range values {
		value = strings.Join(strings.Fields(value), " ")
		if value != "" {
			seen[strings.ToLower(value)] = value
		}
	}
	result := make([]string, 0, len(seen))
	for _, value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func truncateSummary(value string) string {
	runes := []rune(value)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return value
}
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func emptyNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}
func secondsPointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64 * 1000
	return &result
}
func nullableAny(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}
func nullValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

var usageColumns = []string{"account_id", "snapshot_id", "selected_limit_id", "short_raw_slot", "short_used_percent_raw", "short_duration_minutes", "short_resets_at_s", "short_anomaly", "weekly_raw_slot", "weekly_used_percent_raw", "weekly_duration_minutes", "weekly_resets_at_s", "weekly_anomaly", "complete_read_at_ms", "last_attempt_at_ms", "stale", "last_error_code", "last_error_summary", "source", "state_version"}

func rowMap(row *sql.Row, columns []string) map[string]any {
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	if row.Scan(targets...) != nil {
		return map[string]any{}
	}
	result := map[string]any{}
	for i, name := range columns {
		if data, ok := values[i].([]byte); ok {
			result[name] = string(data)
		} else {
			result[name] = values[i]
		}
	}
	return result
}

func (s *Service) labels(ctx context.Context, id string) ([]string, error) {
	rows, err := s.Store.DB.QueryContext(ctx, "SELECT l.name FROM labels l JOIN account_labels al USING(label_id) WHERE al.account_id=? ORDER BY lower(l.name)", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *Service) PublicEnrollmentOpen(ctx context.Context) (bool, error) {
	var value string
	err := s.Store.DB.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE setting_key='public_enrollment_open'").Scan(&value)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return value == "true", err
}
func (s *Service) SetPublicEnrollmentOpen(ctx context.Context, enabled bool) error {
	value, _ := json.Marshal(enabled)
	return s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "INSERT INTO settings VALUES('public_enrollment_open',?,?) ON CONFLICT(setting_key) DO UPDATE SET value_json=excluded.value_json,updated_at_ms=excluded.updated_at_ms", string(value), core.NowMS())
		return err
	})
}

func (s *Service) maintenanceLoop(ctx context.Context) {
	for {
		now, delay := core.NowMS(), time.Hour
		cutoff, incidentCutoff := now-30*24*60*60*1000, now-90*24*60*60*1000
		err := s.Store.Write(ctx, func(db store.Executor) error {
			queries := []struct {
				query string
				value int64
			}{
				{"DELETE FROM admin_sessions WHERE rowid IN (SELECT rowid FROM admin_sessions WHERE absolute_expires_at_ms<? ORDER BY absolute_expires_at_ms LIMIT 250)", now},
				{"DELETE FROM usage_snapshots WHERE snapshot_id IN (SELECT snapshot_id FROM usage_snapshots WHERE attempted_at_ms<? ORDER BY attempted_at_ms LIMIT 250)", cutoff},
				{"DELETE FROM webhook_events WHERE event_id IN (SELECT event_id FROM webhook_events WHERE created_at_ms<? ORDER BY created_at_ms LIMIT 250)", cutoff},
				{"DELETE FROM operations WHERE operation_id IN (SELECT operation_id FROM operations WHERE completed_at_ms<? ORDER BY completed_at_ms LIMIT 250)", cutoff},
				{"DELETE FROM credential_bundles WHERE bundle_id IN (SELECT bundle_id FROM credential_bundles WHERE state='RETIRED' AND COALESCE(retired_at_ms,created_at_ms)<? ORDER BY COALESCE(retired_at_ms,created_at_ms) LIMIT 250)", cutoff},
				{"DELETE FROM incidents WHERE incident_id IN (SELECT incident_id FROM incidents WHERE state='RESOLVED' AND resolved_at_ms<? ORDER BY resolved_at_ms LIMIT 250)", incidentCutoff},
			}
			for _, item := range queries {
				if _, err := db.ExecContext(ctx, item.query, item.value); err != nil {
					return err
				}
			}
			_, err := db.ExecContext(ctx, "PRAGMA incremental_vacuum(64)")
			return err
		})
		if err != nil {
			delay = 5 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-time.After(delay):
		}
	}
}
func (s *Service) usageLoop(ctx context.Context) {
	for {
		delay := time.Duration(s.Config.UsagePollSeconds+rand.IntN(31)) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-time.After(delay):
			rows, err := s.Store.DB.QueryContext(ctx, "SELECT public_token FROM accounts a JOIN account_state s USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED'")
			if err != nil {
				continue
			}
			var values []string
			for rows.Next() {
				var value string
				_ = rows.Scan(&value)
				values = append(values, value)
			}
			rows.Close()
			for _, value := range values {
				_, _ = s.Refresh(ctx, value, "SCHEDULED")
			}
		}
	}
}
func (s *Service) pulseLoop(ctx context.Context) {
	for {
		_ = s.QueuePulses(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-time.After(time.Duration(s.Config.WindowPulsePollSeconds) * time.Second):
		}
	}
}
