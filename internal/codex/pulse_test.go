package codex

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
)

func TestPulseDetailsAndFailureClassification(t *testing.T) {
	for _, scenario := range []struct {
		name, status, message, code string
		itemsOnly                   bool
	}{
		{name: "completed", status: "completed"},
		{name: "final turn items", status: "completed", itemsOnly: true},
		{name: "auth rejected", status: "failed", message: "refresh_token_reused", code: "CODEX_AUTH_REQUIRED"},
		{name: "workspace credits", status: "failed", message: "Your workspace is out of credits", code: "WINDOW_PULSE_FAILED"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			env := []string{"PATH=" + os.Getenv("PATH"), "CODEX_FAKE_APP_SERVER=1", "CODEX_FAKE_PULSE_STATUS=" + scenario.status, "CODEX_FAKE_PULSE_ERROR=" + scenario.message}
			if scenario.itemsOnly {
				env = append(env, "CODEX_FAKE_PULSE_ITEMS_ONLY=1")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Spawn(ctx, os.Args[0], t.TempDir(), env)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			result, err := (Adapter{Client: client}).PulseWindows(ctx)
			if scenario.code != "" {
				var typed *core.Error
				if !errors.As(err, &typed) || typed.Code != scenario.code {
					t.Fatalf("error=%v want %s", err, scenario.code)
				}
				if scenario.code == "WINDOW_PULSE_FAILED" && typed.Detail != scenario.message {
					t.Fatalf("lost failure reason: %s", typed.Detail)
				}
			} else if err != nil || result.Usage == nil {
				t.Fatalf("pulse=%+v error=%v", result, err)
			}
			if result.Model != "test-mini" || result.ReasoningEffort != "minimal" || result.Response != "OK" || result.Status != scenario.status || result.Prompt != "Reply OK." || result.ServiceTier != "default" || result.ThreadID != "thread" || result.TurnID != "turn" {
				t.Fatalf("pulse details=%+v", result)
			}
		})
	}
}
