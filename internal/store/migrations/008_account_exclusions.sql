CREATE TABLE account_exclusions (
 key_id TEXT NOT NULL REFERENCES client_api_keys(key_id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
 failure_kind TEXT NOT NULL CHECK(failure_kind IN('auth','quota','rate_limit')),
 expires_at_ms INTEGER NOT NULL, created_at_ms INTEGER NOT NULL,
 PRIMARY KEY(key_id,account_id)
) STRICT;
CREATE INDEX account_exclusions_expiry_idx ON account_exclusions(expires_at_ms);
