package broker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

func int64p(value int64) *int64       { return &value }
func float64p(value float64) *float64 { return &value }

func TestAggregateProfilesCombinesAccountStatistics(t *testing.T) {
	profiles := []storedProfile{
		{publicToken: "a", label: "Alpha", lifetime: int64p(100), longestTurn: int64p(60), currentStreak: int64p(2), longestStreak: int64p(4), fastMode: float64p(20), reasoning: "medium", reasoningPercent: float64p(60), uniqueSkills: int64p(2), totalSkills: int64p(3), totalThreads: int64p(10), daily: []codex.DailyUsage{{StartDate: "2026-09-18", Tokens: 40}}},
		{publicToken: "b", label: "Beta", lifetime: int64p(300), longestTurn: int64p(120), currentStreak: int64p(3), longestStreak: int64p(5), fastMode: float64p(40), reasoning: "high", reasoningPercent: float64p(70), uniqueSkills: int64p(4), totalSkills: int64p(5), totalThreads: int64p(30), daily: []codex.DailyUsage{{StartDate: "2026-09-18", Tokens: 80}, {StartDate: "2026-09-19", Tokens: 50}}},
	}
	result := aggregateProfiles(profiles)
	if result.LifetimeTokens == nil || *result.LifetimeTokens != 400 || result.PeakDailyTokens == nil || *result.PeakDailyTokens != 120 {
		t.Fatalf("token aggregate = %#v", result)
	}
	if result.FastModeUsagePercentage == nil || *result.FastModeUsagePercentage != 35 || result.MostUsedReasoningEffort != "high" {
		t.Fatalf("insight aggregate = %#v", result)
	}
	if result.UniqueSkillsUsed == nil || *result.UniqueSkillsUsed != 6 || result.TotalThreads == nil || *result.TotalThreads != 40 {
		t.Fatalf("count aggregate = %#v", result)
	}
}

func TestProfileCachePersistsLastGoodValuesOnFailure(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "windowkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','public','Alpha',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES('a','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO account_profiles(account_id) VALUES('a');`)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database}
	account := Account{ID: "a", PublicID: "public"}
	profile := codex.AccountProfile{Stats: codex.ProfileStats{LifetimeTokens: int64p(99), DailyUsageBuckets: []codex.DailyUsage{{StartDate: "2026-09-19", Tokens: 9}}}}
	if err := service.commitProfile(ctx, account, profile); err != nil {
		t.Fatal(err)
	}
	if err := service.recordProfileFailure(ctx, account, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	var tokens int64
	var stale int
	if err := database.DB.QueryRowContext(ctx, "SELECT lifetime_tokens,stale FROM account_profiles WHERE account_id='a'").Scan(&tokens, &stale); err != nil {
		t.Fatal(err)
	}
	if tokens != 99 || stale != 1 {
		t.Fatalf("tokens=%d stale=%d", tokens, stale)
	}
}
