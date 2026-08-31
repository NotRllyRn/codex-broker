# Security policy

Do not report secrets in public issues. Send suspected credential exposure,
OAuth callback bypasses, vault failures, activation duplication, or
managed-Codex boundary bypasses privately to the repository maintainers.

Before reporting, retain only sanitized operation IDs, timestamps, and error
codes. Never attach `auth.json`, callback URLs, device codes, vault keys,
administrator passwords, SQLite files, raw runtime directories, or unsanitized
logs.

Windowkeeper does not enforce browser request origins; operators must restrict
network access themselves. CSRF tokens remain enforced. Downloading the export
`auth.json` requires an authenticated session, CSRF validation, administrator
password reauthentication, and returns a non-cacheable attachment. New manual
token import is retired. Normal operations never use or replace the export, and
Windowkeeper cannot revoke downloaded files or guarantee that an export remains
independently renewable after the managed credential rotates.

Webhook notifications are redacted before durable storage and never contain
credentials. They do contain operational account metadata, including display
name and authenticated email, to identify affected accounts. Configure only
trusted HTTPS destinations.

Treat downloaded credential files like passwords, move them directly into
protected credential storage, and do not give the same rotating export to
multiple independent writers. Windowkeeper's security boundary assumes a
trusted host and the Codex executable managed by its release image. A failed
checkpoint deliberately leaves quarantined plaintext evidence for recovery. A
compromised host, root user, or malicious child binary is outside that
boundary.
