package usage

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const testRates = `{"gpt-test":{"input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"cache_read_input_token_cost":0.0000002,"input_cost_per_token_priority":0.000004,"output_cost_per_token_priority":0.00002},"partial":{"input_cost_per_token":1},"bad":{"input_cost_per_token":null,"output_cost_per_token":1}}`

func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Fatalf("got %g, want %g", got, want)
	}
}

func TestRatesRequireCompletePricesAndPreservePriorityCacheRatio(t *testing.T) {
	rates := ParseRates([]byte(testRates))
	if len(rates) != 1 {
		t.Fatalf("rates = %+v", rates)
	}
	fast, standard, ok := rates.Lookup("GPT-TEST", "priority")
	if !ok {
		t.Fatal("priority rate missing")
	}
	closeTo(t, fast.Cached, 0.0000004)
	closeTo(t, fast.Input, 2*standard.Input)
	if _, _, ok := rates.Lookup("gpt-test", "flex"); ok {
		t.Fatal("guessed an unsupported tier")
	}
	if _, _, ok := rates.Lookup("unknown", "default"); ok {
		t.Fatal("guessed an unknown model")
	}
}

func usageDatabase(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.DB.Exec(`INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','public-a','A',1,'ACTIVE',1,1),('b','public-b','B',1,'ACTIVE',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDailyUsagePricesFiltersDeduplicatesAndSurvivesPruning(t *testing.T) {
	db, ctx := usageDatabase(t), context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	input, cached, output, reasoning, total := int64(100), int64(80), int64(25), int64(10), int64(125)
	value := gateway.Observation{RequestID: "r1", AccountID: "public-a", Endpoint: "/v1/responses", ResponseModel: "gpt-test", ResponseServiceTier: "priority", RequestServiceTier: "default", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamHTTPStatus: 200, RequestStartedAtMS: now.Add(-time.Minute).UnixMilli(), UpstreamStartedAtMS: now.Add(-time.Minute).UnixMilli(), TerminalEventAtMS: now.UnixMilli(), InputTokens: &input, CachedInputTokens: &cached, OutputTokens: &output, ReasoningTokens: &reasoning, TotalTokens: &total}
	for range 2 {
		if _, err := proxytelemetry.Record(ctx, db, value); err != nil {
			t.Fatal(err)
		}
	}
	value.RequestID, value.AccountID, value.ResponseModel = "r2", "public-b", "unknown"
	if _, err := proxytelemetry.Record(ctx, db, value); err != nil {
		t.Fatal(err)
	}
	value.RequestID, value.InputTokens, value.OutputTokens = "r3", nil, nil
	if _, err := proxytelemetry.Record(ctx, db, value); err != nil {
		t.Fatal(err)
	}
	rates := ParseRates([]byte(testRates))
	summary, err := ReadSummary(ctx, db.DB, rates, "public-a", 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Responses != 1 || summary.ProcessedTokens != 125 || summary.UncachedInput != 20 || summary.CachedInput != 80 || summary.Output != 25 || len(summary.Daily) != 7 {
		t.Fatalf("summary=%+v", summary)
	}
	closeTo(t, summary.CostUSD, 0.000612)
	closeTo(t, summary.CacheSavingsUSD, 0.000288)
	closeTo(t, summary.Speed.Fast, summary.CostUSD)
	closeTo(t, summary.Speed.Premium, summary.CostUSD/2)
	closeTo(t, summary.Category.Input+summary.Category.CacheRead+summary.Category.Output, summary.CostUSD)
	all, err := ReadSummary(ctx, db.DB, rates, "all", 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if all.Responses != 3 || all.UnpricedResponses != 1 || all.MissingUsageResponses != 1 {
		t.Fatalf("all=%+v", all)
	}
	closeTo(t, all.CostUSD, summary.CostUSD)
	if err := proxytelemetry.Prune(ctx, db, now.Add(time.Hour).UnixMilli(), 100); err != nil {
		t.Fatal(err)
	}
	after, err := ReadSummary(ctx, db.DB, rates, "public-a", 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if after.Responses != 1 || after.ProcessedTokens != 125 {
		t.Fatalf("history lost after pruning: %+v", after)
	}
	closeTo(t, after.CostUSD, summary.CostUSD)
	outside, err := ReadSummary(ctx, db.DB, rates, "all", 7, now.AddDate(0, 0, 8))
	if err != nil || outside.Responses != 0 {
		t.Fatalf("window: %+v, %v", outside, err)
	}
}

func TestCatalogCachesAcrossRestartsAndFallsBackOffline(t *testing.T) {
	db, ctx := usageDatabase(t), context.Background()
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(testRates)) }))
	defer host.Close()
	catalog := NewCatalog(db)
	catalog.url = host.URL
	for range 2 {
		rates, status := catalog.Read(ctx)
		if len(rates) != 1 || status.Status != "fresh" {
			t.Fatalf("rates=%+v status=%+v", rates, status)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("fetched rates more than once")
	}
	restarted := NewCatalog(db)
	restarted.url = host.URL
	if rates, _ := restarted.Read(ctx); len(rates) != 1 || calls.Load() != 1 {
		t.Fatal("did not reuse persisted rates")
	}
	_, err := db.DB.Exec("UPDATE usage_rate_cache SET fetched_at_ms=?", time.Now().Add(-48*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	host.Close()
	offline := NewCatalog(db)
	offline.url = host.URL
	rates, status := offline.Read(ctx)
	if len(rates) != 1 || status.Status != "cached" {
		t.Fatalf("offline rates=%v status=%+v", rates, status)
	}
}

func TestCanceledCatalogReadDoesNotDelayNextFilter(t *testing.T) {
	db := usageDatabase(t)
	started := make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cancel") != "" {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(testRates))
	}))
	defer host.Close()
	catalog := NewCatalog(db)
	catalog.url = host.URL + "?cancel=1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); catalog.Read(ctx) }()
	<-started
	cancel()
	<-done
	catalog.url = host.URL
	if rates, status := catalog.Read(context.Background()); len(rates) != 1 || status.Status != "fresh" {
		t.Fatalf("next filter could not load rates: %+v %+v", rates, status)
	}
}
