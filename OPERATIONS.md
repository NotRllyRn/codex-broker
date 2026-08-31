# Windowkeeper operations

## Install and initialize

Windowkeeper requires Python 3.12+, a dedicated service account, SQLite storage on a local filesystem, and one exact Codex app-server release. Keep the persistent data directory and runtime directory separate; put the runtime directory on tmpfs.

```bash
uv sync --locked
cp .env.example .env && chmod 600 .env
windowkeeper vault generate-key  # paste into WINDOWKEEPER_VAULT_KEY in .env
windowkeeper doctor
windowkeeper serve
```

Set a 15+ character `WINDOWKEEPER_ADMIN_PASSWORD` in `.env`. Windowkeeper loads the file automatically, creates the vault sentinel on first start, and never stores the vault key in SQLite. `.env` is ignored by Git and Docker builds, but it is still plaintext: keep mode `0600`, do not share it, and restrict access to the repository directory. A service is not ready until `doctor` confirms that the managed Codex executable starts successfully.

## Account sign-in

Device code is recommended for local, NAS, Docker, and SSH deployments. Manual access/refresh-token import is retired because it depends on Codex's private credential schema. Browser OAuth supports three modes:

- `manual`: display the validated authorization URL and paste the resulting localhost callback URL into the authenticated UI.
- `host-loopback`: receive the validated localhost callback directly; only use on a Linux host where Windowkeeper owns the pinned callback ports.
- `disabled`: prohibit browser OAuth while retaining device-code sign-in.

Enrollment requires one ChatGPT approval. Windowkeeper first makes the OAuth credential durable, then uses two isolated Codex refreshes to issue the managed credential and, when none exists, one downloadable export snapshot. The managed result is persisted before export issuance is attempted, so export failure cannot roll it back. Reauthentication advances only the managed credential and leaves an existing export unchanged.

Normal usage, activation, and reconciliation never proactively refresh or use the export. Codex owns normal managed-token refresh timing; Windowkeeper quiesces every authenticated runtime and checkpoints its opaque `auth.json` before deletion, even when the requested RPC fails. A checkpoint failure quarantines the runtime and opens an incident. The export is externally owned, is not kept current, is not remotely revoked, and has no guarantee of independent renewability after the managed lineage rotates. Never copy one export into multiple independently refreshing services.

Windowkeeper activates immediately when an authenticated account reports 0% usage and has no proven activation, then permits only one in-flight activation for that account. It cancels pending activation plans while either the short or weekly window is exhausted and resumes planning automatically after a later usage poll proves capacity is available. A stable upstream reset schedules the next activation just after reset. If an idle upstream reset instead advances by five hours on every poll, Windowkeeper schedules from the last proven activation plus the observed window duration. A definite upstream rejection never becomes a review block; only an outcome that cannot be proven still requires acknowledgment. Reviewing a genuinely ambiguous activation cancels obsolete pending plans before replanning.

Before activation, Windowkeeper reads every page of the authenticated Codex model catalog and intersects visible text models with its program-managed [official Codex credit rate](https://developers.openai.com/codex/pricing) manifest. It selects a model only when that model is no more expensive than every candidate for input, cached input, and output, then explicitly requests its lowest advertised reasoning effort and the standard `default` service tier. Unknown, unavailable, incomparable, or silently substituted choices fail before the activation prompt is dispatched. Update the managed rate manifest whenever the pinned Codex release or official rate card changes.

## Back up and restore

Stop Windowkeeper before all offline CLI operations.

```bash
windowkeeper backup --output /secure/backups/windowkeeper.sqlite
windowkeeper restore --input /secure/backups/windowkeeper.sqlite --confirm RESTORE
windowkeeper vault verify --key-file /secure/windowkeeper-vault.key
windowkeeper doctor
```

Backups contain encrypted credentials and webhook secrets but still contain account metadata; protect them as mode-`0600` files. Back up the vault key separately. Restore validates SQLite integrity and schema, replaces only the database, and never restores a key. Start the service only after key verification succeeds.

## Rotate the vault key

```bash
windowkeeper vault rotate \
  --old-key-file /secure/windowkeeper-vault.key \
  --new-key-file /secure/windowkeeper-vault.next
windowkeeper vault verify --key-file /secure/windowkeeper-vault.next
```

Rotation is offline and transactional. It re-encrypts managed credentials, downloadable auth bundles, webhook URLs, webhook signing secrets, and the sentinel. Replace the configured key-file reference only after verification. Keep the old key in protected recovery storage until the new service has passed readiness and account refresh checks.

## Reverse proxy

Bind Windowkeeper to loopback or a private interface. Set `WINDOWKEEPER_TRUSTED_PROXIES` to an explicit comma-separated IP/CIDR allowlist when using a reverse proxy; wildcards are rejected. Use `WINDOWKEEPER_COOKIE_SECURE=true` for HTTPS. The proxy must preserve the configured root path, enforce request/body limits, and must not log query strings or request bodies. Do not expose Windowkeeper directly to the public internet.

## Webhook notifications

Every notification starts with `WINDOWKEEPER` and a stable type code:
`WK-101` opened, `WK-102` repeated/updated, `WK-103` resolved, and `WK-900`
test. Incident notifications identify the account, state, severity, occurrence
count, detected condition, underlying error, why processing was blocked, and
the recommended recovery action. The event ID uniquely distinguishes repeated
notifications that share a type code. Slack and Discord receive readable
provider-native messages; generic destinations receive the structured
canonical event.

Webhook destinations receive account display names and authenticated email
addresses. Treat destinations as trusted operational systems and remove
destinations that no longer require this metadata.

## Incident response

1. Stop automatic activity by disabling the affected account.
2. Read the incident, operation history, and sanitized logs; retain IDs, timestamps, and error codes only.
3. For `authentication_failed`, reauthenticate and verify the same upstream account/workspace.
4. For `credential_checkpoint`, stop activity and preserve the quarantined runtime tree; it may contain newer credentials than SQLite. Restart only after preserving it, then use explicit reauthentication to establish a new managed lineage.
5. For `activation_ambiguous` or `activation_safety`, inspect upstream thread/turn evidence. Windowkeeper never retries that window. Use **Acknowledge without retry** only after review; this permits future windows, not the ambiguous one.
6. For Codex startup failures, rebuild the image and inspect `windowkeeper doctor` before restarting.
7. For suspected key or host compromise, stop the service, preserve encrypted evidence, rotate credentials outside Windowkeeper, rotate the vault key, and revoke administrator sessions by resetting the password.

Never attach credential files, callback URLs, device codes, vault keys, passwords, SQLite databases, runtime trees, or unsanitized logs to an issue.

## Upgrade and rollback

1. Stop the service and create a database backup.
2. Keep the old image/package, database backup, and vault key.
3. Build the target image and validate its managed Codex release.
4. Install the new Windowkeeper release and run `windowkeeper doctor`.
5. Start it and verify readiness, account refresh, incident state, and webhook delivery.
6. Roll back by stopping the service, restoring the matching backup and prior image, verifying the vault key, and starting again.

Never downgrade a database to a release that does not support its schema.

## Security boundary

Windowkeeper protects credentials at rest, isolates runtime credential trees, redacts known secret-bearing values, and refuses to become ready when Codex is unavailable. Secrets supplied through `.env` are visible to the Windowkeeper process and may be visible to users allowed to inspect its environment or Docker container; use file-backed secrets instead when that distinction matters. Windowkeeper cannot protect against root, a compromised host, or a malicious Codex binary. The managed Codex child process necessarily receives plaintext credentials while its isolated runtime is active.
