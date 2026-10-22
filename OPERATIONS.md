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

The optional active/passive Relay Edge uses `compose.relay.yaml` on a different
host. Bootstrap it with `scripts/bootstrap-relay.sh`; transfer only the
generated relay bundle, not primary secrets or data. See
[`docs/relay-edge.md`](docs/relay-edge.md).

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

Login checkpoints the original verified OAuth credential directly. Codex owns normal token refresh; enrollment never refreshes a credential to create additional token sets. Individual `auth.json` downloads are unavailable. Migration 024 removes legacy export snapshots without changing active credentials. Existing revoked credentials still require sign-in again.

A fixed minimal ephemeral turn starts each account's weekly cycle at its rolling `7 days / eligible accounts` release slot (24 hours for seven accounts, 21 hours for eight). Pulses run one account at a time; short-window resets do not trigger extra pulses. When a weekly window expires, the account is held until its next slot. Routing waits when only held accounts remain, preserving the stagger. Existing clustered windows spread out as they expire; externally using a held account can delay its slot until that upstream weekly window expires. Usage reads and credential maintenance continue while held. Pulse attempts appear as `window.pulse` operations and use the normal credential checkpoint path.

Usage limits are polled every 3600 seconds plus 0–30 seconds of jitter by default (`WINDOWKEEPER_USAGE_POLL_SECONDS`). Profile statistics from `/backend-api/wham/profiles/me` are attempted at most once every 21600 seconds per account (`WINDOWKEEPER_PROFILE_POLL_SECONDS`), during a usage refresh. Both settings are exposed in Compose and the NixOS module (`usage.pollSeconds` and `usage.profilePollSeconds`). The latest successful profile is cached for the dashboard; failed reads retain the last good values. Repeated manual refreshes respect the profile interval, including after failures.

Each known short or weekly reset also schedules a usage refresh two minutes after its reported reset time, checked locally every 15 seconds. The grace period lets upstream usage settle. Busy accounts wait until their current operation finishes; two due windows share one refresh. A successful regular/manual read started after the grace period satisfies that reset without an additional call. Reads during the grace period do not discard the pending delayed refresh, even if upstream already reports the next window. Processed reset timestamps are persisted (two small schedule rows per account), so stale upstream timestamps, failed attempts, or broker restarts do not cause repeated reset polls. Failed reset attempts fall back to regular hourly polling. Reset refreshes respect the six-hour profile cooldown.

An upstream quota rejection overrides cached usage percentages for every client and survives broker restart. The account is temporarily treated as fully exhausted until either known usage window resets or a successful complete usage read that started after the rejection arrives. Failed or already-in-flight reads do not unlock it. With no known reset, it stays blocked until a successful fresh read. Quota failures and completed proxy responses do not initiate extra usage reads. The dashboard marks blocked accounts as usage exhausted and preserves the last reported percentages in both the overview and account detail.

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
transport forwards responses without injecting notices. Production checks on
2026-10-08 confirmed that ChatGPT Codex rejects `stream:false` with `400` and
returns `404` for standalone `/responses/compact`, including when called
directly upstream. Clients must use `stream:true`; the broker passes these
upstream limitations through unchanged.
Before delivering output, `401/403` reports an auth failure to the router for
refresh or failover; a refreshed account is tried once more. A second auth failure
is returned without further replay. `429` and recognized quota errors in JSON
error bodies mark the account exhausted across clients and select another
account. Streamed quota failures mark the account exhausted for subsequent
requests without replaying delivered output. Other upstream errors pass through. Transport failures and
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
identify clients. Prompts and response content are not recorded; terminal
response status, model, timing, and token-usage counters are retained for
aggregate analysis. Per-model token and active-time totals are updated when a
response terminates. Response detail and usage snapshots roll off after seven
days while lifetime totals remain. Token counts and throughput come directly
from Responses streams. Account-level quota-per-hour estimates use consecutive
successful usage polls, without extra upstream reads or per-turn quota snapshots.
For each short/weekly window, only nondecreasing readings from the same unexpired
reset window are compared. Missing/invalid readings, reset-spanning intervals,
and baselines older than seven days are excluded. Temporary quota blocks never
substitute for actual usage measurements. Measured zero-point changes are
included when response activity exists, avoiding upward bias from percentage
rounding; intervals without recorded activity contribute neither consumption
nor time until a late response supplies activity.

Each interval stores its percentage-point change and clipped, merged proxy
activity spans. Responses crossing poll boundaries contribute only their time
inside each interval; simultaneous responses count once per account. Completed
streams and delayed relay telemetry update retained intervals and adjust their
lifetime contributions idempotently, even after original response rows expire.
The seven-day view sums intervals whose end time is within seven days, independent
of the hourly cleanup schedule. Interval records expire after seven days; compact
per-account/window lifetime totals remain. Rates divide summed consumption by
summed measured hours, including for All accounts, rather than averaging rates.
Each window has its own measured-time denominator. Lifetime quota statistics
start with newly measured intervals; historical quota rates are not invented or
backfilled. Only retained intervals can receive late corrections.

The proxy dashboard shares a model selector for token and quota statistics in
both periods. Quota attribution requires exactly one known response model across
an account's poll interval (including sequential, not just concurrent responses).
Mixed/unknown intervals contribute only to All models. Late telemetry can retract
previous model attribution from rolling and lifetime totals without changing the
account total. Per-model lifetime counters survive response/interval pruning.
Migration 022 starts model attribution without backfilling existing intervals;
their model remains unknown even if additional late responses arrive. Existing
account quota totals and token history are unchanged.

These are estimates of quota percentage points per active account hour, not
elapsed wall-clock hours. Model rates describe only attributable intervals and
can have less measured time than token rates. Direct leased-client traffic,
external account use, unrecorded interrupted streams, and upstream reporting
delays can skew the estimate. The dashboard shows supporting measured active
time and interval counts; unknown rates are displayed as a dash. No prompt,
response content, or new error-history table is added.
Upgrade the broker and relay edges together: the streamlined telemetry uses
relay protocol version 3.
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
5. Migration 009 removes legacy activation tables and state; migration 010 adds minimal window-pulse state; migration 011 adds public enrollment claims; migration 012 adds the per-account profile-stat cache; migrations 013-015 add dynamic weekly cadence; migration 016 adds response telemetry; migration 017 adds relay reservations, idempotency, and heartbeat state; migration 018 adds per-model rolling and lifetime proxy usage; migration 019 adds shared temporary quota blocks and removes obsolete quota-measurement records/columns while preserving token totals and response details; migration 020 adds the compact reset-poll schedule; migration 021 adds poll-interval quota measurements and durable lifetime counters. Migration 024 removes obsolete downloadable export snapshots and their index while preserving active credentials and retired history. Restore the corresponding automatic pre-migration backup before running software that predates a schema change. Removed measurement data remains recoverable from the pre-v19 backup.
6. Do not require account relogin for a normal upgrade.
