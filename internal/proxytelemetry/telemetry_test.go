package proxytelemetry

import (
	"context"
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
	return gateway.Observation{
		RequestID: id, AccountID: "public-a", Endpoint: "/v1/responses", RequestModel: "requested",
		ResponseModel: "gpt-a", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamHTTPStatus: 200,
		RequestStartedAtMS: started - 10, UpstreamStartedAtMS: started, ResponseHeadersAtMS: started + 1,
		FirstResponseEventAtMS: started + 2, TerminalEventAtMS: ended, InputTokens: &input,
		CachedInputTokens: &cached, OutputTokens: &output, ReasoningTokens: &reasoning, TotalTokens: &total,
	}
}

func TestRecordSettlesExactModelTotalsAndIsIdempotent(t *testing.T) {
	database, ctx := telemetryFixture(t)
	value := observation("response-1", 100, 200)
	result, err := Record(ctx, database, value)
	if err != nil || !result.Inserted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if again, err := Record(ctx, database, value); err != nil || again.Inserted {
		t.Fatalf("duplicate result=%+v err=%v", again, err)
	}
	totals, err := ReadTotals(ctx, database.DB)
	if err != nil || len(totals) != 2 {
		t.Fatalf("totals=%+v err=%v", totals, err)
	}
	for _, item := range totals {
		if item.Model != "gpt-a" || item.Responses != 1 || item.ActiveMS != 100 || item.TotalTokens != 125 {
			t.Fatalf("unexpected totals: %+v", item)
		}
	}
}

func TestPruneSubtractsRollingButPreservesLifetime(t *testing.T) {
	database, ctx := telemetryFixture(t)
	value := observation("response-1", 100, 200)
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
	second := observation("response-b", 300, 500)
	second.ResponseModel = "gpt-b"
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

func mustTotals(t *testing.T, ctx context.Context, database *store.Store) []Totals {
	t.Helper()
	result, err := ReadTotals(ctx, database.DB)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
