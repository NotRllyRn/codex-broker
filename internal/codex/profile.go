package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const ProfileURL = "https://chatgpt.com/backend-api/wham/profiles/me"

type AccountProfile struct {
	Profile  *ProfileIdentity `json:"profile"`
	Metadata *ProfileMetadata `json:"metadata"`
	Stats    ProfileStats     `json:"stats"`
}

type ProfileIdentity struct {
	DisplayName *string `json:"display_name"`
	Username    *string `json:"username"`
}

type ProfileMetadata struct {
	StatsAsOf  *string `json:"stats_as_of"`
	StatsError *string `json:"stats_error"`
}

type ProfileStats struct {
	LifetimeTokens                    *int64       `json:"lifetime_tokens"`
	PeakDailyTokens                   *int64       `json:"peak_daily_tokens"`
	LongestRunningTurnSec             *int64       `json:"longest_running_turn_sec"`
	CurrentStreakDays                 *int64       `json:"current_streak_days"`
	LongestStreakDays                 *int64       `json:"longest_streak_days"`
	DailyUsageBuckets                 []DailyUsage `json:"daily_usage_buckets"`
	FastModeUsagePercentage           *float64     `json:"fast_mode_usage_percentage"`
	MostUsedReasoningEffort           *string      `json:"most_used_reasoning_effort"`
	MostUsedReasoningEffortPercentage *float64     `json:"most_used_reasoning_effort_percentage"`
	UniqueSkillsUsed                  *int64       `json:"unique_skills_used"`
	TotalSkillsUsed                   *int64       `json:"total_skills_used"`
	TotalThreads                      *int64       `json:"total_threads"`
	TopInvocations                    []Invocation `json:"top_invocations"`
}

type DailyUsage struct {
	StartDate string `json:"start_date"`
	Tokens    int64  `json:"tokens"`
}

type Invocation struct {
	Type       string  `json:"type"`
	PluginName *string `json:"plugin_name"`
	SkillName  *string `json:"skill_name"`
	UsageCount *int64  `json:"usage_count"`
}

func FetchProfile(ctx context.Context, url, accessToken, accountID string) (AccountProfile, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AccountProfile{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("ChatGPT-Account-Id", accountID)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex-broker")
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return AccountProfile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return AccountProfile{}, fmt.Errorf("profile endpoint returned HTTP %d", response.StatusCode)
	}
	var profile AccountProfile
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&profile); err != nil {
		return AccountProfile{}, fmt.Errorf("invalid profile response: %w", err)
	}
	return profile, nil
}
