package httpserver

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

func TestQuotaDashboardWeightsAccountsAndKeepsUnknownRatesNull(t *testing.T) {
	s, _ := proxyFixture(t, 2)
	ctx, now := context.Background(), time.Now()
	empty, err := s.quotaUsage(ctx, now)
	if err != nil || len(empty) != 1 || empty[0].Rolling.Short.PerHour != nil {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	start := now.Add(-time.Hour).UnixMilli()
	reset := now.Add(24 * time.Hour).Unix()
	if _, err := s.App.Store.DB.Exec(`UPDATE usage_current SET complete_read_at_ms=?,short_used_percent_raw=10,short_resets_at_s=?,weekly_used_percent_raw=20,weekly_resets_at_s=?`, start, reset, reset); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		account               string
		active, short, weekly int64
	}{
		{"account0", 5 * time.Minute.Milliseconds(), 12, 23},
		{"account1", 15 * time.Minute.Milliseconds(), 14, 26},
	} {
		_, err := proxytelemetry.Record(ctx, s.App.Store, gateway.Observation{RequestID: item.account, AccountID: item.account, Endpoint: "/v1/responses", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamHTTPStatus: 200, UpstreamStartedAtMS: start, TerminalEventAtMS: start + item.active})
		if err != nil {
			t.Fatal(err)
		}
		err = s.App.Store.Write(ctx, func(db store.Executor) error {
			return proxytelemetry.RecordUsage(ctx, db, item.account, proxytelemetry.QuotaSnapshot{ID: "poll-" + item.account, AtMS: now.UnixMilli(), Short: proxytelemetry.QuotaWindow{Used: &item.short, ResetS: &reset}, Weekly: proxytelemetry.QuotaWindow{Used: &item.weekly, ResetS: &reset}})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.quotaUsage(ctx, now)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for index, want := range []float64{18, 24, 16} {
		for _, period := range []quotaPeriod{rows[index].Rolling, rows[index].Lifetime} {
			if period.Short.PerHour == nil || math.Abs(*period.Short.PerHour-want) > 0.0001 {
				t.Fatalf("account=%s rate=%v want=%f", rows[index].AccountID, period.Short.PerHour, want)
			}
		}
	}
	if rows[0].Rolling.Weekly.PerHour == nil || *rows[0].Rolling.Weekly.PerHour != 27 {
		t.Fatalf("weekly=%+v", rows[0].Rolling.Weekly)
	}
	// Rolling membership changes immediately, independent of maintenance timing.
	later, err := s.quotaUsage(ctx, now.Add(proxytelemetry.QuotaRetention+time.Millisecond))
	if err != nil || later[0].Rolling.Short.PerHour != nil || later[0].Lifetime.Short.PerHour == nil {
		t.Fatalf("later=%+v err=%v", later, err)
	}
}
