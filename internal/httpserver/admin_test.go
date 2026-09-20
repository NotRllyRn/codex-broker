package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/flosch/pongo2/v6"
)

func TestDashboardRendersUsageResetTimes(t *testing.T) {
	short, weekly := int64(1789714203000), int64(1790185173000)
	account := accountView(broker.AccountSummary{DisplayName: "Account", OverallState: "HEALTHY", ShortResetMS: &short, WeeklyResetMS: &weekly})
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	err = renderer.render(response, 200, "dashboard.html", pongo2.Context{"accounts": []map[string]any{account}, "attention": []map[string]any{}, "profiles": []broker.ProfileDashboard{{ID: "all", Label: "All accounts"}}, "profiles_json": `[{"id":"all","label":"All accounts","lifetime_tokens":2708142387,"updated_at_ms":1790194203000,"daily_usage_buckets":[],"token_shares":[]}]`, "counts": map[string]int{"HEALTHY": 1}, "status_options": []string{}, "selected_states": map[string]bool{}, "vault_configured": true})
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, expected := range []string{"2026-09-18T06:50:03.000000Z", "2026-09-23T17:39:33.000000Z"} {
		if !strings.Contains(body, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	for _, expected := range []string{"Account activity", "Lifetime tokens", "Tokens by account", `value="all"`, "data-activity-tooltip"} {
		if !strings.Contains(body, expected) {
			t.Errorf("profile dashboard does not contain %q", expected)
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
