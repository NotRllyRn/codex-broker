CREATE TABLE account_profiles (
    account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
    profile_display_name TEXT,
    username TEXT,
    stats_as_of TEXT,
    stats_error TEXT,
    lifetime_tokens INTEGER,
    peak_daily_tokens INTEGER,
    longest_running_turn_sec INTEGER,
    current_streak_days INTEGER,
    longest_streak_days INTEGER,
    daily_usage_buckets_json TEXT,
    fast_mode_usage_percentage REAL,
    most_used_reasoning_effort TEXT,
    most_used_reasoning_effort_percentage REAL,
    unique_skills_used INTEGER,
    total_skills_used INTEGER,
    total_threads INTEGER,
    top_invocations_json TEXT,
    complete_read_at_ms INTEGER,
    last_attempt_at_ms INTEGER,
    stale INTEGER NOT NULL DEFAULT 1 CHECK(stale IN(0,1)),
    last_error_code TEXT,
    last_error_summary TEXT,
    state_version INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO account_profiles(account_id)
SELECT account_id FROM accounts;
