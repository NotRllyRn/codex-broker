package broker

import (
	"context"
	"database/sql"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
)

func TestBrowserAuthorizationContractAndCallback(t *testing.T) {
	authURL := "https://auth.openai.com/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=expected"
	contract, err := BrowserAuthorizationContract(authURL, []int{1455, 1457}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := ValidateCallback("http://localhost:1455/auth/callback?state=expected&code=grant", *contract, 16384)
	if err != nil || callback != "http://localhost:1455/auth/callback?code=grant&state=expected" {
		t.Fatalf("callback = %q, %v", callback, err)
	}
	if _, err := ValidateCallback("http://localhost:1455/auth/callback?state=wrong&code=grant", *contract, 16384); err == nil {
		t.Fatal("mismatched OAuth state was accepted")
	}
}

func TestInvalidCallbackDoesNotConsumeInteraction(t *testing.T) {
	contract := BrowserContract{Scheme: "http", Host: "localhost", Port: 1455, Path: "/auth/callback", StateHash: auth.Digest("expected")}
	service := &Service{Config: config.Config{BrowserCallbackMaxBytes: 16384}, interactions: map[string]Interaction{
		"attempt": {AttemptID: "attempt", SessionHash: auth.Digest("session"), NonceHash: auth.Digest("nonce"), Login: codex.LoginInteraction{}, Contract: &contract},
	}}
	if err := service.ForwardCallback(context.Background(), "attempt", "session", "nonce", "http://localhost:1455/auth/callback?state=wrong&code=grant"); err == nil {
		t.Fatal("invalid callback was accepted")
	}
	if service.interactions["attempt"].Consumed {
		t.Fatal("invalid callback consumed the interaction")
	}
}

func TestUsageNormalizationAndPoolReset(t *testing.T) {
	usage := NormalizeUsage(map[string]any{"rateLimitsByLimitId": map[string]any{"codex": map[string]any{"windows": []any{
		map[string]any{"name": "short", "usedPercent": float64(25), "windowDurationMins": float64(300), "resetsAt": float64(2100)},
		map[string]any{"name": "weekly", "usedPercent": float64(75), "windowDurationMins": float64(10080), "resetsAt": float64(3000)},
	}}}})
	if usage.Short == nil || usage.Weekly == nil || *usage.Short.UsedPercent != 25 || *usage.Weekly.UsedPercent != 75 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
	now := int64(2_000_000)
	accounts := []Account{
		{ShortUsed: sql.NullInt64{Int64: 100, Valid: true}, ShortReset: sql.NullInt64{Int64: 2100, Valid: true}},
		{WeeklyUsed: sql.NullInt64{Int64: 100, Valid: true}, WeeklyReset: sql.NullInt64{Int64: 2200, Valid: true}},
	}
	if reset := nextRetry(accounts, now); reset != 2_100_000 {
		t.Fatalf("next retry = %d", reset)
	}
}

func TestRoutingRankUsesResetsThenPreference(t *testing.T) {
	now := int64(2_000_000)
	a := Account{PublicID: "a", ID: "a", CreatedAtMS: 1, WeeklyReset: sql.NullInt64{Int64: 3000, Valid: true}}
	b := Account{PublicID: "b", ID: "b", CreatedAtMS: 2, WeeklyReset: sql.NullInt64{Int64: 2500, Valid: true}}
	if !rankLess(b, a, "a", now) {
		t.Fatal("earlier weekly reset did not win")
	}
	b.WeeklyReset = a.WeeklyReset
	if !rankLess(a, b, "a", now) {
		t.Fatal("preferred account did not break the tie")
	}
}
