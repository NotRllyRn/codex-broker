package httpserver

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/flosch/pongo2/v6"
)

func TestPoolUsage(t *testing.T) {
	number := func(n int64) *int64 { return &n }
	account := func(short, weekly int64, blocked bool) broker.AccountSummary {
		return broker.AccountSummary{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(short), WeeklyPercent: number(weekly), QuotaBlocked: blocked}
	}
	tests := []struct {
		name                                                                string
		accounts                                                            []broker.AccountSummary
		shortUsed, weeklyUsed                                               float64
		shortCount, weeklyCount, shortUnknown, weeklyUnknown, shortExcluded int
	}{
		{name: "empty"},
		{name: "equal shares", accounts: []broker.AccountSummary{account(20, 40, false), account(60, 80, false)}, shortUsed: 40, weeklyUsed: 60, shortCount: 2, weeklyCount: 2},
		{name: "short exhaustion keeps weekly reading", accounts: []broker.AccountSummary{account(20, 40, false), account(100, 60, false)}, shortUsed: 60, weeklyUsed: 50, shortCount: 2, weeklyCount: 2},
		{name: "rejection keeps partial weekly usage", accounts: []broker.AccountSummary{account(20, 40, false), account(80, 60, true)}, shortUsed: 60, weeklyUsed: 50, shortCount: 2, weeklyCount: 2},
		{name: "weekly full consumes short capacity", accounts: []broker.AccountSummary{account(20, 40, false), account(0, 100, false)}, shortUsed: 60, weeklyUsed: 70, shortCount: 2, weeklyCount: 2},
		{name: "all blocked", accounts: []broker.AccountSummary{account(80, 40, true)}, shortUsed: 100, weeklyUsed: 40, shortCount: 1, weeklyCount: 1},
		{name: "weekly full without short reading", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", WeeklyPercent: number(100)}}, shortUsed: 100, weeklyUsed: 100, shortCount: 1, weeklyCount: 1},
		{name: "blocked without readings", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", QuotaBlocked: true}}, shortUsed: 100, shortCount: 1, weeklyUnknown: 1},
		{name: "blocked with invalid short reading", accounts: []broker.AccountSummary{account(-1, 40, true)}, shortUsed: 100, weeklyUsed: 40, shortCount: 1, weeklyCount: 1},
		{name: "zero usage", accounts: []broker.AccountSummary{account(0, 0, false)}, shortCount: 1, weeklyCount: 1},
		{name: "unavailable exhausted accounts stay excluded", accounts: []broker.AccountSummary{{QuotaBlocked: true, WeeklyPercent: number(100)}, {Enabled: true, AuthState: "AUTH_REQUIRED", ShortPercent: number(100), WeeklyPercent: number(100)}}, weeklyUsed: 100, weeklyCount: 2, shortExcluded: 2},
		{name: "missing weekly is not full", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(20)}}, shortUsed: 20, shortCount: 1, weeklyUnknown: 1},
		{name: "missing and invalid readings", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED"}, account(-1, 101, false)}, shortUnknown: 2, weeklyUnknown: 2},
		{name: "unavailable accounts still count weekly", accounts: []broker.AccountSummary{{ShortPercent: number(20), WeeklyPercent: number(40)}, {Enabled: true, AuthState: "AUTH_REQUIRED", ShortPercent: number(20), WeeklyPercent: number(60)}}, weeklyUsed: 50, weeklyCount: 2, shortExcluded: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := poolUsage(tt.accounts)
			if got[0].Used != tt.shortUsed || got[1].Used != tt.weeklyUsed || got[0].Accounts != tt.shortCount || got[1].Accounts != tt.weeklyCount || got[0].Unknown != tt.shortUnknown || got[1].Unknown != tt.weeklyUnknown || got[0].Excluded != tt.shortExcluded {
				t.Fatalf("pool=%+v", got)
			}
			for _, window := range got {
				if window.Accounts > 0 && window.Used+window.Remaining != 100 {
					t.Fatalf("used/left=%+v", window)
				}
				if window.Accounts == 0 && window.Remaining != 0 {
					t.Fatal("empty pool fabricates remaining capacity")
				}
			}
		})
	}
}

func TestPoolUsageDashboard(t *testing.T) {
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	err = renderer.render(response, 200, "dashboard.html", pongo2.Context{"next_available_reset": &poolReset{Label: "5-hour", Hours: 2.5}, "pool_usage": []poolWindow{{Label: "5-hour window", Used: 25, Remaining: 75, Accounts: 2, Available: 1}, {Label: "Weekly window", Unknown: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	if strings.Count(body, "Next available reset: 2.5 hours (5-hour)") != 2 {
		t.Error("reset should appear in both pool and account sections, even without matching accounts")
	}
	if strings.Count(body, "account available") != 1 {
		t.Error("availability should appear once for both rings")
	}
	for _, want := range []string{"Total pool usage", "25.0% used, 75.0% left", `stroke-dasharray="25.0 100"`, `25.0%<small>used</small>`, "Weekly window: no measured capacity", "1 account available"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestPoolUsageRingPercentages(t *testing.T) {
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, percent := range []float64{0, 25, 62.5, 100} {
		response := httptest.NewRecorder()
		err := renderer.render(response, 200, "dashboard.html", pongo2.Context{"pool_usage": []poolWindow{{Label: "5-hour window", Used: percent, Remaining: 100 - percent, Accounts: 1}, {Label: "Weekly window", Unknown: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		body := response.Body.String()
		for _, want := range []string{fmt.Sprintf(`stroke-dasharray="%.1f 100"`, percent), fmt.Sprintf(`%.1f%%<small>used</small>`, percent), `—<small>No capacity</small>`} {
			if !strings.Contains(body, want) {
				t.Errorf("percent=%v: missing %q", percent, want)
			}
		}
		if strings.Count(body, `class="pool-ring-fill"`) != 1 {
			t.Fatal("unknown usage should not render a filled arc")
		}
	}
}

func TestPoolAvailableAccounts(t *testing.T) {
	number := func(n int64) *int64 { return &n }
	accounts := []broker.AccountSummary{
		{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(20), WeeklyPercent: number(40)},
		{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(100), WeeklyPercent: number(40)},
		{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(0), WeeklyPercent: number(100)},
		{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(0), QuotaBlocked: true},
		{AuthState: "VERIFIED", ShortPercent: number(0)},
		{Enabled: true, AuthState: "AUTH_REQUIRED", ShortPercent: number(0)},
		{Enabled: true, AuthState: "VERIFIED"},
		{Enabled: true, AuthState: "VERIFIED", ShortPercent: number(-1)},
	}
	if got := poolUsage(accounts)[0].Available; got != 1 {
		t.Fatalf("available = %d, want 1", got)
	}
}

func TestNextAvailableReset(t *testing.T) {
	now := time.Unix(1700000000, 0)
	number := func(n int64) *int64 { return &n }
	reset := func(hours int) *int64 { return number(now.Add(time.Duration(hours) * time.Hour).UnixMilli()) }
	tests := []struct {
		name     string
		accounts []broker.AccountSummary
		label    string
		hours    float64
	}{
		{name: "empty"},
		{name: "earliest short", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", ShortResetMS: reset(2), WeeklyResetMS: reset(10)}}, label: "5-hour", hours: 2},
		{name: "weekly full skips short even if blocked", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", QuotaBlocked: true, WeeklyPercent: number(100), ShortResetMS: reset(1), WeeklyResetMS: reset(8)}, {Enabled: true, AuthState: "VERIFIED", WeeklyPercent: number(40), ShortResetMS: reset(3)}}, label: "5-hour", hours: 3},
		{name: "weekly restores exhausted account", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", QuotaBlocked: true, ShortPercent: number(100), WeeklyPercent: number(100), ShortResetMS: reset(1), WeeklyResetMS: reset(2)}}, label: "Weekly", hours: 2},
		{name: "ignore unavailable and stale resets", accounts: []broker.AccountSummary{{AuthState: "VERIFIED", WeeklyResetMS: reset(1)}, {Enabled: true, AuthState: "AUTH_REQUIRED", WeeklyResetMS: reset(2)}, {Enabled: true, AuthState: "VERIFIED", ShortResetMS: reset(-1), WeeklyResetMS: reset(0)}}},
		{name: "blocked short may recover", accounts: []broker.AccountSummary{{Enabled: true, AuthState: "VERIFIED", QuotaBlocked: true, WeeklyPercent: number(30), ShortResetMS: reset(1)}}, label: "5-hour", hours: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextAvailableReset(tt.accounts, now)
			if tt.label == "" {
				if got != nil {
					t.Fatalf("unexpected reset: %+v", got)
				}
			} else if got == nil || got.Label != tt.label || got.Hours != tt.hours {
				t.Fatalf("reset = %+v, want %s in %v hours", got, tt.label, tt.hours)
			}
		})
	}
}
