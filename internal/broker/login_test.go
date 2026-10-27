package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

var loginMarker = flag.String("login-marker", "", "fake login approval file")
var loginScenario = flag.String("login-scenario", "success", "fake login behavior")

// A login succeeds without any refresh. A forced refresh fails, so introducing
// credential forking again breaks the end-to-end login tests.
func TestLoginFakeAppServer(t *testing.T) {
	if *loginMarker == "" {
		return
	}
	decoder, encoder := json.NewDecoder(bufio.NewReader(os.Stdin)), json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	send := func(v any) { mu.Lock(); defer mu.Unlock(); _ = encoder.Encode(v) }
	for {
		var request map[string]any
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		id, ok := request["id"]
		if !ok {
			continue
		}
		result := map[string]any{}
		switch request["method"] {
		case "account/login/start":
			result = map[string]any{"loginId": "login", "verificationUrl": "https://auth.openai.com/codex/device", "userCode": "TEST-CODE"}
			params, _ := request["params"].(map[string]any)
			if params["type"] == "chatgpt" {
				result["authUrl"] = "https://auth.openai.com/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=expected"
			}
			go func() {
				for {
					if _, err := os.Stat(*loginMarker); err == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if *loginScenario != "missing-auth" {
					_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), loginAuth(), 0600)
				}
				send(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "login", "success": true}})
			}()
		case "account/read":
			params, _ := request["params"].(map[string]any)
			if params["refreshToken"] == true {
				send(map[string]any{"id": id, "error": map[string]any{"message": "forced refresh is forbidden"}})
				continue
			}
			email := "owner@example.com"
			if *loginScenario == "wrong-identity" {
				email = "other@example.com"
			}
			result["account"] = map[string]any{"type": "chatgpt", "email": email, "planType": "plus"}
		case "account/rateLimits/read":
			if *loginScenario == "usage-unavailable" {
				send(map[string]any{"id": id, "error": map[string]any{"message": "usage unavailable"}})
				continue
			}
			result["rateLimits"] = map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 0, "windowDurationMins": 300, "resetsAt": time.Now().Add(5 * time.Hour).Unix()}, "secondary": map[string]any{"usedPercent": 0, "windowDurationMins": 10080, "resetsAt": time.Now().Add(7 * 24 * time.Hour).Unix()}}
		}
		send(map[string]any{"id": id, "result": result})
	}
}

func loginAuth() []byte {
	content, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": testJWT(4102444800), "refresh_token": "original-refresh", "account_id": "upstream"}})
	return content
}

func loginFixture(t *testing.T, scenario string) (*Service, string) {
	t.Helper()
	root, ctx := t.TempDir(), context.Background()
	database, err := store.Open(ctx, filepath.Join(root, "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	instance, _ := database.InstanceID(ctx)
	secrets, _ := vault.New(make([]byte, 32), instance)
	marker, script := filepath.Join(root, "approve"), filepath.Join(root, "codex")
	command := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=^TestLoginFakeAppServer$ -login-marker=%s -login-scenario=%s -- \"$@\"\n", strconv.Quote(os.Args[0]), strconv.Quote(marker), strconv.Quote(scenario))
	if err := os.WriteFile(script, []byte(command), 0700); err != nil {
		t.Fatal(err)
	}
	settings := config.Config{RuntimeDir: filepath.Join(root, "run"), CodexExecutable: script, CodexVersion: "test", ProcessStartConcurrency: 2, UsageRefreshConcurrency: 2, WindowPulseConcurrency: 1, AuthConcurrency: 1, LoginTimeoutSeconds: 10, BrowserOAuthMode: "manual", BrowserOAuthCallbackPorts: []int{1455}, BrowserCallbackMaxBytes: 16384}
	service := NewService(database, settings, secrets, codex.NewRuntimeManager(settings, secrets), events.New(100, 100))
	t.Cleanup(service.Close)
	return service, marker
}

func waitLoginState(t *testing.T, s *Service, attempt string, expected string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := s.Store.DB.QueryRow("SELECT state FROM login_attempts WHERE login_attempt_id=?", attempt).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == expected {
			return
		}
		if time.Now().After(deadline) || strings.HasPrefix(state, "FAILED") && state != expected {
			t.Fatalf("login state=%s, want %s", state, expected)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLoginPreservesOriginalCredential(t *testing.T) {
	for _, method := range []string{"CHATGPT_DEVICE_CODE", "CHATGPT_BROWSER", "PUBLIC"} {
		t.Run(method, func(t *testing.T) {
			ctx := context.Background()
			s, marker := loginFixture(t, "success")
			configureEnrollmentWebhooks(t, s)
			var started map[string]string
			var err error
			var public, id string
			if method == "PUBLIC" {
				if err = s.SetPublicEnrollmentOpen(ctx, true); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"Label", "Another-label"} {
					if err = s.CreateLabel(ctx, name); err != nil {
						t.Fatal(err)
					}
				}
				started, err = s.StartPublicEnrollment(ctx, "session", "label", "LABEL", "Another-label", "unknown-label")
			} else {
				var created map[string]any
				created, err = s.CreateAccount(ctx, "Owner", nil, "workspace", method)
				if err != nil {
					t.Fatal(err)
				}
				public = created["public_token"].(string)
				started, err = s.StartLogin(ctx, public, method, "session", false, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Store.DB.QueryRow("SELECT a.account_id,a.public_token FROM accounts a JOIN login_attempts l USING(account_id) WHERE l.login_attempt_id=?", started["login_attempt_id"]).Scan(&id, &public); err != nil {
				t.Fatal(err)
			}
			if method == "PUBLIC" {
				labels, err := s.labels(ctx, id)
				if err != nil || strings.Join(labels, ",") != "Another-label,Label" {
					t.Fatalf("enrollment labels=%v %v", labels, err)
				}
				var otherLabelID string
				if err := s.Store.DB.QueryRow("SELECT label_id FROM labels WHERE name='Another-label'").Scan(&otherLabelID); err != nil {
					t.Fatal(err)
				}
				if err := s.DeleteLabel(ctx, otherLabelID); err != nil {
					t.Fatal(err)
				}
			}
			// Keep profile polling out of this test; no upstream networking is needed.
			if _, err = s.Store.DB.Exec("INSERT INTO account_profiles(account_id,last_attempt_at_ms) VALUES(?,?) ON CONFLICT(account_id) DO UPDATE SET last_attempt_at_ms=excluded.last_attempt_at_ms", id, time.Now().UnixMilli()); err != nil {
				t.Fatal(err)
			}
			assertEnrollmentEventCount(t, s, 0)
			if err = os.WriteFile(marker, []byte("approve"), 0600); err != nil {
				t.Fatal(err)
			}
			waitLoginState(t, s, started["login_attempt_id"], "COMPLETED")
			if method == "PUBLIC" {
				assertEnrollmentEventCount(t, s, 1)
				assertEnrollmentPayload(t, s, public, []string{"Label"}, true)
			} else {
				assertEnrollmentEventCount(t, s, 0)
			}
			// Wait for post-login usage and its runtime checkpoint to finish as well.
			deadline := time.Now().Add(10 * time.Second)
			for {
				var fresh int
				_ = s.Store.DB.QueryRow("SELECT count(*) FROM operations WHERE account_id=? AND kind='usage.refresh' AND state='SUCCEEDED'", id).Scan(&fresh)
				if fresh == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("post-login usage did not succeed")
				}
				time.Sleep(5 * time.Millisecond)
			}
			payload, err := s.bundle(ctx, id, "ACTIVE")
			if err != nil {
				t.Fatal(err)
			}
			content, err := s.Vault.AuthJSON(payload)
			if err != nil || string(content) != string(loginAuth()) {
				t.Fatalf("original credential changed: %v", err)
			}
			var bundles int
			_ = s.Store.DB.QueryRow("SELECT count(*) FROM credential_bundles WHERE account_id=?", id).Scan(&bundles)
			if bundles != 1 {
				t.Fatalf("created %d credential bundles, want one", bundles)
			}
			account, err := s.account(ctx, public)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := s.Lease(ctx, account, time.Now().UnixMilli(), false)
			if err != nil || lease.AccessToken != testJWT(4102444800) {
				t.Fatalf("lease=%#v, error=%v", lease, err)
			}
			if method == "PUBLIC" {
				labels, err := s.labels(ctx, id)
				if err != nil || strings.Join(labels, ",") != "Label" {
					t.Fatalf("completed enrollment labels=%v %v", labels, err)
				}
				var unknown int
				if err := s.Store.DB.QueryRow("SELECT count(*) FROM labels WHERE lower(name)='unknown-label'").Scan(&unknown); err != nil || unknown != 0 {
					t.Fatalf("unknown label created: %d %v", unknown, err)
				}
				status, err := s.PublicEnrollmentStatus(ctx, started["enrollment_id"], started["login_attempt_id"], "session", started["interaction_nonce"])
				if err != nil || status["state"] != "COMPLETED" || status["email"] != "owner@example.com" || account.DisplayName != "owner@example.com" {
					t.Fatalf("public enrollment=%v, error=%v", status, err)
				}
			} else {
				// Reauthentication retires the previous generation without forking it.
				again, err := s.StartLogin(ctx, public, method, "session", false, "")
				if err != nil {
					t.Fatal(err)
				}
				waitLoginState(t, s, again["login_attempt_id"], "COMPLETED")
				var active, retired int
				_ = s.Store.DB.QueryRow("SELECT count(*) FROM credential_bundles WHERE account_id=? AND state='ACTIVE'", id).Scan(&active)
				_ = s.Store.DB.QueryRow("SELECT count(*) FROM credential_bundles WHERE account_id=? AND state='RETIRED'", id).Scan(&retired)
				assertEnrollmentEventCount(t, s, 0)
				if active != 1 || retired != 1 {
					t.Fatalf("replacement generations active=%d retired=%d", active, retired)
				}
			}
		})
	}
}

func TestLoginFailurePreservesExistingCredential(t *testing.T) {
	for _, scenario := range []string{"wrong-identity", "missing-auth", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, marker := loginFixture(t, scenario)
			created, err := s.CreateAccount(ctx, "Owner", nil, "", "CHATGPT_DEVICE_CODE")
			if err != nil {
				t.Fatal(err)
			}
			id, public := created["account_id"].(string), created["public_token"].(string)
			home := t.TempDir()
			if err = os.WriteFile(filepath.Join(home, "auth.json"), loginAuth(), 0600); err != nil {
				t.Fatal(err)
			}
			payload, err := s.Vault.Capture(home, "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.replaceActive(ctx, id, payload); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Store.DB.Exec("UPDATE account_state SET upstream_email='owner@example.com',auth_state='VERIFIED' WHERE account_id=?", id); err != nil {
				t.Fatal(err)
			}
			started, err := s.StartLogin(ctx, public, "CHATGPT_DEVICE_CODE", "session", false, "")
			if err != nil {
				t.Fatal(err)
			}
			waitLoginState(t, s, started["login_attempt_id"], "WAITING_FOR_USER")
			if scenario == "cancel" {
				if _, err = s.CancelLogin(ctx, started["login_attempt_id"], "session"); err != nil {
					t.Fatal(err)
				}
				waitLoginState(t, s, started["login_attempt_id"], "CANCELLED")
			} else {
				if err = os.WriteFile(marker, []byte("approve"), 0600); err != nil {
					t.Fatal(err)
				}
				expected := "FAILED_RETRYABLE"
				if scenario == "wrong-identity" {
					expected = "FAILED_ACTION_REQUIRED"
				}
				waitLoginState(t, s, started["login_attempt_id"], expected)
			}
			stored, err := s.bundle(ctx, id, "ACTIVE")
			if err != nil {
				t.Fatal(err)
			}
			content, err := s.Vault.AuthJSON(stored)
			if err != nil || string(content) != string(loginAuth()) {
				t.Fatalf("existing credential changed: %v", err)
			}
			var count int
			_ = s.Store.DB.QueryRow("SELECT count(*) FROM credential_bundles WHERE account_id=?", id).Scan(&count)
			if count != 1 {
				t.Fatalf("failure created %d bundles", count)
			}
		})
	}
}

func TestPublicEnrollmentDuplicateStatus(t *testing.T) {
	ctx := context.Background()
	s, marker := loginFixture(t, "success")
	if _, err := s.CreateAccount(ctx, "owner@example.com", nil, "", "CHATGPT_DEVICE_CODE"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPublicEnrollmentOpen(ctx, true); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartPublicEnrollment(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("approve"), 0600); err != nil {
		t.Fatal(err)
	}
	waitLoginState(t, s, started["login_attempt_id"], "FAILED_RETRYABLE")
	status, err := s.PublicEnrollmentStatus(ctx, started["enrollment_id"], started["login_attempt_id"], "session", started["interaction_nonce"])
	if err != nil || status["state"] != "FAILED" || status["error_code"] != "PUBLIC_ACCOUNT_EXISTS" {
		t.Fatalf("status=%v error=%v", status, err)
	}
}

func TestPublicEnrollmentHasNoActiveCap(t *testing.T) {
	s, _ := loginFixture(t, "success")
	ctx := context.Background()
	if err := s.SetPublicEnrollmentOpen(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Keep authorization pending while exceeding the former 16-slot cap.
	for i := 0; i < 20; i++ {
		if _, err := s.StartPublicEnrollment(ctx, fmt.Sprintf("session-%d", i)); err != nil {
			t.Fatalf("enrollment %d rejected: %v", i, err)
		}
	}
}
