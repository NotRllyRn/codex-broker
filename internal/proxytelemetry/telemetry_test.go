package proxytelemetry

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

func telemetryFixture(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('account-a','public-a','Account A',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('account-a','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,snapshot_id,short_used_percent_raw,short_resets_at_s,weekly_used_percent_raw,weekly_resets_at_s,complete_read_at_ms)
		VALUES('account-a','snapshot-0',10,10,20,20,50);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return database, ctx
}

func observation(id string, started, ended int64) gateway.Observation {
	input, cached, output, reasoning, total := int64(100), int64(80), int64(25), int64(10), int64(125)
	short, weekly, shortReset, weeklyReset := int64(10), int64(20), int64(10_000), int64(20_000)
	return gateway.Observation{
		RequestID: id, AccountID: "public-a", Endpoint: "/v1/responses", RequestModel: "requested",
		ResponseModel: "gpt-a", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamHTTPStatus: 200,
		RequestStartedAtMS: started - 10, UpstreamStartedAtMS: started, ResponseHeadersAtMS: started + 1,
		FirstResponseEventAtMS: started + 2, TerminalEventAtMS: ended, InputTokens: &input,
		CachedInputTokens: &cached, OutputTokens: &output, ReasoningTokens: &reasoning, TotalTokens: &total,
		Attempts: []gateway.AttemptObservation{{
			AccountID: "public-a", Ordinal: 1, StartedAtMS: started, EndedAtMS: ended, UpstreamHTTPStatus: 200,
			UsageSnapshotID: "snapshot-0", UsageSnapshotAtMS: 50, ShortUsedPercent: &short,
			ShortResetsAtMS: &shortReset, WeeklyUsedPercent: &weekly, WeeklyResetsAtMS: &weeklyReset,
		}},
	}
}

func TestRecordSettlesExactModelTotalsAndIsIdempotent(t *testing.T) {
	database, ctx := telemetryFixture(t)
	value := observation("response-1", 100, 200)
	result, err := Record(ctx, database, value)
	if err != nil || !result.Inserted || result.RefreshAccount != "public-a" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if again, err := Record(ctx, database, value); err != nil || again.Inserted {
		t.Fatalf("duplicate result=%+v err=%v", again, err)
	}
	short, weekly, shortReset, weeklyReset := int64(12), int64(23), int64(10_000), int64(20_000)
	err = database.Write(ctx, func(db store.Executor) error {
		return CompleteUsage(ctx, db, "account-a", UsageSnapshot{ID: "snapshot-1", AtMS: 250, ShortUsed: &short, ShortResetsAtMS: &shortReset, WeeklyUsed: &weekly, WeeklyResetsAtMS: &weeklyReset})
	})
	if err != nil {
		t.Fatal(err)
	}
	totals, err := ReadTotals(ctx, database.DB)
	if err != nil || len(totals) != 2 {
		t.Fatalf("totals=%+v err=%v", totals, err)
	}
	for _, item := range totals {
		if item.Model != "gpt-a" || item.Responses != 1 || item.ActiveMS != 100 || item.TotalTokens != 125 ||
			item.ShortSamples != 1 || item.ShortUsagePercent != 2 || item.WeeklySamples != 1 || item.WeeklyUsagePercent != 3 {
			t.Fatalf("unexpected totals: %+v", item)
		}
	}
}

func TestOverlappingMeasurementInvalidatesQuotaOnly(t *testing.T) {
	database, ctx := telemetryFixture(t)
	if _, err := Record(ctx, database, observation("response-1", 100, 200)); err != nil {
		t.Fatal(err)
	}
	short, weekly, shortReset, weeklyReset := int64(12), int64(23), int64(10_000), int64(20_000)
	if err := database.Write(ctx, func(db store.Executor) error {
		return CompleteUsage(ctx, db, "account-a", UsageSnapshot{ID: "snapshot-1", AtMS: 250, ShortUsed: &short, ShortResetsAtMS: &shortReset, WeeklyUsed: &weekly, WeeklyResetsAtMS: &weeklyReset})
	}); err != nil {
		t.Fatal(err)
	}
	second := observation("response-2", 150, 300)
	second.Attempts[0].UsageSnapshotID = "snapshot-0"
	if result, err := Record(ctx, database, second); err != nil || result.RefreshAccount != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var isolated int
	var shortDelta, weeklyDelta any
	if err := database.DB.QueryRowContext(ctx, "SELECT quota_isolated,short_usage_delta,weekly_usage_delta FROM proxy_responses WHERE proxy_request_id='response-1'").Scan(&isolated, &shortDelta, &weeklyDelta); err != nil {
		t.Fatal(err)
	}
	if isolated != 0 || shortDelta != nil || weeklyDelta != nil {
		t.Fatalf("isolated=%d short=%v weekly=%v", isolated, shortDelta, weeklyDelta)
	}
	totals, err := ReadTotals(ctx, database.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range totals {
		if item.Responses != 2 || item.ShortSamples != 0 || item.WeeklySamples != 0 {
			t.Fatalf("unexpected totals: %+v", item)
		}
	}
}

func TestPruneSubtractsRollingButPreservesLifetime(t *testing.T) {
	database, ctx := telemetryFixture(t)
	value := observation("response-1", 100, 200)
	value.Attempts = nil
	if _, err := Record(ctx, database, value); err != nil {
		t.Fatal(err)
	}
	if err := Prune(ctx, database, 201, 1000); err != nil {
		t.Fatal(err)
	}
	totals, err := ReadTotals(ctx, database.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range totals {
		want := int64(0)
		if item.Period == LifetimePeriod {
			want = 1
		}
		if item.Responses != want {
			t.Fatalf("period=%s responses=%d want=%d", item.Period, item.Responses, want)
		}
	}
}

func TestModelsRemainSeparate(t *testing.T) {
	database, ctx := telemetryFixture(t)
	first := observation("response-a", 100, 200)
	first.Attempts = nil
	second := observation("response-b", 300, 500)
	second.ResponseModel, second.Attempts = "gpt-b", nil
	if _, err := Record(ctx, database, first); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(ctx, database, second); err != nil {
		t.Fatal(err)
	}
	models := map[string]int64{}
	for _, item := range mustTotals(t, ctx, database) {
		if item.Period == LifetimePeriod {
			models[item.Model] = item.ActiveMS
		}
	}
	if models["gpt-a"] != 100 || models["gpt-b"] != 200 || len(models) != 2 {
		t.Fatalf("models=%v", models)
	}
}

func TestQuotaIgnoresZeroAndResetSpanningObservations(t *testing.T) {
	for _, test := range []struct {
		name                  string
		short, weekly         int64
		shortReset, weekReset int64
		wantReason            string
	}{
		{name: "below resolution", short: 10, weekly: 20, shortReset: 10_000, weekReset: 20_000, wantReason: "below_resolution"},
		{name: "window reset", short: 1, weekly: 1, shortReset: 11_000, weekReset: 21_000, wantReason: "window_reset"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, ctx := telemetryFixture(t)
			if _, err := Record(ctx, database, observation("response", 100, 200)); err != nil {
				t.Fatal(err)
			}
			if err := database.Write(ctx, func(db store.Executor) error {
				return CompleteUsage(ctx, db, "account-a", UsageSnapshot{ID: "snapshot-1", AtMS: 250,
					ShortUsed: &test.short, ShortResetsAtMS: &test.shortReset, WeeklyUsed: &test.weekly, WeeklyResetsAtMS: &test.weekReset})
			}); err != nil {
				t.Fatal(err)
			}
			var reason string
			if err := database.DB.QueryRowContext(ctx, "SELECT quota_invalid_reason FROM proxy_responses WHERE proxy_request_id='response'").Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if reason != test.wantReason {
				t.Fatalf("reason=%q want=%q", reason, test.wantReason)
			}
			for _, item := range mustTotals(t, ctx, database) {
				if item.ShortSamples != 0 || item.WeeklySamples != 0 {
					t.Fatalf("unexpected quota samples: %+v", item)
				}
			}
		})
	}
}

func mustTotals(t *testing.T, ctx context.Context, database *store.Store) []Totals {
	t.Helper()
	result, err := ReadTotals(ctx, database.DB)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func FuzzPositiveDelta(f *testing.F) {
	f.Add(int64(10), int64(12), int64(100), int64(100))
	f.Add(int64(10), int64(10), int64(100), int64(100))
	f.Add(int64(90), int64(1), int64(100), int64(200))
	f.Fuzz(func(t *testing.T, start, end, reset, endReset int64) {
		value, _ := positiveDelta(nullable(start), nullable(reset), &end, &endReset)
		if value != nil && (*value <= 0 || end <= start || reset != endReset) {
			t.Fatalf("invalid delta=%d start=%d end=%d reset=%d endReset=%d", *value, start, end, reset, endReset)
		}
	})
}

func nullable(value int64) (result sql.NullInt64) {
	result.Int64, result.Valid = value, true
	return result
}
