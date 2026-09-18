package broker

import "database/sql"

type Account struct {
	ID, PublicID, DisplayName, Workspace, PreferredMethod, AuthState, WorkerState string
	Enabled, Deleted                                                              bool
	CreatedAtMS                                                                   int64
	Email, Plan, LastMethod                                                       sql.NullString
	LastVerified, ShortUsed, ShortReset, WeeklyUsed, WeeklyReset, ExcludedUntil   sql.NullInt64
}

type AccountSummary struct {
	AccountID, PublicToken, DisplayName, OverallState, AuthState, UsageState, Evidence string
	Labels                                                                             []string
	Enabled                                                                            bool
	ShortPercent, ShortResetMS, WeeklyPercent, WeeklyResetMS, LastRefreshMS            *int64
	ActiveOperation                                                                    *string
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
