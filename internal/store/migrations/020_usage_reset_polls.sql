-- Keep at most one pending reset and one processed watermark per account/window.
CREATE TABLE usage_reset_polls (
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    window_kind TEXT NOT NULL CHECK(window_kind IN ('short','weekly')),
    reset_at_s INTEGER NOT NULL CHECK(reset_at_s > 0),
    last_polled_reset_at_s INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(account_id,window_kind)
) STRICT;
