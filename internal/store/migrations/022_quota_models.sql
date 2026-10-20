-- NULL means mixed/unknown activity or an interval measured before attribution.
-- Empty model means no activity observed yet; a late response may identify it.
ALTER TABLE quota_intervals ADD COLUMN model TEXT;

CREATE TABLE quota_model_lifetime (
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    model TEXT NOT NULL CHECK(model<>'' AND model<>'unknown'),
    window_kind TEXT NOT NULL CHECK(window_kind IN ('short','weekly')),
    samples INTEGER NOT NULL CHECK(samples>=0),
    used_percent INTEGER NOT NULL CHECK(used_percent>=0),
    active_ms INTEGER NOT NULL CHECK(active_ms>=0),
    PRIMARY KEY(account_id,model,window_kind)
) STRICT;
