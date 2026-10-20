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
		if row.Period == period && row.Window == window && row.AccountID == "public-a" {
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
