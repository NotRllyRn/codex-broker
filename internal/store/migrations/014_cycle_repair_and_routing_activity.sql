ALTER TABLE account_state
ADD COLUMN last_routed_at_ms INTEGER;

UPDATE window_pulse_state
SET cycle_state='HELD', weekly_started_at_ms=NULL, next_pulse_at_ms=NULL
WHERE cycle_state='IN_CYCLE'
  AND account_id IN (
    SELECT account_id
    FROM usage_current
    WHERE COALESCE(weekly_used_percent_raw, 0)=0
  );
