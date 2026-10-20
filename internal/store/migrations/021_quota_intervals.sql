CREATE TABLE quota_intervals (
    snapshot_id TEXT NOT NULL,
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    window_kind TEXT NOT NULL CHECK(window_kind IN ('short','weekly')),
    started_at_ms INTEGER NOT NULL,
    ended_at_ms INTEGER NOT NULL CHECK(ended_at_ms>started_at_ms),
    used_percent INTEGER NOT NULL CHECK(used_percent BETWEEN 0 AND 100),
    active_ms INTEGER NOT NULL DEFAULT 0 CHECK(active_ms>=0),
    active_spans_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(active_spans_json)),
    PRIMARY KEY(snapshot_id,window_kind)
) STRICT;
CREATE INDEX quota_intervals_account_time_idx ON quota_intervals(account_id,started_at_ms,ended_at_ms);
CREATE INDEX quota_intervals_end_idx ON quota_intervals(ended_at_ms);

CREATE TABLE quota_lifetime (
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    window_kind TEXT NOT NULL CHECK(window_kind IN ('short','weekly')),
    samples INTEGER NOT NULL CHECK(samples>=0),
    used_percent INTEGER NOT NULL CHECK(used_percent>=0),
    active_ms INTEGER NOT NULL CHECK(active_ms>=0),
    PRIMARY KEY(account_id,window_kind)
) STRICT;
