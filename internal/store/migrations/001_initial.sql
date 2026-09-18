PRAGMA auto_vacuum = INCREMENTAL;
PRAGMA journal_mode = WAL;

CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ms INTEGER NOT NULL) STRICT;
CREATE TABLE instance_metadata (singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1), instance_uuid TEXT NOT NULL UNIQUE, created_at_ms INTEGER NOT NULL, schema_created_by_version TEXT NOT NULL) STRICT;
CREATE TABLE vault_state (singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1), active_key_id TEXT NOT NULL, sentinel_nonce BLOB NOT NULL, sentinel_ciphertext BLOB NOT NULL, updated_at_ms INTEGER NOT NULL) STRICT;
CREATE TABLE accounts (
 account_id TEXT PRIMARY KEY, public_token TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL,
 auth_mode TEXT NOT NULL DEFAULT 'chatgpt', preferred_login_method TEXT NOT NULL DEFAULT 'CHATGPT_DEVICE_CODE',
 last_successful_login_method TEXT, workspace_constraint TEXT, enabled INTEGER NOT NULL CHECK(enabled IN(0,1)),
 lifecycle_state TEXT NOT NULL, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL, deleted_at_ms INTEGER
) STRICT;
CREATE UNIQUE INDEX accounts_active_name_uq ON accounts(lower(display_name)) WHERE deleted_at_ms IS NULL;
CREATE TABLE labels (label_id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at_ms INTEGER NOT NULL) STRICT;
CREATE UNIQUE INDEX labels_name_uq ON labels(lower(name));
CREATE TABLE account_labels (account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE, label_id TEXT NOT NULL REFERENCES labels(label_id) ON DELETE CASCADE, PRIMARY KEY(account_id,label_id)) STRICT;
CREATE TABLE credential_bundles (
 bundle_id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
 state TEXT NOT NULL, envelope_version INTEGER NOT NULL, payload_schema_version INTEGER NOT NULL,
 key_id TEXT NOT NULL, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, aad BLOB NOT NULL,
 codex_version TEXT NOT NULL, created_at_ms INTEGER NOT NULL, promoted_at_ms INTEGER, retired_at_ms INTEGER
) STRICT;
CREATE UNIQUE INDEX credential_one_active_uq ON credential_bundles(account_id) WHERE state='ACTIVE';
CREATE UNIQUE INDEX credential_nonce_uq ON credential_bundles(account_id,key_id,nonce);
CREATE TABLE account_state (
 account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE, auth_state TEXT NOT NULL,
 worker_state TEXT NOT NULL, overall_state TEXT NOT NULL, usage_state TEXT NOT NULL, activation_state TEXT NOT NULL,
 upstream_email TEXT, upstream_plan TEXT, workspace_verified INTEGER, last_auth_verified_at_ms INTEGER,
 active_operation_id TEXT, last_error_code TEXT, last_error_summary TEXT, state_version INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
) STRICT;
CREATE TABLE usage_current (
 account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE, snapshot_id TEXT,
 selected_limit_id TEXT, short_raw_slot TEXT, short_used_percent_raw INTEGER, short_duration_minutes INTEGER,
 short_resets_at_s INTEGER, short_anomaly INTEGER NOT NULL DEFAULT 0, weekly_raw_slot TEXT,
 weekly_used_percent_raw INTEGER, weekly_duration_minutes INTEGER, weekly_resets_at_s INTEGER,
 weekly_anomaly INTEGER NOT NULL DEFAULT 0, complete_read_at_ms INTEGER, last_attempt_at_ms INTEGER,
 stale INTEGER NOT NULL DEFAULT 1 CHECK(stale IN(0,1)), last_error_code TEXT, last_error_summary TEXT,
 source TEXT NOT NULL DEFAULT 'NONE', state_version INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE TABLE usage_snapshots (
 snapshot_id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
 attempted_at_ms INTEGER NOT NULL, completed_at_ms INTEGER, success INTEGER NOT NULL CHECK(success IN(0,1)),
 selected_limit_id TEXT, normalized_json TEXT, raw_shape_summary_json TEXT, error_code TEXT,
 error_summary TEXT, duration_ms INTEGER NOT NULL
) STRICT;
CREATE INDEX usage_snapshots_account_time_idx ON usage_snapshots(account_id,attempted_at_ms DESC);
CREATE TABLE operations (
 operation_id TEXT PRIMARY KEY, account_id TEXT REFERENCES accounts(account_id) ON DELETE SET NULL,
 kind TEXT NOT NULL, trigger TEXT NOT NULL, state TEXT NOT NULL, progress_code TEXT, progress_summary TEXT,
 result_json TEXT, error_code TEXT, error_summary TEXT, created_at_ms INTEGER NOT NULL, started_at_ms INTEGER,
 completed_at_ms INTEGER, lease_token TEXT, lease_expires_at_ms INTEGER, state_version INTEGER NOT NULL
) STRICT;
CREATE INDEX operations_runnable_idx ON operations(state,created_at_ms) WHERE state IN('QUEUED','RETRY_SCHEDULED');
CREATE TABLE login_attempts (
 login_attempt_id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
 operation_id TEXT NOT NULL UNIQUE REFERENCES operations(operation_id) ON DELETE CASCADE,
 method TEXT NOT NULL CHECK(method IN('CHATGPT_BROWSER','CHATGPT_DEVICE_CODE')), state TEXT NOT NULL,
 upstream_login_id TEXT, initiating_session_hash BLOB NOT NULL, interaction_nonce_hash BLOB,
 callback_port INTEGER, callback_mode TEXT, requested_at_ms INTEGER NOT NULL, started_at_ms INTEGER,
 interaction_expires_at_ms INTEGER, oauth_completed_at_ms INTEGER, completed_at_ms INTEGER,
 expected_workspace_id TEXT, observed_email TEXT, observed_plan_type TEXT, error_code TEXT,
 error_summary TEXT, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX login_active_account_uq ON login_attempts(account_id) WHERE state IN('CREATED','STARTING_RUNTIME','STARTING_LOGIN','WAITING_FOR_USER','OAUTH_COMPLETED','VERIFYING_ACCOUNT','QUIESCING_RUNTIME','CHECKPOINTING_CREDENTIAL','CANCEL_REQUESTED');
CREATE TABLE activation_attempts (
 activation_id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
 window_key TEXT NOT NULL, trigger TEXT NOT NULL, prompt_version INTEGER NOT NULL, prompt_sha256 BLOB NOT NULL,
 schedule_source TEXT NOT NULL, schedule_confidence TEXT NOT NULL, basis_reset_at_s INTEGER,
 basis_duration_minutes INTEGER, scheduled_for_ms INTEGER, state TEXT NOT NULL, upstream_thread_id TEXT,
 upstream_turn_id TEXT, client_user_message_id TEXT NOT NULL, normalized_result TEXT, terminal_status TEXT,
 ambiguity_reason TEXT, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL, completed_at_ms INTEGER,
 state_version INTEGER NOT NULL, UNIQUE(account_id,window_key)
) STRICT;
CREATE TABLE activation_operations (
 activation_operation_id TEXT PRIMARY KEY, activation_id TEXT NOT NULL REFERENCES activation_attempts(activation_id) ON DELETE CASCADE,
 operation_kind TEXT NOT NULL, attempt_number INTEGER NOT NULL, state TEXT NOT NULL, request_id TEXT,
 write_started_at_ms INTEGER, write_completed_at_ms INTEGER, accepted_at_ms INTEGER, completed_at_ms INTEGER,
 upstream_thread_id TEXT, upstream_turn_id TEXT, error_code TEXT, error_summary TEXT, evidence_json TEXT,
 created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL, UNIQUE(activation_id,operation_kind,attempt_number)
) STRICT;
CREATE UNIQUE INDEX activation_one_unfinished_uq ON activation_operations(activation_id) WHERE state IN('STARTED','REQUEST_WRITING','AWAITING_RESPONSE','RECONCILING');
CREATE TABLE incidents (
 incident_id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_key TEXT NOT NULL, problem_type TEXT NOT NULL,
 state TEXT NOT NULL, severity TEXT NOT NULL, summary TEXT NOT NULL, current_error_code TEXT,
 occurrence_count INTEGER NOT NULL, opened_at_ms INTEGER NOT NULL, last_seen_at_ms INTEGER NOT NULL,
 resolved_at_ms INTEGER, resolution_reason TEXT, state_version INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX incident_one_open_uq ON incidents(scope_kind,scope_key,problem_type) WHERE state='OPEN';
CREATE TABLE webhook_destinations (
 destination_id TEXT PRIMARY KEY, display_name TEXT NOT NULL, kind TEXT NOT NULL, enabled INTEGER NOT NULL,
 url_nonce BLOB NOT NULL, encrypted_url BLOB NOT NULL, secret_nonce BLOB, encrypted_signing_secret BLOB,
 created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
) STRICT;
CREATE TABLE webhook_events (event_id TEXT PRIMARY KEY,event_type TEXT NOT NULL,subject TEXT NOT NULL,occurred_at_ms INTEGER NOT NULL,canonical_body BLOB NOT NULL,incident_id TEXT REFERENCES incidents(incident_id) ON DELETE SET NULL,created_at_ms INTEGER NOT NULL) STRICT;
CREATE TABLE webhook_deliveries (
 delivery_id TEXT PRIMARY KEY,event_id TEXT NOT NULL REFERENCES webhook_events(event_id) ON DELETE CASCADE,
 destination_id TEXT NOT NULL REFERENCES webhook_destinations(destination_id) ON DELETE CASCADE,state TEXT NOT NULL,
 attempt_count INTEGER NOT NULL,next_attempt_at_ms INTEGER NOT NULL,immutable_body BLOB NOT NULL,content_type TEXT NOT NULL,
 lease_token TEXT,lease_expires_at_ms INTEGER,last_status_code INTEGER,last_error_code TEXT,last_response_excerpt TEXT,
 created_at_ms INTEGER NOT NULL,completed_at_ms INTEGER,UNIQUE(event_id,destination_id)
) STRICT;
CREATE TABLE admin_credentials (singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),password_hash TEXT NOT NULL,password_changed_at_ms INTEGER NOT NULL,bootstrap_complete INTEGER NOT NULL CHECK(bootstrap_complete IN(0,1))) STRICT;
CREATE TABLE admin_sessions (
 session_id_hash BLOB PRIMARY KEY,csrf_token_hash BLOB NOT NULL,created_at_ms INTEGER NOT NULL,last_seen_at_ms INTEGER NOT NULL,
 idle_expires_at_ms INTEGER NOT NULL,absolute_expires_at_ms INTEGER NOT NULL,reauthenticated_at_ms INTEGER,
 revoked_at_ms INTEGER,client_fingerprint_hash BLOB
) STRICT;
CREATE TABLE settings (setting_key TEXT PRIMARY KEY,value_json TEXT NOT NULL,updated_at_ms INTEGER NOT NULL) STRICT;
CREATE TABLE log_level_overrides (override_id TEXT PRIMARY KEY,level TEXT NOT NULL,scope_kind TEXT NOT NULL,scope_key TEXT,created_at_ms INTEGER NOT NULL,expires_at_ms INTEGER NOT NULL,created_by_session_hash BLOB) STRICT;
