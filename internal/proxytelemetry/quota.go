package proxytelemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

const QuotaRetention = 7 * 24 * time.Hour

type QuotaWindow struct{ Used, ResetS *int64 }
type QuotaSnapshot struct {
	ID            string
	AtMS          int64
	Short, Weekly QuotaWindow
}

type QuotaTotals struct {
	AccountID, AccountLabel, Period, Window, Model string
	Samples, UsedPercent, ActiveMS                 int64
}

type quotaInterval struct {
	snapshot, account, window string
	start, end, used, active  int64
	spans                     [][2]int64
	model                     sql.NullString
}

// RecordUsage runs before usage_current is replaced, in the poll's transaction.
// Only actual same-window measurements enter the estimate, never routing blocks.
func RecordUsage(ctx context.Context, db store.Executor, accountID string, snapshot QuotaSnapshot) error {
	var start, shortUsed, shortReset, weeklyUsed, weeklyReset sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT complete_read_at_ms,short_used_percent_raw,short_resets_at_s,
		weekly_used_percent_raw,weekly_resets_at_s FROM usage_current WHERE account_id=?`, accountID).
		Scan(&start, &shortUsed, &shortReset, &weeklyUsed, &weeklyReset); err != nil {
		return err
	}
	if !start.Valid || start.Int64 <= 0 || snapshot.AtMS <= start.Int64 || snapshot.AtMS-start.Int64 > QuotaRetention.Milliseconds() {
		return nil
	}
	var spans [][2]int64
	model := sql.NullString{Valid: true}
	rows, err := db.QueryContext(ctx, `SELECT upstream_started_at_ms,terminal_event_at_ms,stats_model FROM proxy_responses
		WHERE account_id=? AND upstream_started_at_ms>0 AND upstream_started_at_ms<? AND terminal_event_at_ms>?`, accountID, snapshot.AtMS, start.Int64)
	if err != nil {
		return err
	}
	for rows.Next() {
		var from, to int64
		var responseModel string
		if err := rows.Scan(&from, &to, &responseModel); err != nil {
			rows.Close()
			return err
		}
		if from, to = max(from, start.Int64), min(to, snapshot.AtMS); to > from {
			spans = append(spans, [2]int64{from, to})
			model = quotaModel(model, responseModel)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	spans = mergeSpans(spans)
	for _, window := range []struct {
		kind        string
		used, reset sql.NullInt64
		next        QuotaWindow
	}{
		{"short", shortUsed, shortReset, snapshot.Short}, {"weekly", weeklyUsed, weeklyReset, snapshot.Weekly},
	} {
		if !window.used.Valid || !window.reset.Valid || window.next.Used == nil || window.next.ResetS == nil {
			continue
		}
		used, reset := *window.next.Used, *window.next.ResetS
		if window.used.Int64 < 0 || used < window.used.Int64 || used > 100 || reset != window.reset.Int64 || reset <= snapshot.AtMS/1000 {
			continue
		}
		interval := quotaInterval{snapshot: snapshot.ID, account: accountID, window: window.kind, start: start.Int64, end: snapshot.AtMS, used: used - window.used.Int64, spans: spans, model: model}
		insert, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO quota_intervals(snapshot_id,account_id,window_kind,started_at_ms,ended_at_ms,used_percent,model)
			VALUES(?,?,?,?,?,?,?)`, interval.snapshot, accountID, interval.window, interval.start, interval.end, interval.used, model)
		if err != nil {
			return err
		}
		changed, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		if changed > 0 {
			if err := saveActivity(ctx, db, interval); err != nil {
				return err
			}
		}
	}
	return nil
}

// Completed streams and delayed relay events can arrive after a poll. Retained
// merged spans make corrections safe even after the response rows are pruned.
func recordQuotaActivity(ctx context.Context, db store.Executor, accountID, model string, start, end int64) error {
	if start <= 0 || end <= start {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT snapshot_id,window_kind,started_at_ms,ended_at_ms,used_percent,active_ms,active_spans_json,model
		FROM quota_intervals WHERE account_id=? AND started_at_ms<? AND ended_at_ms>?`, accountID, end, start)
	if err != nil {
		return err
	}
	var intervals []quotaInterval
	for rows.Next() {
		interval := quotaInterval{account: accountID}
		var encoded string
		if err := rows.Scan(&interval.snapshot, &interval.window, &interval.start, &interval.end, &interval.used, &interval.active, &encoded, &interval.model); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(encoded), &interval.spans); err != nil {
			rows.Close()
			return err
		}
		if from, to := max(start, interval.start), min(end, interval.end); to > from {
			interval.spans = mergeSpans(append(interval.spans, [2]int64{from, to}))
			interval.model = quotaModel(interval.model, model)
		}
		intervals = append(intervals, interval)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, interval := range intervals {
		if err := saveActivity(ctx, db, interval); err != nil {
			return err
		}
	}
	return nil
}

// Once mixed or unknown, an interval cannot regain single-model attribution.
func quotaModel(current sql.NullString, model string) sql.NullString {
	if !current.Valid || model == "" || model == "unknown" || current.String != "" && current.String != model {
		return sql.NullString{}
	}
	return sql.NullString{String: model, Valid: true}
}

func mergeSpans(spans [][2]int64) [][2]int64 {
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := make([][2]int64, 0, len(spans))
	for _, span := range spans {
		if len(merged) > 0 && span[0] <= merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = max(merged[len(merged)-1][1], span[1])
		} else {
			merged = append(merged, span)
		}
	}
	return merged
}

func saveActivity(ctx context.Context, db store.Executor, interval quotaInterval) error {
	var active int64
	for _, span := range interval.spans {
		active += span[1] - span[0]
	}
	var previousModel sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT model FROM quota_intervals WHERE snapshot_id=? AND window_kind=?`, interval.snapshot, interval.window).Scan(&previousModel); err != nil {
		return err
	}
	if active == interval.active && previousModel == interval.model {
		return nil
	}
	// Reverse the prior attribution before applying the corrected one. This also
	// retracts an estimate when a delayed response reveals mixed-model activity.
	if err := adjustModelLifetime(ctx, db, interval, previousModel, interval.active, -1); err != nil {
		return err
	}
	if err := adjustModelLifetime(ctx, db, interval, interval.model, active, 1); err != nil {
		return err
	}
	encoded, err := json.Marshal(interval.spans)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_intervals SET active_ms=?,active_spans_json=?,model=? WHERE snapshot_id=? AND window_kind=?`, active, string(encoded), interval.model, interval.snapshot, interval.window); err != nil {
		return err
	}
	var samples, used int64
	if interval.active == 0 && active > 0 {
		samples, used = 1, interval.used
	}
	_, err = db.ExecContext(ctx, `INSERT INTO quota_lifetime(account_id,window_kind,samples,used_percent,active_ms) VALUES(?,?,?,?,?)
		ON CONFLICT(account_id,window_kind) DO UPDATE SET samples=samples+excluded.samples,
		used_percent=used_percent+excluded.used_percent,active_ms=active_ms+excluded.active_ms`, interval.account, interval.window, samples, used, active-interval.active)
	return err
}

func adjustModelLifetime(ctx context.Context, db store.Executor, interval quotaInterval, model sql.NullString, active, sign int64) error {
	if !model.Valid || model.String == "" || active == 0 {
		return nil
	}
	if sign < 0 {
		_, err := db.ExecContext(ctx, `UPDATE quota_model_lifetime SET samples=samples-1,used_percent=used_percent-?,active_ms=active_ms-?
			WHERE account_id=? AND model=? AND window_kind=?`, interval.used, active, interval.account, model.String, interval.window)
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO quota_model_lifetime(account_id,model,window_kind,samples,used_percent,active_ms) VALUES(?,?,?,1,?,?)
		ON CONFLICT(account_id,model,window_kind) DO UPDATE SET samples=samples+1,used_percent=used_percent+excluded.used_percent,active_ms=active_ms+excluded.active_ms`, interval.account, model.String, interval.window, interval.used, active)
	return err
}

func ReadQuotaTotals(ctx context.Context, db *sql.DB, cutoff int64) ([]QuotaTotals, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.public_token,a.display_name,q.period,q.window_kind,q.model,q.samples,q.used_percent,q.active_ms FROM (
		SELECT account_id,'LIFETIME' AS period,window_kind,'' AS model,samples,used_percent,active_ms FROM quota_lifetime
		UNION ALL SELECT account_id,'ROLLING_7D',window_kind,'',count(*),sum(used_percent),sum(active_ms) FROM quota_intervals
		WHERE ended_at_ms>=? AND active_ms>0 GROUP BY account_id,window_kind
		UNION ALL SELECT account_id,'LIFETIME',window_kind,model,samples,used_percent,active_ms FROM quota_model_lifetime WHERE samples>0
		UNION ALL SELECT account_id,'ROLLING_7D',window_kind,model,count(*),sum(used_percent),sum(active_ms) FROM quota_intervals
		WHERE ended_at_ms>=? AND active_ms>0 AND model IS NOT NULL AND model<>'' GROUP BY account_id,window_kind,model
	) q JOIN accounts a USING(account_id) ORDER BY q.model,a.display_name,a.public_token,q.period,q.window_kind`, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []QuotaTotals
	for rows.Next() {
		var item QuotaTotals
		if err := rows.Scan(&item.AccountID, &item.AccountLabel, &item.Period, &item.Window, &item.Model, &item.Samples, &item.UsedPercent, &item.ActiveMS); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
