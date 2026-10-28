package broker

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

func configureEnrollmentWebhooks(t *testing.T, s *Service) {
	t.Helper()
	s.Webhooks = webhook.New(s.Store, s.Vault)
	t.Cleanup(s.Webhooks.Close)
	for _, kind := range []string{"generic", "slack", "discord", "disabled"} {
		format := kind
		if kind == "disabled" {
			format = "generic"
		}
		id, err := s.Webhooks.Create(t.Context(), kind, "https://hooks.example.com/enrollment", "", format)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "disabled" {
			if err := s.Webhooks.SetEnabled(t.Context(), id, false); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertEnrollmentEventCount(t *testing.T, s *Service, want int) {
	t.Helper()
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM webhook_events WHERE event_type='account.enrolled'").Scan(&count); err != nil || count != want {
		t.Fatalf("enrollment events=%d want=%d err=%v", count, want, err)
	}
}

func assertEnrollmentPayload(t *testing.T, s *Service, public string, labels []string, available bool) {
	t.Helper()
	var raw []byte
	if err := s.Store.DB.QueryRow("SELECT canonical_body FROM webhook_events WHERE event_type='account.enrolled'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Data struct {
			ID     string             `json:"account_id"`
			Email  string             `json:"account_email"`
			Labels []string           `json:"labels"`
			Usage  map[string]*Window `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event.Data.ID != public || event.Data.Email != "owner@example.com" || !reflect.DeepEqual(event.Data.Labels, labels) {
		t.Fatalf("payload=%s", raw)
	}
	for _, key := range []string{"short", "weekly"} {
		window := event.Data.Usage[key]
		if available && (window == nil || window.UsedPercent == nil || window.ResetsAtS == nil || *window.UsedPercent != 0) {
			t.Fatalf("missing usage: %s", raw)
		}
		if !available && window != nil {
			t.Fatalf("invented usage: %s", raw)
		}
	}
	rows, err := s.Store.DB.Query(`SELECT w.kind,d.immutable_body FROM webhook_deliveries d JOIN webhook_destinations w USING(destination_id) JOIN webhook_events e USING(event_id) WHERE e.event_type='account.enrolled'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind string
		var body []byte
		if err := rows.Scan(&kind, &body); err != nil {
			t.Fatal(err)
		}
		count++
		if !strings.Contains(string(body), "owner@example.com") || !json.Valid(body) {
			t.Fatalf("%s lost email: %s", kind, body)
		}
		if kind != "generic" {
			expected := "Unavailable"
			if available {
				expected = "0% used"
			}
			if !strings.Contains(string(body), expected) || !strings.Contains(string(body), "Labels:") || !strings.Contains(string(body), "CB-201") {
				t.Fatalf("%s lost details: %s", kind, body)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("deliveries=%d", count)
	}
}

func TestPublicEnrollmentNotificationWithoutUsageOrLabels(t *testing.T) {
	s, marker := loginFixture(t, "usage-unavailable")
	configureEnrollmentWebhooks(t, s)
	if err := s.SetPublicEnrollmentOpen(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartPublicEnrollment(t.Context(), "session", "unknown-label")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("approve"), 0600); err != nil {
		t.Fatal(err)
	}
	waitLoginState(t, s, started["login_attempt_id"], "COMPLETED")
	var public string
	if err := s.Store.DB.QueryRow("SELECT public_token FROM accounts JOIN login_attempts USING(account_id) WHERE login_attempt_id=?", started["login_attempt_id"]).Scan(&public); err != nil {
		t.Fatal(err)
	}
	assertEnrollmentEventCount(t, s, 1)
	assertEnrollmentPayload(t, s, public, []string{}, false)
}

func TestFailedPublicEnrollmentNotifies(t *testing.T) {
	s, marker := loginFixture(t, "missing-auth")
	configureEnrollmentWebhooks(t, s)
	if err := s.SetPublicEnrollmentOpen(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartPublicEnrollment(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("approve"), 0600); err != nil {
		t.Fatal(err)
	}
	waitLoginState(t, s, started["login_attempt_id"], "FAILED_RETRYABLE")
	assertEnrollmentEventCount(t, s, 0)
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM webhook_events WHERE event_type='account.enrollment_failed'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure events=%d err=%v", count, err)
	}
}

func TestPublicEnrollmentFailureWebhookDetails(t *testing.T) {
	for _, scenario := range []string{"error:device auth failed with status 429 Too Many Requests", "missing-auth"} {
		t.Run(scenario, func(t *testing.T) {
			s, marker := loginFixture(t, scenario)
			configureEnrollmentWebhooks(t, s)
			if err := s.SetPublicEnrollmentOpen(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			started, err := s.StartPublicEnrollment(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("approve"), 0600); err != nil {
				t.Fatal(err)
			}
			waitLoginState(t, s, started["login_attempt_id"], "FAILED_RETRYABLE")
			var raw []byte
			if err := s.Store.DB.QueryRow("SELECT canonical_body FROM webhook_events WHERE event_type='account.enrollment_failed'").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var event struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			email := ""
			if scenario == "missing-auth" {
				email = "owner@example.com"
			}
			if event.Data["account_email"] != email || event.Data["enrollment_id"] != started["enrollment_id"] || event.Data["login_attempt_id"] != started["login_attempt_id"] || event.Data["error_summary"] == "" || event.Data["account_id"] == "" {
				t.Fatalf("payload=%s", raw)
			}
			if strings.HasPrefix(scenario, "error:") && event.Data["error_summary"] != strings.TrimPrefix(scenario, "error:") {
				t.Fatalf("error lost: %s", raw)
			}
			rows, err := s.Store.DB.Query("SELECT immutable_body FROM webhook_deliveries JOIN webhook_events USING(event_id) WHERE event_type='account.enrollment_failed'")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var body []byte
				if err := rows.Scan(&body); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), event.Data["error_summary"].(string)) || (email != "" && !strings.Contains(string(body), email)) {
					t.Fatalf("delivery lost details: %s", body)
				}
				count++
			}
			err = rows.Err()
			rows.Close()
			if err != nil || count != 3 {
				t.Fatalf("deliveries=%d err=%v", count, err)
			}
			var accountID, operation string
			if err := s.Store.DB.QueryRow("SELECT account_id,operation_id FROM login_attempts WHERE login_attempt_id=?", started["login_attempt_id"]).Scan(&accountID, &operation); err != nil {
				t.Fatal(err)
			}
			if err := s.failLogin(t.Context(), Account{ID: accountID}, started["login_attempt_id"], operation, "FAILED_RETRYABLE", "LOGIN_DENIED", "again"); err != nil {
				t.Fatal(err)
			}
			var events int
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM webhook_events WHERE event_type='account.enrollment_failed'").Scan(&events); err != nil || events != 1 {
				t.Fatalf("duplicate events=%d err=%v", events, err)
			}
		})
	}
}
