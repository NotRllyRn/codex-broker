CREATE TABLE relay_events_next (
 event_id TEXT PRIMARY KEY,
 relay_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN('activation','failure','telemetry','extension_log')),
 occurred_at_ms INTEGER NOT NULL,
 received_at_ms INTEGER NOT NULL
) STRICT;
INSERT INTO relay_events_next SELECT * FROM relay_events;
DROP TABLE relay_events;
ALTER TABLE relay_events_next RENAME TO relay_events;
