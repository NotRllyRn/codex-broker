-- Upstream rejections override cached percentages until fresh evidence or a reset.
CREATE TABLE account_quota_blocks (
    account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
    blocked_at_ms INTEGER NOT NULL,
    retry_at_ms INTEGER NOT NULL
) STRICT;

DELETE FROM account_exclusions WHERE failure_kind='quota';

-- Quota-per-hour measurement no longer drives extra upstream reads or storage.
DROP TABLE proxy_response_attempts;
ALTER TABLE proxy_responses DROP COLUMN quota_isolated;
ALTER TABLE proxy_responses DROP COLUMN quota_invalid_reason;
ALTER TABLE proxy_responses DROP COLUMN short_usage_delta;
ALTER TABLE proxy_responses DROP COLUMN weekly_usage_delta;
ALTER TABLE proxy_usage_totals DROP COLUMN short_usage_samples;
ALTER TABLE proxy_usage_totals DROP COLUMN short_usage_active_ms;
ALTER TABLE proxy_usage_totals DROP COLUMN short_usage_percent;
ALTER TABLE proxy_usage_totals DROP COLUMN weekly_usage_samples;
ALTER TABLE proxy_usage_totals DROP COLUMN weekly_usage_active_ms;
ALTER TABLE proxy_usage_totals DROP COLUMN weekly_usage_percent;
