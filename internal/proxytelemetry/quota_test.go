package proxytelemetry

import (
	"context"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func quotaPointer(value int64) *int64 { return &value }

func measureQuota(t *testing.T, db *store.Store, id string, end, short, weekly int64) {
	t.Helper()
	snapshot := QuotaSnapshot{ID: id, AtMS: end, Short: QuotaWindow{quotaPointer(short), quotaPointer(10)}, Weekly: QuotaWindow{quotaPointer(weekly), quotaPointer(20)}}
	err := db.Write(context.Background(), func(tx store.Executor) error {
		// A repeated delivery must not add the same interval twice.
		for range 2 {
			if err := RecordUsage(context.Background(), tx, "account-a", snapshot); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(context.Background(), `UPDATE usage_current SET snapshot_id=?,complete_read_at_ms=?,short_used_percent_raw=?,weekly_used_percent_raw=? WHERE account_id='account-a'`, id, end, short, weekly)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func expectQuota(t *testing.T, db *store.Store, cutoff int64, period, window string, samples, used, active int64) {
	t.Helper()
	rows, err := ReadQuotaTotals(context.Background(), db.DB, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	var found QuotaTotals
	for _, row := range rows {
		if row.Period == period && row.Window == window && row.AccountID == "public-a" && row.Model == "" {
			found = row
		}
	}
	if found.Samples != samples || found.UsedPercent != used || found.ActiveMS != active {
		t.Fatalf("%s/%s: %+v want samples=%d used=%d active=%d", period, window, found, samples, used, active)
	}
}

func TestQuotaIntervalsMergeActivityAndWeightMeasurements(t *testing.T) {
	db, ctx := telemetryFixture(t)
	for _, response := range []struct {
		id         string
		start, end int64
	}{
		{"a", 25, 150}, {"b", 100, 200}, {"c", 200, 300},
	} {
		if _, err := Record(ctx, db, observation(response.id, response.start, response.end)); err != nil {
			t.Fatal(err)
		}
	}
	measureQuota(t, db, "poll-1", 250, 12, 23)
	// Clip at both poll boundaries, and count overlapping/adjacent spans once.
	expectQuota(t, db, 0, LifetimePeriod, "short", 1, 2, 200)
	expectQuota(t, db, 0, RollingPeriod, "weekly", 1, 3, 200)
	if _, err := Record(ctx, db, observation("d", 300, 600)); err != nil {
		t.Fatal(err)
	}
	measureQuota(t, db, "poll-2", 650, 13, 25)
	for _, period := range []string{RollingPeriod, LifetimePeriod} {
		expectQuota(t, db, 0, period, "short", 2, 3, 550)
		expectQuota(t, db, 0, period, "weekly", 2, 5, 550)
	}
}

func TestQuotaLateResponseUpdatesBothPollIntervalsIdempotently(t *testing.T) {
	db, ctx := telemetryFixture(t)
	measureQuota(t, db, "poll-1", 250, 12, 23)
	measureQuota(t, db, "poll-2", 450, 15, 25)
	expectQuota(t, db, 0, LifetimePeriod, "short", 0, 0, 0)
	for range 2 {
		if _, err := Record(ctx, db, observation("late", 100, 500)); err != nil {
			t.Fatal(err)
		}
	}
	for _, period := range []string{RollingPeriod, LifetimePeriod} {
		expectQuota(t, db, 0, period, "short", 2, 5, 350)
		expectQuota(t, db, 0, period, "weekly", 2, 5, 350)
	}
}

func TestQuotaSurvivesResponsePruningAndLifetimeSurvivesIntervalExpiry(t *testing.T) {
	db, ctx := telemetryFixture(t)
	if _, err := Record(ctx, db, observation("early", 100, 200)); err != nil {
		t.Fatal(err)
	}
	measureQuota(t, db, "poll", 300, 12, 23)
	if err := Prune(ctx, db, 250, 1000); err != nil {
		t.Fatal(err)
	}
	var responses int
	if err := db.DB.QueryRow("SELECT count(*) FROM proxy_responses").Scan(&responses); err != nil || responses != 0 {
		t.Fatalf("responses=%d err=%v", responses, err)
	}
	if _, err := Record(ctx, db, observation("late", 150, 275)); err != nil {
		t.Fatal(err)
	}
	// The original 100–200 span was retained independently of its response row.
	expectQuota(t, db, 250, RollingPeriod, "short", 1, 2, 175)
	expectQuota(t, db, 301, RollingPeriod, "short", 0, 0, 0) // Exact cutoff without waiting for pruning.
	if err := Prune(ctx, db, 301, 1000); err != nil {
		t.Fatal(err)
	}
	expectQuota(t, db, 0, RollingPeriod, "short", 0, 0, 0)
	expectQuota(t, db, 0, LifetimePeriod, "short", 1, 2, 175)
	if _, err := Record(ctx, db, observation("too-late", 50, 300)); err != nil {
		t.Fatal(err)
	}
	expectQuota(t, db, 0, LifetimePeriod, "short", 1, 2, 175)
}

func TestQuotaIncludesMeasuredZeroConsumption(t *testing.T) {
	db, ctx := telemetryFixture(t)
	if _, err := Record(ctx, db, observation("a", 100, 200)); err != nil {
		t.Fatal(err)
	}
	measureQuota(t, db, "poll-1", 250, 10, 20)
	if _, err := Record(ctx, db, observation("b", 300, 400)); err != nil {
		t.Fatal(err)
	}
	measureQuota(t, db, "poll-2", 450, 12, 22)
	expectQuota(t, db, 0, LifetimePeriod, "short", 2, 2, 200)
	expectModelQuota(t, db, 0, LifetimePeriod, "gpt-a", 2, 2, 200)
}

func TestQuotaRejectsResetsAndInvalidSamplesIndependently(t *testing.T) {
	for _, test := range []struct {
		name        string
		used, reset *int64
		end         int64
	}{
		{"changed reset", quotaPointer(12), quotaPointer(11), 250},
		{"decreasing", quotaPointer(9), quotaPointer(10), 250},
		{"negative", quotaPointer(-1), quotaPointer(10), 250},
		{"over 100", quotaPointer(101), quotaPointer(10), 250},
		{"missing usage", nil, quotaPointer(10), 250},
		{"missing reset", quotaPointer(12), nil, 250},
		{"expired reset", quotaPointer(12), quotaPointer(10), 10000},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, ctx := telemetryFixture(t)
			if _, err := Record(ctx, db, observation("a", 100, 200)); err != nil {
				t.Fatal(err)
			}
			err := db.Write(ctx, func(tx store.Executor) error {
				return RecordUsage(ctx, tx, "account-a", QuotaSnapshot{ID: "poll", AtMS: test.end, Short: QuotaWindow{test.used, test.reset}, Weekly: QuotaWindow{quotaPointer(23), quotaPointer(20)}})
			})
			if err != nil {
				t.Fatal(err)
			}
			expectQuota(t, db, 0, LifetimePeriod, "short", 0, 0, 0)
			expectQuota(t, db, 0, LifetimePeriod, "weekly", 1, 3, 100)
		})
	}
}

func TestQuotaIntervalsAndTotalsSurviveRestart(t *testing.T) {
	db, ctx := telemetryFixture(t)
	measureQuota(t, db, "poll", 250, 12, 23)
	path := db.Path
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := Record(ctx, reopened, observation("late", 100, 200)); err != nil {
		t.Fatal(err)
	}
	expectQuota(t, reopened, 0, LifetimePeriod, "short", 1, 2, 100)
}

func TestQuotaRejectsIntervalsOlderThanResponseRetention(t *testing.T) {
	db, ctx := telemetryFixture(t)
	end := int64(51) + QuotaRetention.Milliseconds()
	reset := end/1000 + 10000
	if _, err := db.DB.Exec("UPDATE usage_current SET short_resets_at_s=?,weekly_resets_at_s=?", reset, reset); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(ctx, db, observation("a", 100, 200)); err != nil {
		t.Fatal(err)
	}
	err := db.Write(ctx, func(tx store.Executor) error {
		return RecordUsage(ctx, tx, "account-a", QuotaSnapshot{ID: "poll", AtMS: end, Short: QuotaWindow{quotaPointer(12), &reset}, Weekly: QuotaWindow{quotaPointer(23), &reset}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow("SELECT count(*) FROM quota_intervals").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired baseline intervals=%d err=%v", count, err)
	}
}

func expectModelQuota(t *testing.T, db *store.Store, cutoff int64, period, model string, samples, used, active int64) {
	t.Helper()
	rows, err := ReadQuotaTotals(context.Background(), db.DB, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	var found QuotaTotals
	for _, row := range rows {
		if row.AccountID == "public-a" && row.Model == model && row.Period == period && row.Window == "short" {
			found = row
		}
	}
	if found.Samples != samples || found.UsedPercent != used || found.ActiveMS != active {
		t.Fatalf("%s/%s: %+v want %d/%d/%d", period, model, found, samples, used, active)
	}
}

func TestQuotaModelsExcludeMixedAndUnknownIntervals(t *testing.T) {
	for _, test := range []struct {
		name, model string
		start, end  int64
		attributed  bool
	}{
		{"same model overlap", "gpt-a", 150, 250, true},
		{"mixed overlap", "gpt-b", 150, 250, false},
		{"mixed sequential", "gpt-b", 220, 250, false},
		{"unknown model", "", 150, 250, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, ctx := telemetryFixture(t)
			if _, err := Record(ctx, db, observation("first", 100, 200)); err != nil {
				t.Fatal(err)
			}
			value := observation("second", test.start, test.end)
			value.ResponseModel, value.RequestModel = test.model, test.model
			if _, err := Record(ctx, db, value); err != nil {
				t.Fatal(err)
			}
			measureQuota(t, db, "poll", 300, 12, 23)
			for _, period := range []string{RollingPeriod, LifetimePeriod} {
				if test.attributed {
					expectModelQuota(t, db, 0, period, "gpt-a", 1, 2, 150)
				} else {
					expectModelQuota(t, db, 0, period, "gpt-a", 0, 0, 0)
					expectModelQuota(t, db, 0, period, "gpt-b", 0, 0, 0)
				}
			}
		})
	}
}

func TestQuotaLateModelRetractsAttributionAfterResponsePruning(t *testing.T) {
	db, ctx := telemetryFixture(t)
	if _, err := Record(ctx, db, observation("first", 100, 200)); err != nil {
		t.Fatal(err)
	}
	measureQuota(t, db, "poll", 300, 12, 23)
	if err := Prune(ctx, db, 250, 100); err != nil {
		t.Fatal(err)
	}
	expectModelQuota(t, db, 0, LifetimePeriod, "gpt-a", 1, 2, 100)
	// The late span is wholly contained: time does not change, attribution does.
	value := observation("late", 120, 180)
	value.ResponseModel = "gpt-b"
	for range 2 {
		if _, err := Record(ctx, db, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, period := range []string{RollingPeriod, LifetimePeriod} {
		expectModelQuota(t, db, 0, period, "gpt-a", 0, 0, 0)
		expectModelQuota(t, db, 0, period, "gpt-b", 0, 0, 0)
		expectQuota(t, db, 0, period, "short", 1, 2, 100)
	}
}

func TestQuotaModelLifetimeSurvivesRestartAndExpiry(t *testing.T) {
	db, ctx := telemetryFixture(t)
	measureQuota(t, db, "first", 250, 12, 23)
	measureQuota(t, db, "second", 450, 15, 25)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, value := range []struct {
		id         string
		start, end int64
	}{{"late", 100, 500}, {"overlap", 110, 200}} {
		if _, err := Record(ctx, reopened, observation(value.id, value.start, value.end)); err != nil {
			t.Fatal(err)
		}
	}
	for _, period := range []string{RollingPeriod, LifetimePeriod} {
		expectModelQuota(t, reopened, 0, period, "gpt-a", 2, 5, 350)
	}
	// Query cutoff works before maintenance; lifetime never expires with detail.
	expectModelQuota(t, reopened, 451, RollingPeriod, "gpt-a", 0, 0, 0)
	if err := Prune(ctx, reopened, 600, 100); err != nil {
		t.Fatal(err)
	}
	expectModelQuota(t, reopened, 0, RollingPeriod, "gpt-a", 0, 0, 0)
	expectModelQuota(t, reopened, 0, LifetimePeriod, "gpt-a", 2, 5, 350)
}

func TestQuotaIrregularReadsSplitDelayedModelActivityConservatively(t *testing.T) {
	db, ctx := telemetryFixture(t)
	// Uneven observations, including a rounded zero-point change, must use
	// actual activity rather than the polling interval as the denominator.
	for _, poll := range []struct {
		id       string
		at, used int64
	}{
		{"manual", 113, 10}, {"scheduled", 900, 14}, {"reset-check", 1600, 16},
	} {
		measureQuota(t, db, poll.id, poll.at, poll.used, 20)
	}
	for _, response := range []struct {
		id, model  string
		start, end int64
	}{
		{"long", "gpt-a", 100, 1400}, {"overlap", "gpt-a", 110, 1000}, {"switch", "gpt-b", 1000, 1500},
	} {
		value := observation(response.id, response.start, response.end)
		value.RequestModel, value.ResponseModel = response.model, response.model
		for range 2 {
			if _, err := Record(ctx, db, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, period := range []string{RollingPeriod, LifetimePeriod} {
		// Union [100,1500], never summed stream durations or elapsed poll time.
		expectQuota(t, db, 0, period, "short", 3, 6, 1400)
		// First two intervals contain only gpt-a. The third is mixed, so its
		// entire delta remains unassigned rather than guessing a model share.
		expectModelQuota(t, db, 0, period, "gpt-a", 2, 4, 800)
		expectModelQuota(t, db, 0, period, "gpt-b", 0, 0, 0)
	}
}
