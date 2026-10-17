ALTER TABLE window_pulse_state
ADD COLUMN last_usage_observed_at_ms INTEGER;

ALTER TABLE window_pulse_state
ADD COLUMN idle_observation_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE weekly_cycle_state
RENAME COLUMN last_release_at_ms TO cadence_anchor_at_ms;

UPDATE weekly_cycle_state SET cadence_anchor_at_ms=NULL;
