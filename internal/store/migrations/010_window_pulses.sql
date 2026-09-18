CREATE TABLE window_pulse_state (
    account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
    last_attempt_at_ms INTEGER NOT NULL,
    last_success_at_ms INTEGER,
    next_pulse_at_ms INTEGER,
    last_error_code TEXT
) STRICT;
