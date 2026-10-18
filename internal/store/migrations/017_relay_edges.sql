CREATE TABLE relay_reservations (
 relay_id TEXT PRIMARY KEY,
 account_id TEXT NOT NULL UNIQUE REFERENCES accounts(account_id) ON DELETE CASCADE,
 permit_epoch TEXT NOT NULL UNIQUE,
 expires_at_ms INTEGER NOT NULL,
 updated_at_ms INTEGER NOT NULL
) STRICT;

CREATE INDEX relay_reservations_expiry_idx ON relay_reservations(expires_at_ms);

CREATE TABLE relay_events (
 event_id TEXT PRIMARY KEY,
 relay_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN('activation','failure','telemetry')),
 occurred_at_ms INTEGER NOT NULL,
 received_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE relay_heartbeats (
 relay_id TEXT PRIMARY KEY,
 protocol_version INTEGER NOT NULL,
 snapshot_id TEXT,
 cache_generated_at_ms INTEGER,
 ready INTEGER NOT NULL CHECK(ready IN(0,1)),
 last_seen_at_ms INTEGER NOT NULL
) STRICT;
