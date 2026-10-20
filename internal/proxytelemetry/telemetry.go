package proxytelemetry

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const RollingPeriod = "ROLLING_7D"
const LifetimePeriod = "LIFETIME"

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
}

type RecordResult struct {
	Inserted bool
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
			output_tokens,reasoning_output_tokens,total_tokens,usage_json,stats_model,active_duration_ms
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			value.RequestID, accountID, clientID, emptyNil(value.ThreadID), emptyNil(value.TurnID), value.Endpoint,
			emptyNil(value.RequestModel), emptyNil(value.ReasoningEffort), emptyNil(value.RequestServiceTier),
			emptyNil(value.PreviousResponseID), emptyNil(value.ResponseID), emptyNil(value.ResponseModel),
			emptyNil(value.ResponseServiceTier), value.TerminalStatus, value.UpstreamAttempts, value.UpstreamHTTPStatus,
			value.RequestStartedAtMS, value.UpstreamStartedAtMS, value.ResponseHeadersAtMS, value.FirstResponseEventAtMS,
			value.TerminalEventAtMS, value.InputTokens, value.CachedInputTokens, value.OutputTokens, value.ReasoningTokens,
			value.TotalTokens, usageJSON, model, activeMS)
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
		return recordQuotaActivity(ctx, db, accountID, model, value.UpstreamStartedAtMS, value.TerminalEventAtMS)
	})
	return result, err
}

func Prune(ctx context.Context, database *store.Store, cutoff int64, limit int) error {
	return database.Write(ctx, func(db store.Executor) error {
		rows, err := db.QueryContext(ctx, `SELECT proxy_request_id,stats_model,active_duration_ms,
			COALESCE(input_tokens,0),COALESCE(cached_input_tokens,0),COALESCE(output_tokens,0),
			COALESCE(reasoning_output_tokens,0),COALESCE(total_tokens,0)
			FROM proxy_responses WHERE terminal_event_at_ms<? ORDER BY terminal_event_at_ms LIMIT ?`, cutoff, limit)
		if err != nil {
			return err
		}
		type expired struct {
			id, model                                       string
			active, input, cached, output, reasoning, total int64
		}
		var values []expired
		for rows.Next() {
			var item expired
			if err := rows.Scan(&item.id, &item.model, &item.active, &item.input, &item.cached, &item.output, &item.reasoning, &item.total); err != nil {
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
		_, err = db.ExecContext(ctx, "DELETE FROM quota_intervals WHERE ended_at_ms<?", cutoff)
		return err
	})
}

func ReadTotals(ctx context.Context, database *sql.DB) ([]Totals, error) {
	rows, err := database.QueryContext(ctx, `SELECT period,model,response_count,active_duration_ms,input_tokens,
		cached_input_tokens,output_tokens,reasoning_output_tokens,total_tokens
		FROM proxy_usage_totals ORDER BY model,period`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Totals
	for rows.Next() {
		var item Totals
		if err := rows.Scan(&item.Period, &item.Model, &item.Responses, &item.ActiveMS, &item.InputTokens,
			&item.CachedInputTokens, &item.OutputTokens, &item.ReasoningOutputTokens, &item.TotalTokens); err != nil {
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

func subtractResponse(ctx context.Context, db store.Executor, item struct {
	id, model                                       string
	active, input, cached, output, reasoning, total int64
}) error {
	_, err := db.ExecContext(ctx, `UPDATE proxy_usage_totals SET
		response_count=response_count-1,active_duration_ms=active_duration_ms-?,input_tokens=input_tokens-?,
		cached_input_tokens=cached_input_tokens-?,output_tokens=output_tokens-?,reasoning_output_tokens=reasoning_output_tokens-?,
		total_tokens=total_tokens-?
		WHERE period=? AND model=?`, item.active, item.input, item.cached, item.output, item.reasoning, item.total,
		RollingPeriod, item.model)
	return err
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

func emptyNil(value string) any {
	if value == "" {
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
