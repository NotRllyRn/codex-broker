ALTER TABLE window_pulse_state
ADD COLUMN cycle_state TEXT NOT NULL DEFAULT 'IN_CYCLE'
CHECK(cycle_state IN('IN_CYCLE','HELD','RELEASING'));

ALTER TABLE window_pulse_state
ADD COLUMN weekly_started_at_ms INTEGER;

CREATE TABLE weekly_cycle_state (
    singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),
    last_release_at_ms INTEGER,
    member_count INTEGER NOT NULL DEFAULT 0,
    state_version INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO weekly_cycle_state(singleton_id) VALUES(1);
