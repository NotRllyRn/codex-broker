package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/NotRllyRn/codex-broker/internal/codex"
)

type storedProfile struct {
	accountID, publicToken, label, username, statsAsOf, reasoning string
	lifetime, peak, longestTurn, currentStreak, longestStreak     *int64
	fastMode, reasoningPercent                                    *float64
	uniqueSkills, totalSkills, totalThreads, updatedAt            *int64
	daily                                                         []codex.DailyUsage
	stale                                                         bool
}

func (s *Service) ProfileDashboard(ctx context.Context) ([]ProfileDashboard, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT a.account_id,a.public_token,a.display_name,p.username,p.stats_as_of,p.lifetime_tokens,p.peak_daily_tokens,p.longest_running_turn_sec,p.current_streak_days,p.longest_streak_days,p.daily_usage_buckets_json,p.fast_mode_usage_percentage,p.most_used_reasoning_effort,p.most_used_reasoning_effort_percentage,p.unique_skills_used,p.total_skills_used,p.total_threads,p.complete_read_at_ms,p.stale FROM accounts a LEFT JOIN account_profiles p USING(account_id) WHERE a.deleted_at_ms IS NULL ORDER BY lower(a.display_name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var profiles []storedProfile
	for rows.Next() {
		var item storedProfile
		var username, statsAsOf, daily, reasoning sql.NullString
		var lifetime, peak, longestTurn, currentStreak, longestStreak, uniqueSkills, totalSkills, totalThreads, updated sql.NullInt64
		var fastMode, reasoningPercent sql.NullFloat64
		var stale sql.NullInt64
		if err := rows.Scan(&item.accountID, &item.publicToken, &item.label, &username, &statsAsOf, &lifetime, &peak, &longestTurn, &currentStreak, &longestStreak, &daily, &fastMode, &reasoning, &reasoningPercent, &uniqueSkills, &totalSkills, &totalThreads, &updated, &stale); err != nil {
			return nil, err
		}
		item.username, item.statsAsOf, item.reasoning = username.String, statsAsOf.String, reasoning.String
		item.lifetime, item.peak, item.longestTurn = nullableInt(lifetime), nullableInt(peak), nullableInt(longestTurn)
		item.currentStreak, item.longestStreak = nullableInt(currentStreak), nullableInt(longestStreak)
		item.fastMode, item.reasoningPercent = nullableFloat(fastMode), nullableFloat(reasoningPercent)
		item.uniqueSkills, item.totalSkills, item.totalThreads, item.updatedAt = nullableInt(uniqueSkills), nullableInt(totalSkills), nullableInt(totalThreads), nullableInt(updated)
		item.stale = !stale.Valid || stale.Int64 != 0
		if daily.Valid {
			_ = json.Unmarshal([]byte(daily.String), &item.daily)
		}
		profiles = append(profiles, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]ProfileDashboard, 0, len(profiles)+1)
	result = append(result, aggregateProfiles(profiles))
	for _, profile := range profiles {
		result = append(result, dashboardProfile(profile))
	}
	return result, nil
}

func dashboardProfile(profile storedProfile) ProfileDashboard {
	result := ProfileDashboard{
		ID: profile.publicToken, Label: profile.label, Username: profile.username, StatsAsOf: profile.statsAsOf,
		LifetimeTokens: profile.lifetime, PeakDailyTokens: profile.peak, LongestRunningTurnSec: profile.longestTurn,
		CurrentStreakDays: profile.currentStreak, LongestStreakDays: profile.longestStreak,
		FastModeUsagePercentage: profile.fastMode, MostUsedReasoningEffort: profile.reasoning,
		MostUsedReasoningEffortPercentage: profile.reasoningPercent, UniqueSkillsUsed: profile.uniqueSkills,
		TotalSkillsUsed: profile.totalSkills, TotalThreads: profile.totalThreads, DailyUsageBuckets: profile.daily,
		UpdatedAtMS: profile.updatedAt, Stale: profile.stale,
	}
	if profile.lifetime != nil {
		result.TokenShares = []TokenShare{{AccountID: profile.publicToken, Label: profile.label, Tokens: *profile.lifetime}}
	}
	return result
}

func aggregateProfiles(profiles []storedProfile) ProfileDashboard {
	result := ProfileDashboard{ID: "all", Label: "All accounts", Stale: len(profiles) == 0}
	daily := map[string]int64{}
	var lifetime, uniqueSkills, totalSkills, totalThreads int64
	var hasLifetime, hasUnique, hasSkills, hasThreads bool
	var fastWeighted, fastWeight float64
	reasoningScores := map[string]float64{}
	var reasoningWeight float64
	for _, profile := range profiles {
		result.Stale = result.Stale || profile.stale
		result.LongestRunningTurnSec = maximum(result.LongestRunningTurnSec, profile.longestTurn)
		result.CurrentStreakDays = maximum(result.CurrentStreakDays, profile.currentStreak)
		result.LongestStreakDays = maximum(result.LongestStreakDays, profile.longestStreak)
		result.UpdatedAtMS = maximum(result.UpdatedAtMS, profile.updatedAt)
		if profile.statsAsOf > result.StatsAsOf {
			result.StatsAsOf = profile.statsAsOf
		}
		if profile.lifetime != nil {
			lifetime, hasLifetime = lifetime+*profile.lifetime, true
			result.TokenShares = append(result.TokenShares, TokenShare{AccountID: profile.publicToken, Label: profile.label, Tokens: *profile.lifetime})
		}
		if profile.uniqueSkills != nil {
			uniqueSkills, hasUnique = uniqueSkills+*profile.uniqueSkills, true
		}
		if profile.totalSkills != nil {
			totalSkills, hasSkills = totalSkills+*profile.totalSkills, true
		}
		weight := float64(1)
		if profile.totalThreads != nil {
			totalThreads, hasThreads = totalThreads+*profile.totalThreads, true
			if *profile.totalThreads > 0 {
				weight = float64(*profile.totalThreads)
			}
		}
		if profile.fastMode != nil {
			fastWeighted += *profile.fastMode * weight
			fastWeight += weight
		}
		if profile.reasoning != "" && profile.reasoningPercent != nil {
			reasoningScores[profile.reasoning] += *profile.reasoningPercent * weight
			reasoningWeight += weight
		}
		for _, bucket := range profile.daily {
			daily[bucket.StartDate] += bucket.Tokens
		}
	}
	if hasLifetime {
		result.LifetimeTokens = &lifetime
	}
	if hasUnique {
		result.UniqueSkillsUsed = &uniqueSkills
	}
	if hasSkills {
		result.TotalSkillsUsed = &totalSkills
	}
	if hasThreads {
		result.TotalThreads = &totalThreads
	}
	if fastWeight > 0 {
		value := fastWeighted / fastWeight
		result.FastModeUsagePercentage = &value
	}
	var bestScore float64
	for effort, score := range reasoningScores {
		if score > bestScore {
			result.MostUsedReasoningEffort, bestScore = effort, score
		}
	}
	if reasoningWeight > 0 && result.MostUsedReasoningEffort != "" {
		value := bestScore / reasoningWeight
		result.MostUsedReasoningEffortPercentage = &value
	}
	for date, tokens := range daily {
		result.DailyUsageBuckets = append(result.DailyUsageBuckets, codex.DailyUsage{StartDate: date, Tokens: tokens})
		result.PeakDailyTokens = maximum(result.PeakDailyTokens, &tokens)
	}
	sort.Slice(result.DailyUsageBuckets, func(i, j int) bool {
		return result.DailyUsageBuckets[i].StartDate < result.DailyUsageBuckets[j].StartDate
	})
	sort.Slice(result.TokenShares, func(i, j int) bool { return result.TokenShares[i].Tokens > result.TokenShares[j].Tokens })
	return result
}

func maximum(current, candidate *int64) *int64 {
	if candidate == nil || current != nil && *current >= *candidate {
		return current
	}
	value := *candidate
	return &value
}

func nullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}
