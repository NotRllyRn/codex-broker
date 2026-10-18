package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	for _, removed := range []string{"Last selected", "Weekly release order", "Next cycle pulse", `action="/accounts//pulse"`} {
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
		"operations":  []map[string]any{},
		"incidents":   []map[string]any{},
		"auth_export": map[string]any{"available": false},
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
