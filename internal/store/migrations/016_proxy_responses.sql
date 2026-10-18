CREATE TABLE proxy_responses (
    proxy_request_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(account_id),
    client_key_id TEXT REFERENCES client_api_keys(key_id) ON DELETE SET NULL,
    thread_id TEXT,
    turn_id TEXT,
    endpoint TEXT NOT NULL,
    request_model TEXT,
    reasoning_effort TEXT,
    request_service_tier TEXT,
    previous_response_id TEXT,
    response_id TEXT,
    response_model TEXT,
    response_service_tier TEXT,
    terminal_status TEXT NOT NULL CHECK(terminal_status IN('completed','incomplete','failed')),
    upstream_attempts INTEGER NOT NULL CHECK(upstream_attempts > 0),
    upstream_http_status INTEGER NOT NULL,
    request_started_at_ms INTEGER NOT NULL,
    upstream_started_at_ms INTEGER NOT NULL,
    response_headers_at_ms INTEGER NOT NULL,
    first_response_event_at_ms INTEGER NOT NULL,
    terminal_event_at_ms INTEGER NOT NULL,
    input_tokens INTEGER CHECK(input_tokens IS NULL OR input_tokens >= 0),
    cached_input_tokens INTEGER CHECK(cached_input_tokens IS NULL OR cached_input_tokens >= 0),
    output_tokens INTEGER CHECK(output_tokens IS NULL OR output_tokens >= 0),
    reasoning_output_tokens INTEGER CHECK(reasoning_output_tokens IS NULL OR reasoning_output_tokens >= 0),
    total_tokens INTEGER CHECK(total_tokens IS NULL OR total_tokens >= 0),
    usage_json TEXT CHECK(usage_json IS NULL OR json_valid(usage_json))
) STRICT;

CREATE INDEX proxy_responses_account_time_idx
ON proxy_responses(account_id, terminal_event_at_ms DESC);

CREATE INDEX proxy_responses_thread_time_idx
ON proxy_responses(thread_id, request_started_at_ms)
WHERE thread_id IS NOT NULL;
