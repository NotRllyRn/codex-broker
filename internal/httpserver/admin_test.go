package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/flosch/pongo2/v6"
)

func TestDashboardShowsCycleStatusAndNextPulse(t *testing.T) {
	short, weekly := int64(1789714203000), int64(1790185173000)
	shortUsed, weeklyUsed := int64(25), int64(70)
	account := accountView(broker.AccountSummary{DisplayName: "Account", OverallState: "HEALTHY", ShortPercent: &shortUsed, WeeklyPercent: &weeklyUsed, ShortResetMS: &short, WeeklyResetMS: &weekly, CycleState: "HELD"})
	routed := int64(1790185000000)
	gap := (18*time.Hour + 40*time.Minute).Milliseconds()
	rotating := accountView(broker.AccountSummary{DisplayName: "Rotating", OverallState: "HEALTHY", CycleState: "IN_CYCLE", CyclePosition: 2, CycleSize: 5, CycleGapMS: &gap, LastRoutedMS: &routed})
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	err = renderer.render(response, 200, "dashboard.html", pongo2.Context{"accounts": []map[string]any{account, rotating}, "attention": []map[string]any{}, "profiles": []broker.ProfileDashboard{{ID: "all", Label: "All accounts"}}, "profiles_json": `[{"id":"all","label":"All accounts","lifetime_tokens":2708142387,"updated_at_ms":1790194203000,"daily_usage_buckets":[],"token_shares":[]}]`, "counts": map[string]int{"HEALTHY": 2}, "status_options": []string{}, "selected_states": map[string]bool{}, "vault_configured": true, "cycle_enabled": true, "cycle_spacing": "18h40m", "cycle_next_at": "2026-09-23T17:39:33.000000Z", "cycle_next_ms": weekly})
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, expected := range []string{"2026-09-18T06:50:03.000000Z", "2026-09-23T17:39:33.000000Z"} {
		if !strings.Contains(body, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	for _, expected := range []string{"Account activity", "Lifetime tokens", "Tokens by account", `value="all"`, "data-activity-tooltip", "data-proxy-card", "OpenAI proxy", "Redundant relay edges", "data-relay-summary", "data-proxy-relays", "Since broker restart"} {
		if !strings.Contains(body, expected) {
			t.Errorf("profile dashboard does not contain %q", expected)
		}
	}
	for _, expected := range []string{"Held", "In Rotation", "25%", "70%", "5h used", "7d used", "Rotation #2", "18h40m from previous activation", "Intended spacing", "Next random pulse", `data-countdown="1790185173000"`, `datetime="2026-09-23T17:39:33.000000Z"`, `datetime="2026-09-18T06:50:03.000000Z"`} {
		if !strings.Contains(body, expected) {
			t.Errorf("cycle status does not contain %q", expected)
		}
	}
	for _, removed := range []string{"Activity insights", "Last selected", "Weekly release order", "Next cycle pulse", `action="/accounts//pulse"`} {
		if strings.Contains(body, removed) {
			t.Errorf("dashboard still contains %q", removed)
		}
	}
	for _, expected := range []string{`"lifetime_tokens":2708142387`, `"updated_at_ms":1790194203000`} {
		if !strings.Contains(body, expected) {
			t.Errorf("embedded profile data does not contain %q", expected)
		}
	}
	if strings.Contains(body, "data-activity-mode") {
		t.Error("dashboard still renders token activity mode controls")
	}
	if strings.Count(body, "data-proxy-model") != 1 || strings.Contains(body, "data-quota-account") || strings.Contains(body, "data-quota-period") {
		t.Error("proxy statistics must share one model selector and period panels")
	}
	for _, field := range []string{"tokens_per_hour", "short_rate", "weekly_rate"} {
		if strings.Count(body, `="`+field+`"`) != 2 {
			t.Errorf("%s must appear in both rolling and lifetime panels", field)
		}
	}
}

func TestExhaustedAccountKeepsWindowDashboardStatus(t *testing.T) {
	full, reset := int64(100), int64(1790185173000)
	view := accountView(broker.AccountSummary{OverallState: "HEALTHY", QuotaBlocked: true, ShortPercent: &full, WeeklyPercent: &full, ShortResetMS: &reset, WeeklyResetMS: &reset})
	if view["overall_state"] != "HEALTHY" || view["cycle_label"] != "Usage exhausted" || view["short_percent"] != &full || view["weekly_percent"] != &full || view["short_reset"] == nil || view["weekly_reset"] == nil {
		t.Fatalf("exhausted account dashboard status=%+v", view)
	}
}

func TestAccountDetailRendersPulseHistory(t *testing.T) {
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	detail := map[string]any{
		"account": map[string]any{"display_name": "Account", "public_token": "public", "labels": []string{}, "enabled": true},
		"usage":   map[string]any{},
		"pulse_operations": []map[string]any{{
			"operation_id": "pulse", "trigger": "CYCLE_RELEASE", "state": "SUCCEEDED", "created_at": "2026-09-30T13:54:47.000000Z", "progress_summary": "Usage windows kept active",
		}},
		"operations": []map[string]any{},
		"incidents":  []map[string]any{},
	}
	if err := renderer.render(response, 200, "account_detail.html", pongo2.Context{"detail": detail}); err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, expected := range []string{`action="/accounts/public/pulse"`, ">Pulse</button>", "Recent window pulses", "latest 100 pulses", "Cycle Release pulse", "Succeeded", "2026-09-30T13:54:47.000000Z", "Usage windows kept active"} {
		if !strings.Contains(body, expected) {
			t.Errorf("pulse history does not contain %q", expected)
		}
	}
}

func TestAuthExportRouteRemoved(t *testing.T) {
	server, _ := proxyFixture(t, 0)
	response := httptest.NewRecorder()
	server.mux.ServeHTTP(response, httptest.NewRequest("POST", "/accounts/account/auth-export", nil))
	if response.Code != 405 {
		t.Fatalf("removed credential download returned %d", response.Code)
	}
}

func TestDashboardAvailableFilter(t *testing.T) {
	server, _ := proxyFixture(t, 3)
	var err error
	server.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, err = server.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest("dashboard-test"), auth.Digest("csrf"), now.UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli(), nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.App.Store.DB.Exec(`UPDATE usage_current SET short_used_percent_raw=100 WHERE account_id='account1'; UPDATE usage_current SET weekly_used_percent_raw=100 WHERE account_id='account2'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		query     string
		want      int
		available bool
	}{
		{"", 1, true}, {"?state_filter=", 3, false}, {"?state_filter=AVAILABLE", 1, true}, {"?state_filter=HEALTHY", 3, false}, {"?q=account1", 0, true},
	} {
		t.Run(tt.query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/"+tt.query, nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "dashboard-test"})
			response := httptest.NewRecorder()
			if err := server.dashboardView(response, request); err != nil {
				t.Fatal(err)
			}
			body := response.Body.String()
			if got := strings.Count(body, `class="orbit-account state-`); got != tt.want {
				t.Fatalf("got %d account cards, want %d", got, tt.want)
			}
			if !strings.Contains(body, "1 available") || strings.Count(body, "Next available reset:") != 2 {
				t.Fatal("pool summary must remain visible regardless of filters")
			}
			if tt.available && !strings.Contains(body, `value="AVAILABLE"
        selected`) {
				t.Fatal("Available is not selected")
			}
		})
	}
}
