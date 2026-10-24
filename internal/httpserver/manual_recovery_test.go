package httpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flosch/pongo2/v6"
)

func waitRecoveryOperation(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.App.Service.Operation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if op["state"] == "SUCCEEDED" || op["state"] == "FAILED" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return nil
}

func TestManualRecoveryWithStoredCredentials(t *testing.T) {
	for _, action := range []string{"refresh", "pulse"} {
		for _, scenario := range []string{"valid", "invalid auth", "wrong identity"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				s, _ := proxyFixture(t, 1)
				ctx := context.Background()
				if _, err := s.App.Store.DB.Exec(`UPDATE accounts SET enabled=0; UPDATE account_state SET auth_state='AUTH_REQUIRED',overall_state='DISABLED',last_error_code='CODEX_AUTH_REQUIRED'`); err != nil {
					t.Fatal(err)
				}
				if err := s.App.Service.OpenIncident(ctx, "account0", "authentication_failed", "ERROR", "Sign-in required"); err != nil {
					t.Fatal(err)
				}
				if scenario == "wrong identity" {
					if _, err := s.App.Store.DB.Exec(`UPDATE account_state SET upstream_email='different@example.com'`); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "invalid auth" {
					home := t.TempDir()
					writeProxyAuth(t, home, "account0")
					raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
					if err != nil {
						t.Fatal(err)
					}
					var auth map[string]any
					if err = json.Unmarshal(raw, &auth); err != nil {
						t.Fatal(err)
					}
					auth["test_failure"] = "authentication required"
					raw, err = json.Marshal(auth)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(home, "auth.json"), raw, 0600); err != nil {
						t.Fatal(err)
					}
					payload, err := s.App.Service.Vault.Capture(home, "test", nil)
					if err != nil {
						t.Fatal(err)
					}
					envelope, err := s.App.Service.Vault.Encrypt("account0", payload)
					if err != nil {
						t.Fatal(err)
					}
					_, err = s.App.Store.DB.Exec(`UPDATE credential_bundles SET bundle_id=?,nonce=?,ciphertext=?,aad=? WHERE account_id='account0'`, envelope.BundleID, envelope.Nonce, envelope.Ciphertext, envelope.AAD)
					if err != nil {
						t.Fatal(err)
					}
				}
				// Background work must not retry AUTH_REQUIRED accounts.
				if _, err := s.App.Service.Refresh(ctx, "account0", "SCHEDULED"); err == nil {
					t.Fatal("scheduled refresh accepted AUTH_REQUIRED")
				}
				var id string
				var err error
				if action == "pulse" {
					id, err = s.App.Service.Pulse(ctx, "account0")
				} else {
					id, err = s.App.Service.Refresh(ctx, "account0", "USER")
				}
				if err != nil {
					t.Fatal(err)
				}
				op := waitRecoveryOperation(t, s, id)
				var authState, overall, incidentState string
				var enabled int
				if err = s.App.Store.DB.QueryRow(`SELECT s.auth_state,s.overall_state,a.enabled FROM account_state s JOIN accounts a USING(account_id) WHERE account_id='account0'`).Scan(&authState, &overall, &enabled); err != nil {
					t.Fatal(err)
				}
				if err = s.App.Store.DB.QueryRow(`SELECT state FROM incidents WHERE scope_key='account0' AND problem_type='authentication_failed'`).Scan(&incidentState); err != nil {
					t.Fatal(err)
				}
				if enabled != 0 {
					t.Fatal("manual recovery enabled traffic")
				}
				if scenario == "valid" {
					if op["state"] != "SUCCEEDED" || authState != "VERIFIED" || overall != "DISABLED" || incidentState != "RESOLVED" {
						t.Fatalf("recovery: op=%v auth=%s overall=%s incident=%s", op, authState, overall, incidentState)
					}
					if action == "pulse" {
						var result map[string]any
						if err = json.Unmarshal([]byte(op["result_json"].(string)), &result); err != nil {
							t.Fatal(err)
						}
						for k, want := range map[string]string{"model": "test-mini", "reasoning_effort": "low", "service_tier": "default", "response": "OK", "prompt": "Reply OK.", "thread_id": "thread", "turn_id": "turn", "status": "completed", "codex_version": "test"} {
							if result[k] != want {
								t.Errorf("pulse %s=%v want %s", k, result[k], want)
							}
						}
					}
				} else if op["state"] != "FAILED" || authState != "AUTH_REQUIRED" || incidentState != "OPEN" {
					t.Fatalf("failed attempt cleared auth: %v auth=%s incident=%s", op, authState, incidentState)
				}
			})
		}
	}
}

func TestPulseResultPageEscapesModelOutput(t *testing.T) {
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	op := map[string]any{"operation_id": "pulse", "kind": "window.pulse", "state": "SUCCEEDED", "result": map[string]any{"model": "test-mini", "reasoning_effort": "low", "service_tier": "default", "codex_version": "0.164.0", "prompt": "Reply OK.", "response": "<script>alert(1)</script>", "status": "completed", "thread_id": "thread", "turn_id": "turn", "duration_ms": 42}}
	if err = renderer.render(response, 200, "operation.html", pongo2.Context{"operation": op}); err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, want := range []string{"test-mini", "Low", "default", "0.164.0", "Reply OK.", "Model response", "&lt;script&gt;", "42 ms", `data-state="SUCCEEDED"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("model output rendered as HTML")
	}
}
