ALTER TABLE proxy_responses ADD COLUMN stats_model TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE proxy_responses ADD COLUMN active_duration_ms INTEGER NOT NULL DEFAULT 0 CHECK(active_duration_ms >= 0);
ALTER TABLE proxy_responses ADD COLUMN quota_isolated INTEGER NOT NULL DEFAULT 0 CHECK(quota_isolated IN (0,1));
ALTER TABLE proxy_responses ADD COLUMN quota_invalid_reason TEXT;
ALTER TABLE proxy_responses ADD COLUMN short_usage_delta INTEGER CHECK(short_usage_delta IS NULL OR short_usage_delta > 0);
ALTER TABLE proxy_responses ADD COLUMN weekly_usage_delta INTEGER CHECK(weekly_usage_delta IS NULL OR weekly_usage_delta > 0);

UPDATE proxy_responses SET
    stats_model=COALESCE(NULLIF(response_model,''),NULLIF(request_model,''),'unknown'),
    active_duration_ms=max(0,terminal_event_at_ms-upstream_started_at_ms);

CREATE TABLE proxy_response_attempts (
    proxy_request_id TEXT NOT NULL REFERENCES proxy_responses(proxy_request_id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal > 0),
    account_id TEXT NOT NULL REFERENCES accounts(account_id),
    model TEXT NOT NULL,
    started_at_ms INTEGER NOT NULL,
    ended_at_ms INTEGER NOT NULL CHECK(ended_at_ms >= started_at_ms),
    upstream_http_status INTEGER NOT NULL,
    failure_kind TEXT,
    usage_start_snapshot_id TEXT,
    usage_start_at_ms INTEGER,
    short_start_used_percent INTEGER,
    short_start_resets_at_ms INTEGER,
    weekly_start_used_percent INTEGER,
    weekly_start_resets_at_ms INTEGER,
    usage_end_snapshot_id TEXT,
    usage_end_at_ms INTEGER,
    short_end_used_percent INTEGER,
    short_end_resets_at_ms INTEGER,
    weekly_end_used_percent INTEGER,
    weekly_end_resets_at_ms INTEGER,
    quota_eligible INTEGER NOT NULL CHECK(quota_eligible IN (0,1)),
    quota_isolated INTEGER NOT NULL CHECK(quota_isolated IN (0,1)),
    invalid_reason TEXT,
    PRIMARY KEY(proxy_request_id,ordinal)
) STRICT;

INSERT INTO proxy_response_attempts(
    proxy_request_id,ordinal,account_id,model,started_at_ms,ended_at_ms,upstream_http_status,
    quota_eligible,quota_isolated,invalid_reason
)
SELECT proxy_request_id,upstream_attempts,account_id,stats_model,upstream_started_at_ms,
       terminal_event_at_ms,upstream_http_status,0,0,'migrated_without_usage_baseline'
FROM proxy_responses;

CREATE INDEX proxy_response_attempts_account_time_idx
ON proxy_response_attempts(account_id,started_at_ms,ended_at_ms);

CREATE INDEX proxy_response_attempts_pending_usage_idx
ON proxy_response_attempts(account_id,ended_at_ms)
WHERE quota_eligible=1 AND usage_end_snapshot_id IS NULL;

CREATE TABLE proxy_usage_totals (
    period TEXT NOT NULL CHECK(period IN ('ROLLING_7D','LIFETIME')),
    model TEXT NOT NULL,
    response_count INTEGER NOT NULL DEFAULT 0 CHECK(response_count >= 0),
    active_duration_ms INTEGER NOT NULL DEFAULT 0 CHECK(active_duration_ms >= 0),
    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(input_tokens >= 0),
    cached_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(cached_input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(output_tokens >= 0),
    reasoning_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(reasoning_output_tokens >= 0),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK(total_tokens >= 0),
    short_usage_samples INTEGER NOT NULL DEFAULT 0 CHECK(short_usage_samples >= 0),
    short_usage_active_ms INTEGER NOT NULL DEFAULT 0 CHECK(short_usage_active_ms >= 0),
    short_usage_percent INTEGER NOT NULL DEFAULT 0 CHECK(short_usage_percent >= 0),
    weekly_usage_samples INTEGER NOT NULL DEFAULT 0 CHECK(weekly_usage_samples >= 0),
    weekly_usage_active_ms INTEGER NOT NULL DEFAULT 0 CHECK(weekly_usage_active_ms >= 0),
    weekly_usage_percent INTEGER NOT NULL DEFAULT 0 CHECK(weekly_usage_percent >= 0),
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY(period,model)
) STRICT;

INSERT INTO proxy_usage_totals(
    period,model,response_count,active_duration_ms,input_tokens,cached_input_tokens,
    output_tokens,reasoning_output_tokens,total_tokens,updated_at_ms
)
SELECT 'LIFETIME',stats_model,count(*),sum(active_duration_ms),
       sum(COALESCE(input_tokens,0)),sum(COALESCE(cached_input_tokens,0)),
       sum(COALESCE(output_tokens,0)),sum(COALESCE(reasoning_output_tokens,0)),
       sum(COALESCE(total_tokens,0)),CAST(strftime('%s','now') AS INTEGER)*1000
FROM proxy_responses GROUP BY stats_model;

INSERT INTO proxy_usage_totals(
    period,model,response_count,active_duration_ms,input_tokens,cached_input_tokens,
    output_tokens,reasoning_output_tokens,total_tokens,updated_at_ms
)
SELECT 'ROLLING_7D',stats_model,count(*),sum(active_duration_ms),
       sum(COALESCE(input_tokens,0)),sum(COALESCE(cached_input_tokens,0)),
       sum(COALESCE(output_tokens,0)),sum(COALESCE(reasoning_output_tokens,0)),
       sum(COALESCE(total_tokens,0)),CAST(strftime('%s','now') AS INTEGER)*1000
FROM proxy_responses
WHERE terminal_event_at_ms >= (CAST(strftime('%s','now') AS INTEGER)-604800)*1000
GROUP BY stats_model;

DELETE FROM proxy_responses
WHERE terminal_event_at_ms < (CAST(strftime('%s','now') AS INTEGER)-604800)*1000;

DELETE FROM usage_snapshots
WHERE attempted_at_ms < (CAST(strftime('%s','now') AS INTEGER)-604800)*1000;
