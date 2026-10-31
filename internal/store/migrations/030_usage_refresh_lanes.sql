ALTER TABLE usage_poll_state ADD COLUMN next_sweep_at_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_poll_state ADD COLUMN regular_cursor TEXT NOT NULL DEFAULT '';
