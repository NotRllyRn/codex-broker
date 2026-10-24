CREATE TABLE usage_identities (
    identity_id TEXT PRIMARY KEY,
    name TEXT NOT NULL COLLATE NOCASE UNIQUE,
    code_hash BLOB NOT NULL UNIQUE,
    created_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE usage_devices (
    device_id TEXT PRIMARY KEY,
    identity_id TEXT NOT NULL REFERENCES usage_identities(identity_id) ON DELETE CASCADE,
    installation_id TEXT NOT NULL,
    name TEXT NOT NULL,
    token_hash BLOB NOT NULL UNIQUE,
    created_at_ms INTEGER NOT NULL,
    last_sync_at_ms INTEGER,
    revoked_at_ms INTEGER,
    UNIQUE(identity_id, installation_id)
) STRICT;

CREATE TABLE identity_proxy_keys (
    key_id TEXT PRIMARY KEY REFERENCES client_api_keys(key_id) ON DELETE CASCADE,
    identity_id TEXT NOT NULL REFERENCES usage_identities(identity_id) ON DELETE CASCADE
) STRICT;

-- Numerical events and their deduplication identities outlive proxy cleanup.
CREATE TABLE identity_usage (
    identity_id TEXT NOT NULL REFERENCES usage_identities(identity_id) ON DELETE CASCADE,
    event_id TEXT NOT NULL,
    device_id TEXT REFERENCES usage_devices(device_id),
    source TEXT NOT NULL,
    timestamp_ms INTEGER NOT NULL,
    model TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    input_tokens INTEGER NOT NULL CHECK(input_tokens>=0),
    cached_input_tokens INTEGER NOT NULL CHECK(cached_input_tokens>=0 AND cached_input_tokens<=input_tokens),
    cache_write_tokens INTEGER NOT NULL CHECK(cache_write_tokens>=0 AND cache_write_tokens<=input_tokens-cached_input_tokens),
    output_tokens INTEGER NOT NULL CHECK(output_tokens>=0),
    reasoning_tokens INTEGER NOT NULL CHECK(reasoning_tokens>=0 AND reasoning_tokens<=output_tokens),
    usage_known INTEGER NOT NULL CHECK(usage_known IN (0,1)),
    PRIMARY KEY(identity_id,event_id)
) STRICT;
CREATE INDEX identity_usage_time ON identity_usage(identity_id,timestamp_ms);
CREATE INDEX identity_usage_device_time ON identity_usage(device_id,timestamp_ms);
