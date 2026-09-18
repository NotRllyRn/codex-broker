CREATE TABLE public_enrollments (
    enrollment_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL UNIQUE REFERENCES accounts(account_id) ON DELETE CASCADE,
    login_attempt_id TEXT NOT NULL UNIQUE REFERENCES login_attempts(login_attempt_id) ON DELETE CASCADE,
    session_hash BLOB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('ACTIVE','COMPLETED','FAILED')),
    observed_email TEXT,
    created_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER
) STRICT;

CREATE UNIQUE INDEX public_enrollments_active_email
ON public_enrollments(lower(observed_email))
WHERE state = 'ACTIVE' AND observed_email IS NOT NULL;

CREATE INDEX public_enrollments_active_expiry
ON public_enrollments(state, expires_at_ms);
