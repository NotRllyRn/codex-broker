package broker

import (
	"context"
	"database/sql"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

var operationColumns = []string{"operation_id", "account_id", "kind", "trigger", "state", "progress_code", "progress_summary", "result_json", "error_code", "error_summary", "created_at_ms", "started_at_ms", "completed_at_ms", "lease_token", "lease_expires_at_ms", "state_version"}
var incidentColumns = []string{"incident_id", "scope_kind", "scope_key", "problem_type", "state", "severity", "summary", "current_error_code", "occurrence_count", "opened_at_ms", "last_seen_at_ms", "resolved_at_ms", "resolution_reason", "state_version"}

func (s *Service) CreateOperation(ctx context.Context, accountID, kind, trigger string) (string, error) {
	id, now := core.NewID(), core.NowMS()
	err := s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, nullString(accountID), kind, trigger, "QUEUED", nil, nil, nil, nil, nil, now, nil, nil, nil, nil, 1)
		return err
	})
	return id, err
}

func (s *Service) OperationState(ctx context.Context, id, state, code, summary string) error {
	now := core.NowMS()
	err := s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "UPDATE operations SET state=?,progress_code=?,progress_summary=?,started_at_ms=COALESCE(started_at_ms,?),completed_at_ms=CASE WHEN ? IN('SUCCEEDED','FAILED','CANCELLED') THEN ? ELSE completed_at_ms END,state_version=state_version+1 WHERE operation_id=?", state, code, summary, now, state, now, id)
		return err
	})
	if err == nil {
		s.Events.Publish("operation.updated", map[string]any{"operation_id": id, "state": state})
	}
	return err
}

func (s *Service) FailOperation(ctx context.Context, id, code, summary string) error {
	if redacted, ok := logbook.Redact(summary).(string); ok {
		summary = redacted
	}
	summary = truncateSummary(summary)
	now := core.NowMS()
	err := s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "UPDATE operations SET state='FAILED',error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?", code, summary, now, id)
		return err
	})
	if err == nil {
		s.Events.Publish("operation.updated", map[string]any{"operation_id": id, "state": "FAILED", "error_code": code})
	}
	return err
}

func queryMaps(ctx context.Context, database *sql.DB, query string, columns []string, args ...any) ([]map[string]any, error) {
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		item := map[string]any{}
		for i, column := range columns {
			if raw, ok := values[i].([]byte); ok {
				item[column] = string(raw)
			} else {
				item[column] = values[i]
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Service) Operations(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit > 500 {
		limit = 500
	}
	return queryMaps(ctx, s.Store.DB, "SELECT * FROM operations ORDER BY created_at_ms DESC LIMIT ?", operationColumns, limit)
}
func (s *Service) OperationsFor(ctx context.Context, accountID string, limit int) ([]map[string]any, error) {
	return queryMaps(ctx, s.Store.DB, "SELECT * FROM operations WHERE account_id=? ORDER BY created_at_ms DESC LIMIT ?", operationColumns, accountID, limit)
}
func (s *Service) Operation(ctx context.Context, id string) (map[string]any, error) {
	values, err := queryMaps(ctx, s.Store.DB, "SELECT * FROM operations WHERE operation_id=?", operationColumns, id)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, core.NewError("OPERATION_NOT_FOUND", "Operation not found", 404)
	}
	return values[0], nil
}
func (s *Service) Incidents(ctx context.Context) ([]map[string]any, error) {
	return queryMaps(ctx, s.Store.DB, "SELECT * FROM incidents ORDER BY opened_at_ms DESC LIMIT 200", incidentColumns)
}
func (s *Service) IncidentsFor(ctx context.Context, accountID string) ([]map[string]any, error) {
	return queryMaps(ctx, s.Store.DB, "SELECT * FROM incidents WHERE scope_key=? ORDER BY opened_at_ms DESC LIMIT 20", incidentColumns, accountID)
}
