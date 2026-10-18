package proxytelemetry

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const RollingPeriod = "ROLLING_7D"
const LifetimePeriod = "LIFETIME"

type UsageSnapshot struct {
	ID                                                       string
	AtMS                                                     int64
	ShortUsed, ShortResetsAtMS, WeeklyUsed, WeeklyResetsAtMS *int64
}

type Totals struct {
	Period                string `json:"period"`
	Model                 string `json:"model"`
	Responses             int64  `json:"responses"`
	ActiveMS              int64  `json:"active_ms"`
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	ReasoningOutputTokens int64  `json:"reasoning_output_tokens"`
	TotalTokens           int64  `json:"total_tokens"`
	ShortSamples          int64  `json:"short_usage_samples"`
	ShortActiveMS         int64  `json:"short_usage_active_ms"`
	ShortUsagePercent     int64  `json:"short_usage_percent"`
	WeeklySamples         int64  `json:"weekly_usage_samples"`
	WeeklyActiveMS        int64  `json:"weekly_usage_active_ms"`
	WeeklyUsagePercent    int64  `json:"weekly_usage_percent"`
}

type RecordResult struct {
	Inserted       bool
	RefreshAccount string
}

func Record(ctx context.Context, database *store.Store, value gateway.Observation) (result RecordResult, err error) {
	err = database.Write(ctx, func(db store.Executor) error {
		accountID, err := internalAccount(ctx, db, value.AccountID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		clientID := any(nil)
		if value.ClientID != "" {
			var exists bool
			if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_api_keys WHERE key_id=?)", value.ClientID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				clientID = value.ClientID
				_, _ = db.ExecContext(ctx, "UPDATE client_api_keys SET last_used_at_ms=? WHERE key_id=?", value.TerminalEventAtMS, value.ClientID)
			}
		}
		model := statsModel(value.ResponseModel, value.RequestModel)
		activeMS := max(int64(0), value.TerminalEventAtMS-value.UpstreamStartedAtMS)
		usageJSON := any(nil)
		if len(value.UsageJSON) > 0 {
			usageJSON = string(value.UsageJSON)
		}
		insert, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO proxy_responses(
			proxy_request_id,account_id,client_key_id,thread_id,turn_id,endpoint,request_model,reasoning_effort,
			request_service_tier,previous_response_id,response_id,response_model,response_service_tier,
			terminal_status,upstream_attempts,upstream_http_status,request_started_at_ms,upstream_started_at_ms,
			response_headers_at_ms,first_response_event_at_ms,terminal_event_at_ms,input_tokens,cached_input_tokens,
			output_tokens,reasoning_output_tokens,total_tokens,usage_json,stats_model,active_duration_ms,quota_isolated
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			value.RequestID, accountID, clientID, emptyNil(value.ThreadID), emptyNil(value.TurnID), value.Endpoint,
			emptyNil(value.RequestModel), emptyNil(value.ReasoningEffort), emptyNil(value.RequestServiceTier),
			emptyNil(value.PreviousResponseID), emptyNil(value.ResponseID), emptyNil(value.ResponseModel),
			emptyNil(value.ResponseServiceTier), value.TerminalStatus, value.UpstreamAttempts, value.UpstreamHTTPStatus,
			value.RequestStartedAtMS, value.UpstreamStartedAtMS, value.ResponseHeadersAtMS, value.FirstResponseEventAtMS,
			value.TerminalEventAtMS, value.InputTokens, value.CachedInputTokens, value.OutputTokens, value.ReasoningTokens,
			value.TotalTokens, usageJSON, model, activeMS, 1)
		if err != nil {
			return err
		}
		changed, _ := insert.RowsAffected()
		if changed == 0 {
			return nil
		}
		result.Inserted = true
		if err := addResponse(ctx, db, RollingPeriod, model, activeMS, value, value.TerminalEventAtMS); err != nil {
			return err
		}
		if err := addResponse(ctx, db, LifetimePeriod, model, activeMS, value, value.TerminalEventAtMS); err != nil {
			return err
		}
		for index, attempt := range value.Attempts {
			attemptAccount, err := internalAccount(ctx, db, attempt.AccountID)
			if err != nil {
				return err
			}
			eligible := index == len(value.Attempts)-1 && attempt.UsageSnapshotID != "" && attempt.UsageSnapshotAtMS > 0
			_, err = db.ExecContext(ctx, `INSERT INTO proxy_response_attempts(
				proxy_request_id,ordinal,account_id,model,started_at_ms,ended_at_ms,upstream_http_status,failure_kind,
				usage_start_snapshot_id,usage_start_at_ms,short_start_used_percent,short_start_resets_at_ms,
				weekly_start_used_percent,weekly_start_resets_at_ms,quota_eligible,quota_isolated
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, value.RequestID, attempt.Ordinal, attemptAccount, model,
				attempt.StartedAtMS, attempt.EndedAtMS, attempt.UpstreamHTTPStatus, emptyNil(attempt.FailureKind),
				emptyNil(attempt.UsageSnapshotID), nullablePositive(attempt.UsageSnapshotAtMS), attempt.ShortUsedPercent,
				attempt.ShortResetsAtMS, attempt.WeeklyUsedPercent, attempt.WeeklyResetsAtMS, boolInt(eligible))
			if err != nil {
				return err
			}
		}
		if len(value.Attempts) == 0 {
			_, err = db.ExecContext(ctx, `INSERT INTO proxy_response_attempts(
				proxy_request_id,ordinal,account_id,model,started_at_ms,ended_at_ms,upstream_http_status,
				quota_eligible,quota_isolated,invalid_reason
			) VALUES(?,?,?,?,?,?,?,0,0,'missing_attempt_telemetry')`, value.RequestID, value.UpstreamAttempts,
				accountID, model, value.UpstreamStartedAtMS, value.TerminalEventAtMS, value.UpstreamHTTPStatus)
			if err != nil {
				return err
			}
		}
		isolated, err := isolateNewResponse(ctx, db, value.RequestID)
		if err != nil {
			return err
		}
		if isolated {
			result.RefreshAccount = value.AccountID
		}
		return nil
	})
	return result, err
}

func CompleteUsage(ctx context.Context, db store.Executor, accountID string, snapshot UsageSnapshot) error {
	rows, err := db.QueryContext(ctx, `SELECT a.proxy_request_id,a.ordinal,a.model,a.quota_isolated,
		a.short_start_used_percent,a.short_start_resets_at_ms,a.weekly_start_used_percent,a.weekly_start_resets_at_ms,
		r.active_duration_ms
		FROM proxy_response_attempts a JOIN proxy_responses r USING(proxy_request_id)
		WHERE a.account_id=? AND a.quota_eligible=1 AND a.usage_end_snapshot_id IS NULL AND a.ended_at_ms<=?`, accountID, snapshot.AtMS)
	if err != nil {
		return err
	}
	type pending struct {
		requestID      string
		ordinal        int
		model          string
		isolated       bool
		shortStart     sql.NullInt64
		shortReset     sql.NullInt64
		weeklyStart    sql.NullInt64
		weeklyReset    sql.NullInt64
		activeDuration int64
	}
	var values []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.requestID, &item.ordinal, &item.model, &item.isolated, &item.shortStart, &item.shortReset, &item.weeklyStart, &item.weeklyReset, &item.activeDuration); err != nil {
			rows.Close()
			return err
		}
		values = append(values, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range values {
		_, err := db.ExecContext(ctx, `UPDATE proxy_response_attempts SET
			usage_end_snapshot_id=?,usage_end_at_ms=?,short_end_used_percent=?,short_end_resets_at_ms=?,
			weekly_end_used_percent=?,weekly_end_resets_at_ms=?
			WHERE proxy_request_id=? AND ordinal=?`, snapshot.ID, snapshot.AtMS, snapshot.ShortUsed,
			snapshot.ShortResetsAtMS, snapshot.WeeklyUsed, snapshot.WeeklyResetsAtMS, item.requestID, item.ordinal)
		if err != nil || !item.isolated {
			if err != nil {
				return err
			}
			continue
		}
		shortDelta, shortReason := positiveDelta(item.shortStart, item.shortReset, snapshot.ShortUsed, snapshot.ShortResetsAtMS)
		weeklyDelta, weeklyReason := positiveDelta(item.weeklyStart, item.weeklyReset, snapshot.WeeklyUsed, snapshot.WeeklyResetsAtMS)
		reason := joinReasons(shortReason, weeklyReason)
		if _, err := db.ExecContext(ctx, `UPDATE proxy_responses SET short_usage_delta=?,weekly_usage_delta=?,quota_invalid_reason=? WHERE proxy_request_id=?`, shortDelta, weeklyDelta, emptyNil(reason), item.requestID); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `UPDATE proxy_response_attempts SET invalid_reason=? WHERE proxy_request_id=? AND ordinal=?`, emptyNil(reason), item.requestID, item.ordinal); err != nil {
			return err
		}
		for _, period := range []string{RollingPeriod, LifetimePeriod} {
			if err := addQuota(ctx, db, period, item.model, item.activeDuration, shortDelta, weeklyDelta, snapshot.AtMS); err != nil {
				return err
			}
		}
	}
	return nil
}

func Prune(ctx context.Context, database *store.Store, cutoff int64, limit int) error {
	return database.Write(ctx, func(db store.Executor) error {
		rows, err := db.QueryContext(ctx, `SELECT proxy_request_id,stats_model,active_duration_ms,
			COALESCE(input_tokens,0),COALESCE(cached_input_tokens,0),COALESCE(output_tokens,0),
			COALESCE(reasoning_output_tokens,0),COALESCE(total_tokens,0),short_usage_delta,weekly_usage_delta
			FROM proxy_responses WHERE terminal_event_at_ms<? ORDER BY terminal_event_at_ms LIMIT ?`, cutoff, limit)
		if err != nil {
			return err
		}
		type expired struct {
			id, model                                       string
			active, input, cached, output, reasoning, total int64
			short, weekly                                   sql.NullInt64
		}
		var values []expired
		for rows.Next() {
			var item expired
			if err := rows.Scan(&item.id, &item.model, &item.active, &item.input, &item.cached, &item.output, &item.reasoning, &item.total, &item.short, &item.weekly); err != nil {
				rows.Close()
				return err
			}
			values = append(values, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range values {
			if err := subtractResponse(ctx, db, item); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, "DELETE FROM proxy_responses WHERE proxy_request_id=?", item.id); err != nil {
				return err
			}
		}
		return nil
	})
}

func ReadTotals(ctx context.Context, database *sql.DB) ([]Totals, error) {
	rows, err := database.QueryContext(ctx, `SELECT period,model,response_count,active_duration_ms,input_tokens,
		cached_input_tokens,output_tokens,reasoning_output_tokens,total_tokens,short_usage_samples,
		short_usage_active_ms,short_usage_percent,weekly_usage_samples,weekly_usage_active_ms,weekly_usage_percent
		FROM proxy_usage_totals ORDER BY model,period`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Totals
	for rows.Next() {
		var item Totals
		if err := rows.Scan(&item.Period, &item.Model, &item.Responses, &item.ActiveMS, &item.InputTokens,
			&item.CachedInputTokens, &item.OutputTokens, &item.ReasoningOutputTokens, &item.TotalTokens,
			&item.ShortSamples, &item.ShortActiveMS, &item.ShortUsagePercent, &item.WeeklySamples,
			&item.WeeklyActiveMS, &item.WeeklyUsagePercent); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func addResponse(ctx context.Context, db store.Executor, period, model string, activeMS int64, value gateway.Observation, now int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO proxy_usage_totals(
		period,model,response_count,active_duration_ms,input_tokens,cached_input_tokens,
		output_tokens,reasoning_output_tokens,total_tokens,updated_at_ms
	) VALUES(?,?,1,?,?,?,?,?,?,?) ON CONFLICT(period,model) DO UPDATE SET
		response_count=response_count+1,active_duration_ms=active_duration_ms+excluded.active_duration_ms,
		input_tokens=input_tokens+excluded.input_tokens,cached_input_tokens=cached_input_tokens+excluded.cached_input_tokens,
		output_tokens=output_tokens+excluded.output_tokens,reasoning_output_tokens=reasoning_output_tokens+excluded.reasoning_output_tokens,
		total_tokens=total_tokens+excluded.total_tokens,updated_at_ms=excluded.updated_at_ms`, period, model, activeMS,
		valueOrZero(value.InputTokens), valueOrZero(value.CachedInputTokens), valueOrZero(value.OutputTokens),
		valueOrZero(value.ReasoningTokens), valueOrZero(value.TotalTokens), now)
	return err
}

func addQuota(ctx context.Context, db store.Executor, period, model string, activeMS int64, short, weekly *int64, now int64) error {
	shortSample, weeklySample := boolInt(short != nil), boolInt(weekly != nil)
	_, err := db.ExecContext(ctx, `INSERT INTO proxy_usage_totals(
		period,model,short_usage_samples,short_usage_active_ms,short_usage_percent,
		weekly_usage_samples,weekly_usage_active_ms,weekly_usage_percent,updated_at_ms
	) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(period,model) DO UPDATE SET
		short_usage_samples=short_usage_samples+excluded.short_usage_samples,
		short_usage_active_ms=short_usage_active_ms+excluded.short_usage_active_ms,
		short_usage_percent=short_usage_percent+excluded.short_usage_percent,
		weekly_usage_samples=weekly_usage_samples+excluded.weekly_usage_samples,
		weekly_usage_active_ms=weekly_usage_active_ms+excluded.weekly_usage_active_ms,
		weekly_usage_percent=weekly_usage_percent+excluded.weekly_usage_percent,
		updated_at_ms=excluded.updated_at_ms`, period, model, shortSample, activeMS*int64(shortSample),
		valueOrZero(short), weeklySample, activeMS*int64(weeklySample), valueOrZero(weekly), now)
	return err
}

func isolateNewResponse(ctx context.Context, db store.Executor, requestID string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT other.proxy_request_id
		FROM proxy_response_attempts current JOIN proxy_response_attempts other
		ON other.account_id=current.account_id AND other.proxy_request_id<>current.proxy_request_id
		AND current.started_at_ms<COALESCE(other.usage_end_at_ms,?)
		WHERE current.proxy_request_id=? AND current.quota_eligible=1 AND other.quota_eligible=1`, int64(math.MaxInt64), requestID)
	if err != nil {
		return false, err
	}
	var contaminated []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		contaminated = append(contaminated, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(contaminated) == 0 {
		var eligible bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM proxy_response_attempts WHERE proxy_request_id=? AND quota_eligible=1)", requestID).Scan(&eligible); err != nil {
			return false, err
		}
		if !eligible {
			_, err := db.ExecContext(ctx, "UPDATE proxy_responses SET quota_isolated=0,quota_invalid_reason='missing_usage_baseline' WHERE proxy_request_id=?", requestID)
			return false, err
		}
		return true, nil
	}
	contaminated = append(contaminated, requestID)
	for _, id := range contaminated {
		if err := invalidateQuota(ctx, db, id, "shared_usage_interval"); err != nil {
			return false, err
		}
	}
	return false, nil
}

func invalidateQuota(ctx context.Context, db store.Executor, requestID, reason string) error {
	var model string
	var active int64
	var short, weekly sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT stats_model,active_duration_ms,short_usage_delta,weekly_usage_delta FROM proxy_responses WHERE proxy_request_id=?", requestID).Scan(&model, &active, &short, &weekly)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, period := range []string{RollingPeriod, LifetimePeriod} {
		if _, err := db.ExecContext(ctx, `UPDATE proxy_usage_totals SET
			short_usage_samples=short_usage_samples-?,short_usage_active_ms=short_usage_active_ms-?,short_usage_percent=short_usage_percent-?,
			weekly_usage_samples=weekly_usage_samples-?,weekly_usage_active_ms=weekly_usage_active_ms-?,weekly_usage_percent=weekly_usage_percent-?
			WHERE period=? AND model=?`, boolInt(short.Valid), active*int64(boolInt(short.Valid)), short.Int64,
			boolInt(weekly.Valid), active*int64(boolInt(weekly.Valid)), weekly.Int64, period, model); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, "UPDATE proxy_responses SET quota_isolated=0,quota_invalid_reason=?,short_usage_delta=NULL,weekly_usage_delta=NULL WHERE proxy_request_id=?", reason, requestID); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE proxy_response_attempts SET quota_isolated=0,invalid_reason=? WHERE proxy_request_id=? AND quota_eligible=1", reason, requestID)
	return err
}

func subtractResponse(ctx context.Context, db store.Executor, item struct {
	id, model                                       string
	active, input, cached, output, reasoning, total int64
	short, weekly                                   sql.NullInt64
}) error {
	_, err := db.ExecContext(ctx, `UPDATE proxy_usage_totals SET
		response_count=response_count-1,active_duration_ms=active_duration_ms-?,input_tokens=input_tokens-?,
		cached_input_tokens=cached_input_tokens-?,output_tokens=output_tokens-?,reasoning_output_tokens=reasoning_output_tokens-?,
		total_tokens=total_tokens-?,short_usage_samples=short_usage_samples-?,short_usage_active_ms=short_usage_active_ms-?,
		short_usage_percent=short_usage_percent-?,weekly_usage_samples=weekly_usage_samples-?,
		weekly_usage_active_ms=weekly_usage_active_ms-?,weekly_usage_percent=weekly_usage_percent-?
		WHERE period=? AND model=?`, item.active, item.input, item.cached, item.output, item.reasoning, item.total,
		boolInt(item.short.Valid), item.active*int64(boolInt(item.short.Valid)), item.short.Int64,
		boolInt(item.weekly.Valid), item.active*int64(boolInt(item.weekly.Valid)), item.weekly.Int64,
		RollingPeriod, item.model)
	return err
}

func positiveDelta(start, reset sql.NullInt64, end, endReset *int64) (*int64, string) {
	if !start.Valid || !reset.Valid || end == nil || endReset == nil {
		return nil, "missing_window_data"
	}
	if reset.Int64 != *endReset || *end < start.Int64 {
		return nil, "window_reset"
	}
	if *end == start.Int64 {
		return nil, "below_resolution"
	}
	value := *end - start.Int64
	return &value, ""
}

func internalAccount(ctx context.Context, db store.Executor, public string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, "SELECT account_id FROM accounts WHERE public_token=?", public).Scan(&id)
	return id, err
}

func statsModel(response, request string) string {
	if value := strings.TrimSpace(response); value != "" {
		return value
	}
	if value := strings.TrimSpace(request); value != "" {
		return value
	}
	return "unknown"
}

func joinReasons(short, weekly string) string {
	if short == "" || weekly == "" {
		return ""
	}
	if short == weekly {
		return short
	}
	return short + "," + weekly
}

func emptyNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
