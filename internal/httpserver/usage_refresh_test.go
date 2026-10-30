package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduledPulsePreflightPreservesExternalWindowAndCadence(t *testing.T) {
	for _, age := range []time.Duration{3 * 24 * time.Hour, 12 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			s, _ := proxyFixture(t, 7)
			service := s.App.Service
			service.Config.WindowPulseEnabled = true
			service.Config.WindowPulseRetrySeconds = 900
			now := time.Now().UnixMilli()
			week := 7 * 24 * time.Hour
			for i := 0; i < 7; i++ {
				id := fmt.Sprintf("account%d", i)
				state := "IN_CYCLE"
				used := 1
				started := now - 2*(24*time.Hour).Milliseconds()
				attempt := int64(0)
				if i < 2 {
					state = "HELD"
					used = 0
					started = now
				}
				if i == 1 {
					attempt = now + time.Hour.Milliseconds()
				}
				if _, err := s.App.Store.DB.Exec(`UPDATE usage_current SET short_used_percent_raw=?,weekly_used_percent_raw=?,weekly_duration_minutes=10080,weekly_resets_at_s=?,complete_read_at_ms=? WHERE account_id=?`, used, used, (started+week.Milliseconds())/1000, now, id); err != nil {
					t.Fatal(err)
				}
				if _, err := s.App.Store.DB.Exec(`INSERT INTO window_pulse_state(account_id,cycle_state,last_attempt_at_ms,weekly_started_at_ms,last_usage_observed_at_ms) VALUES(?,?,?,?,?)`, id, state, attempt, started, now); err != nil {
					t.Fatal(err)
				}
			}
			writeFixture := func(id string, reset int64, used int) {
				t.Helper()
				root := filepath.Join(service.Config.RuntimeDir, "accounts", id)
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(map[string]any{"reset_s": reset, "used": used})
				if err := os.WriteFile(filepath.Join(root, "usage-fixture.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			externalStart := now - age.Milliseconds()
			writeFixture("account0", (externalStart+week.Milliseconds())/1000, 0) // Fixed zero-percent windows still carry a real start.
			writeFixture("account1", 0, 0)
			if err := service.QueuePulses(context.Background()); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var finished int
				if err := s.App.Store.DB.QueryRow(`SELECT count(*) FROM operations WHERE kind='window.pulse' AND state='SUCCEEDED'`).Scan(&finished); err != nil {
					t.Fatal(err)
				}
				if finished == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("preflight did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
			var state string
			var actualStart int64
			if err := s.App.Store.DB.QueryRow(`SELECT cycle_state,weekly_started_at_ms FROM window_pulse_state WHERE account_id='account0'`).Scan(&state, &actualStart); err != nil {
				t.Fatal(err)
			}
			if state != "IN_CYCLE" || actualStart/1000 != externalStart/1000 {
				t.Fatalf("external start lost: %s %d want %d", state, actualStart, externalStart)
			}
			if _, err := os.Stat(filepath.Join(service.Config.RuntimeDir, "accounts/account0/usage-fixture.json.pulsed")); !os.IsNotExist(err) {
				t.Fatal("externally started account was pulsed")
			}
			// Make a second candidate available and re-evaluate the one-day cadence.
			if _, err := s.App.Store.DB.Exec(`UPDATE window_pulse_state SET last_attempt_at_ms=0 WHERE account_id='account1'`); err != nil {
				t.Fatal(err)
			}
			if err := service.QueuePulses(context.Background()); err != nil {
				t.Fatal(err)
			}
			if age < 24*time.Hour {
				var count int
				if err := s.App.Store.DB.QueryRow(`SELECT count(*) FROM operations WHERE account_id='account1'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("recent start should defer cadence: %d %v", count, err)
				}
				return
			}
			for {
				var finished int
				if err := s.App.Store.DB.QueryRow(`SELECT count(*) FROM operations WHERE account_id='account1' AND state='SUCCEEDED'`).Scan(&finished); err != nil {
					t.Fatal(err)
				}
				if finished == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("old external window consumed current pulse slot")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := os.Stat(filepath.Join(service.Config.RuntimeDir, "accounts/account1/usage-fixture.json.pulsed")); err != nil {
				t.Fatalf("next idle candidate was not pulsed: %v", err)
			}
		})
	}
}

func TestPulsePreflightRefusesInvalidUsageAndUnknownReset(t *testing.T) {
	for _, kind := range []string{"negative percentage", "expired reset"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := proxyFixture(t, 1)
			service := s.App.Service
			root := filepath.Join(service.Config.RuntimeDir, "accounts/account0")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			fixture := map[string]any{"used": 0, "reset_s": time.Now().Add(-time.Hour).Unix()}
			if kind == "negative percentage" {
				fixture["used"] = -1
				fixture["reset_s"] = time.Now().Add(7 * 24 * time.Hour).Unix()
			}
			data, _ := json.Marshal(fixture)
			path := filepath.Join(root, "usage-fixture.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			operation, err := service.Pulse(context.Background(), "account0")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var state string
				if err := s.App.Store.DB.QueryRow(`SELECT state FROM operations WHERE operation_id=?`, operation).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state == "FAILED" {
					break
				}
				if state == "SUCCEEDED" || time.Now().After(deadline) {
					t.Fatalf("unsafe pulse result: %s", state)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := os.Stat(path + ".pulsed"); !os.IsNotExist(err) {
				t.Fatal("pulse ran without reliable usage/reset evidence")
			}
		})
	}
}

func TestQuotaRejectionDuringPreflightPreventsPulse(t *testing.T) {
	s, key := proxyFixture(t, 1)
	service := s.App.Service
	ctx := context.Background()
	root := filepath.Join(service.Config.RuntimeDir, "accounts/account0")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "usage-fixture.json")
	if err := os.WriteFile(path, []byte(`{"used":0,"wait":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path+".release", []byte("1"), 0600)
	operation, err := service.Pulse(ctx, "account0")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path + ".reading"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preflight did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, err := s.App.Router.Route(ctx, key.ID, broker.RouteRequest{FailedAccountID: "account0", FailureKind: "quota"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".release", []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		var state string
		if err := s.App.Store.DB.QueryRow(`SELECT state FROM operations WHERE operation_id=?`, operation).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "SUCCEEDED" {
			break
		}
		if state == "FAILED" || time.Now().After(deadline) {
			t.Fatalf("preflight did not skip pulse cleanly: %s", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(path + ".pulsed"); !os.IsNotExist(err) {
		t.Fatal("pulse ran despite concurrent rejection")
	}
	var blocks int
	if err := s.App.Store.DB.QueryRow(`SELECT count(*) FROM account_quota_blocks`).Scan(&blocks); err != nil || blocks != 1 {
		t.Fatalf("preflight erased rejection: %d %v", blocks, err)
	}
}
