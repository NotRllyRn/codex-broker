CREATE TABLE client_api_keys (
 key_id TEXT PRIMARY KEY, name TEXT NOT NULL, key_prefix TEXT NOT NULL,
 secret_hash BLOB NOT NULL UNIQUE, created_at_ms INTEGER NOT NULL,
 last_used_at_ms INTEGER, revoked_at_ms INTEGER
) STRICT;
CREATE INDEX client_api_keys_active_idx ON client_api_keys(revoked_at_ms,created_at_ms);
