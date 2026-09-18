DROP INDEX login_active_account_uq;
ALTER TABLE login_attempts RENAME TO login_attempts_old;
CREATE TABLE login_attempts (
 login_attempt_id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
 operation_id TEXT NOT NULL UNIQUE REFERENCES operations(operation_id) ON DELETE CASCADE,
 method TEXT NOT NULL CHECK(method IN('CHATGPT_BROWSER','CHATGPT_DEVICE_CODE','MANUAL_TOKENS')), state TEXT NOT NULL,
 upstream_login_id TEXT, initiating_session_hash BLOB NOT NULL, interaction_nonce_hash BLOB,
 callback_port INTEGER, callback_mode TEXT, requested_at_ms INTEGER NOT NULL, started_at_ms INTEGER,
 interaction_expires_at_ms INTEGER, oauth_completed_at_ms INTEGER, completed_at_ms INTEGER,
 expected_workspace_id TEXT, observed_email TEXT, observed_plan_type TEXT, error_code TEXT,
 error_summary TEXT, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
) STRICT;
INSERT INTO login_attempts SELECT * FROM login_attempts_old;
DROP TABLE login_attempts_old;
CREATE UNIQUE INDEX login_active_account_uq ON login_attempts(account_id) WHERE state IN('CREATED','STARTING_RUNTIME','STARTING_LOGIN','WAITING_FOR_USER','OAUTH_COMPLETED','VERIFYING_ACCOUNT','STARTING_EXPORT_LOGIN','WAITING_FOR_EXPORT_USER','VERIFYING_EXPORT','FORKING_CREDENTIALS','QUIESCING_RUNTIME','CHECKPOINTING_CREDENTIAL','CANCEL_REQUESTED');
