package broker

import "database/sql"

import "github.com/NotRllyRn/codex-broker/internal/codex"

type Account struct {
	ID, PublicID, DisplayName, Workspace, PreferredMethod, AuthState, WorkerState string
	CycleState                                                                    string
	Enabled, Deleted                                                              bool
	CreatedAtMS                                                                   int64
	Email, Plan, LastMethod                                                       sql.NullString
	LastVerified, ShortUsed, ShortReset, WeeklyUsed, WeeklyReset, ExcludedUntil   sql.NullInt64
	CycleStarted                                                                  sql.NullInt64
}

type AccountSummary struct {
	AccountID, PublicToken, DisplayName, OverallState, AuthState, UsageState, Evidence string
	Labels                                                                             []string
	Enabled                                                                            bool
	ShortPercent, ShortResetMS, WeeklyPercent, WeeklyResetMS, LastRefreshMS            *int64
	ActiveOperation                                                                    *string
	CycleState                                                                         string
	CyclePosition, CycleSize                                                           int
	CycleReleaseMS                                                                     *int64
}

type Window struct {
	Slot            string `json:"slot"`
	UsedPercent     *int64 `json:"used_percent"`
	DurationMinutes *int64 `json:"duration_minutes"`
	ResetsAtS       *int64 `json:"resets_at_s"`
}

type Usage struct {
	SelectedLimitID string   `json:"selected_limit_id,omitempty"`
	Short           *Window  `json:"short,omitempty"`
	Weekly          *Window  `json:"weekly,omitempty"`
	Others          []Window `json:"others"`
}

type RouteRequest struct {
	SessionID, TurnID, PreferredAccountID, FailedAccountID, FailureKind string
}

type RouteLease struct {
	AccountID, AccountLabel, AccessToken, ChatGPTAccountID string
	ExpiresAtMS                                            int64
	ShortRemainingPercent, WeeklyRemainingPercent          *int64
	ShortResetsAtMS, WeeklyResetsAtMS                      *int64
}

type PoolWait struct {
	NextRetryAtMS     int64
	RetryAfterSeconds int
}

type Operation struct {
	ID, Kind, Trigger, State                                           string
	AccountID                                                          sql.NullString
	ProgressCode, ProgressSummary, ResultJSON, ErrorCode, ErrorSummary sql.NullString
	CreatedAtMS                                                        int64
	StartedAtMS, CompletedAtMS                                         sql.NullInt64
}

type ProfileDashboard struct {
	ID                                string             `json:"id"`
	Label                             string             `json:"label"`
	Username                          string             `json:"username,omitempty"`
	StatsAsOf                         string             `json:"stats_as_of,omitempty"`
	LifetimeTokens                    *int64             `json:"lifetime_tokens"`
	PeakDailyTokens                   *int64             `json:"peak_daily_tokens"`
	LongestRunningTurnSec             *int64             `json:"longest_running_turn_sec"`
	CurrentStreakDays                 *int64             `json:"current_streak_days"`
	LongestStreakDays                 *int64             `json:"longest_streak_days"`
	FastModeUsagePercentage           *float64           `json:"fast_mode_usage_percentage"`
	MostUsedReasoningEffortPercentage *float64           `json:"most_used_reasoning_effort_percentage"`
	MostUsedReasoningEffort           string             `json:"most_used_reasoning_effort,omitempty"`
	UniqueSkillsUsed                  *int64             `json:"unique_skills_used"`
	TotalSkillsUsed                   *int64             `json:"total_skills_used"`
	TotalThreads                      *int64             `json:"total_threads"`
	DailyUsageBuckets                 []codex.DailyUsage `json:"daily_usage_buckets"`
	TokenShares                       []TokenShare       `json:"token_shares"`
	UpdatedAtMS                       *int64             `json:"updated_at_ms"`
	Stale                             bool               `json:"stale"`
}

type TokenShare struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
	Tokens    int64  `json:"tokens"`
}
