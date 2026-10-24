-- Daily cumulative snapshots replace agent-specific event ingestion. Keep legacy
-- tables for recovery, but never combine their totals with the new reports.
CREATE TABLE usage_reports (
 device_id TEXT NOT NULL REFERENCES usage_devices(device_id) ON DELETE CASCADE,
 day TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>0),
 ccusage_version TEXT NOT NULL,
 payload TEXT NOT NULL CHECK(json_valid(payload)),
 updated_at_ms INTEGER NOT NULL,
 PRIMARY KEY(device_id,day)
) STRICT;
CREATE INDEX usage_reports_day ON usage_reports(day);
