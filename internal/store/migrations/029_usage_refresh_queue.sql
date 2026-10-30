CREATE TABLE usage_poll_state (
    singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),
    next_regular_at_ms INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO usage_poll_state(singleton_id) VALUES(1);
CREATE TABLE usage_refresh_requests (
    account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
    due_at_ms INTEGER NOT NULL,
    reason TEXT NOT NULL
) STRICT;
INSERT INTO usage_refresh_requests(account_id,due_at_ms,reason)
    SELECT account_id,blocked_at_ms+45000,'QUOTA_REJECTED' FROM account_quota_blocks;
