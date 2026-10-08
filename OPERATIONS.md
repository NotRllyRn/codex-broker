# Codex Broker operations

## Bootstrap and start

For secure LAN deployment:

```bash
scripts/bootstrap.sh 192.168.1.20
docker compose up --build -d
```

The bootstrap script creates `.env`, a private local CA, and a server certificate containing the supplied local IP. Install `deployment/certs/ca.crt` on every client. Keep `.env`, CA private key, server key, vault key, client keys, and administrator password out of source control.

Loopback HTTP is permitted for development. A non-loopback bind without a TLS certificate and key fails at startup.

The optional Internet-facing enrollment site is a separate process and Compose service on port `8788`; never publish private broker port `8787` to the Internet. See [`docs/public-enrollment.md`](docs/public-enrollment.md).

## Health

- `/health/live`: process liveness.
- `/health/ready`: browser/operator readiness.
- `/api/v1/health`: client-key-authenticated machine readiness.

```bash
codex-broker health --json
codex-broker status --json
codex-broker doctor
```

## Accounts and credentials

Enroll with device code when possible. Browser OAuth supports manual forwarding of the exact loopback callback URL. Manual token import is retired. The public flow always uses device code, verifies the resulting identity, labels the account with its normalized email, and rejects duplicate managed/pending emails. Enable or disable it from **Settings → Public enrollment**; disabling makes visitor routes return empty 404 responses.

Each account has one mutable encrypted `ACTIVE` credential. Every authenticated broker runtime is quiesced and checkpointed before plaintext cleanup, including after failed RPCs or cancellation. A checkpoint failure quarantines the runtime and blocks further credential use until explicit reauthentication.

An optional `EXPORT` is an immutable manual snapshot. Normal usage, routing, and reauthentication never replace it. Do not distribute one export to multiple independent refresh-token writers.

A fixed minimal ephemeral turn starts each account's weekly cycle at its rolling `7 days / eligible accounts` release slot (24 hours for seven accounts, 21 hours for eight). Pulses run one account at a time; short-window resets do not trigger extra pulses. When a weekly window expires, the account is held until its next slot. Routing waits when only held accounts remain, preserving the stagger. Existing clustered windows spread out as they expire; externally using a held account can delay its slot until that upstream weekly window expires. Usage reads and credential maintenance continue while held. Pulse attempts appear as `window.pulse` operations and use the normal credential checkpoint path.

The regular usage poll also reads the authenticated Codex profile summary from ChatGPT. The latest successful response is cached per account for the dashboard; a failed profile read marks that cache stale but retains its last good values and does not invalidate otherwise-successful rate-limit evidence.

## Client keys

Create a key in Settings or offline:

```bash
codex-broker client-key create "Pi desktop"
```

Copy the secret once; only its hash and prefix are stored. Revoke unused or exposed keys immediately. Revocation blocks new leases and proxy requests. Existing streams and access tokens already issued upstream remain usable.

## Routing and exhaustion

A client calls `POST /api/v1/route` once per user turn. The broker skips the reported failed account and ranks in-cycle accounts by earliest weekly reset, then earliest short reset, preferred-account affinity, and stable creation order. Held accounts are excluded until their scheduled cycle pulse succeeds. Disabled, deleted, unauthenticated, credential-less, or known-exhausted accounts are ineligible.

When all eligible accounts are exhausted, the response includes the earliest authoritative reset plus configured padding and an integer `Retry-After`. Unknown reset evidence fails explicitly rather than fabricating a wait.

## Responses proxy

Use the broker's existing HTTPS listener, a named `cbk_` key as the API key, and
`https://broker:8787/v1` as the base URL. Include any configured root path.
`POST /v1/responses` and `POST /v1/responses/compact` call the router in process;
no extra configuration, port, credential type, or migration is required.

Bodies remain opaque and must match ChatGPT Codex's supported protocol. The
proxy forwards non-streaming and streaming responses without injecting notices.
Before delivering output, `401/403` reports an auth failure to the router for
refresh or failover; a refreshed account is tried once more. A second auth failure
is returned without further replay. `429` reports quota failure and selects
another account. Other upstream errors pass through. Transport failures and
redirects return `502`; a broken response stream is aborted without replay.

Known pool exhaustion returns immediate `429`, `Retry-After`, and an OpenAI-style
`pool_exhausted` error. Unknown retry evidence returns `503` rather than inventing
a reset time. Respect `Retry-After` in clients. For `401`, check the client key;
for upstream authentication errors, check account sign-in status. For `502`, check
outbound connectivity and TLS trust. Disable response buffering in any reverse
proxy to preserve streaming, and allow long-running Responses requests.

The dashboard polls admin-session-protected `GET /api/internal/v1/proxy` every
five seconds. Active counts open requests, recent clients are distinct keys used
within five minutes (or still active), and failovers count account changes rather
than same-account refreshes. Counters reset on restart. Names and key prefixes
identify clients; prompts, responses, models, and token usage are not recorded.
Keep the macOS adapter for its ChatGPT-specific behavior until direct routing and
native Voice routes have been verified on a Mac.

## Incidents

1. Open the account and operation detail.
2. For authentication failure, use **Sign in again**.
3. For credential checkpoint failure, preserve the quarantined runtime and recover or reauthenticate before restarting account work.
4. For broker/client TLS failures, verify URL, local CA trust, certificate IP SAN, clock, and client-key status.
5. Export sanitized logs only; never copy runtime trees or SQLite into tickets.

## Backup and restore

Stop or use the offline CLI as documented by each command:

```bash
codex-broker backup --output /secure/backups/windowkeeper.sqlite
codex-broker restore --input /secure/backups/windowkeeper.sqlite --confirm RESTORE
codex-broker vault verify --key-file /secure/current.key
codex-broker vault rotate --old-key-file /secure/old.key --new-key-file /secure/new.key
```

Back up the database and vault key separately. A database without its matching key cannot decrypt credentials. Vault rotation is all-or-nothing.

## Upgrade and rollback

1. Back up the database, vault key, and Compose configuration.
2. Stop the old process.
3. Start the new image against a copy first and run readiness, account listing, lease, and managed checkpoint checks.
4. Preserve the physical `windowkeeper-data` volume, `windowkeeper.db`, lock, vault KDF/AAD strings, sentinel, and historical schema identifiers.
5. Migration 009 removes legacy activation tables and state; migration 010 adds minimal window-pulse state; migration 011 adds public enrollment claims; migration 012 adds the per-account profile-stat cache; migration 013 adds weekly release-cycle metadata. These migrations do not change existing credentials. Rollback to software expecting the old activation tables requires restoring the pre-v9 backup; rollback across migrations 012 or 013 requires restoring the corresponding automatic pre-migration backup.
6. Do not require account relogin for a normal upgrade.
