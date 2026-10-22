package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"

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
		{name: "short exhaustion keeps weekly reading", accounts: []broker.AccountSummary{account(20, 40, false), account(100, 60, false)}, shortUsed: 20, weeklyUsed: 50, shortCount: 1, weeklyCount: 2, shortExcluded: 1},
		{name: "rejection keeps partial weekly usage", accounts: []broker.AccountSummary{account(20, 40, false), account(80, 60, true)}, shortUsed: 20, weeklyUsed: 50, shortCount: 1, weeklyCount: 2, shortExcluded: 1},
		{name: "weekly full excludes unused short capacity", accounts: []broker.AccountSummary{account(20, 40, false), account(0, 100, false)}, shortUsed: 20, weeklyUsed: 70, shortCount: 1, weeklyCount: 2, shortExcluded: 1},
		{name: "all blocked", accounts: []broker.AccountSummary{account(80, 40, true)}, weeklyUsed: 40, weeklyCount: 1, shortExcluded: 1},
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
	err = renderer.render(response, 200, "dashboard.html", pongo2.Context{"pool_usage": []poolWindow{{Label: "5-hour window", Used: 25, Remaining: 75, Accounts: 2}, {Label: "Weekly window", Unknown: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, want := range []string{"Total pool usage", "25.0% used, 75.0% left", "--pool-used: 25.0%", "Weekly window: no measured capacity", "2 accounts included", "1 unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
