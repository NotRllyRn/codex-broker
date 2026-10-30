package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
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
