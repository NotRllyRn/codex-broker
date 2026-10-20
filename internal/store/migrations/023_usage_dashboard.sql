-- Keep numerical daily history after the seven-day response detail retention.
CREATE TABLE usage_daily (
    day TEXT NOT NULL,
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    usage_known INTEGER NOT NULL CHECK(usage_known IN (0,1)),
    responses INTEGER NOT NULL,
    uncached_input_tokens INTEGER NOT NULL,
    cached_input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    PRIMARY KEY(day,account_id,model,service_tier,usage_known)
) STRICT;

INSERT INTO usage_daily
SELECT strftime('%Y-%m-%d',terminal_event_at_ms/1000,'unixepoch'),account_id,stats_model,
       lower(COALESCE(NULLIF(response_service_tier,''),NULLIF(request_service_tier,''),'default')),
       input_tokens IS NOT NULL AND output_tokens IS NOT NULL,count(*),
       sum(max(0,COALESCE(input_tokens,0)-COALESCE(cached_input_tokens,0))),
       sum(min(COALESCE(input_tokens,0),COALESCE(cached_input_tokens,0))),
       sum(COALESCE(output_tokens,0))
FROM proxy_responses GROUP BY 1,2,3,4,5;

-- INSERT OR IGNORE of replayed response IDs does not fire this trigger.
CREATE TRIGGER usage_daily_record AFTER INSERT ON proxy_responses BEGIN
    INSERT INTO usage_daily VALUES(
        strftime('%Y-%m-%d',NEW.terminal_event_at_ms/1000,'unixepoch'),NEW.account_id,NEW.stats_model,
        lower(COALESCE(NULLIF(NEW.response_service_tier,''),NULLIF(NEW.request_service_tier,''),'default')),
        NEW.input_tokens IS NOT NULL AND NEW.output_tokens IS NOT NULL,1,
        max(0,COALESCE(NEW.input_tokens,0)-COALESCE(NEW.cached_input_tokens,0)),
        min(COALESCE(NEW.input_tokens,0),COALESCE(NEW.cached_input_tokens,0)),COALESCE(NEW.output_tokens,0)
    ) ON CONFLICT(day,account_id,model,service_tier,usage_known) DO UPDATE SET
        responses=responses+1,
        uncached_input_tokens=uncached_input_tokens+excluded.uncached_input_tokens,
        cached_input_tokens=cached_input_tokens+excluded.cached_input_tokens,
        output_tokens=output_tokens+excluded.output_tokens;
END;

CREATE TABLE usage_rate_cache (
    singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),
    fetched_at_ms INTEGER NOT NULL,
    document TEXT NOT NULL CHECK(json_valid(document))
) STRICT;
