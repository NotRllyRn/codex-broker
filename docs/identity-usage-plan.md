# Identity usage and device sync

## Boundaries

Identities are named usage owners, independent of accounts and account labels.
Many devices and proxy keys can belong to one identity. Administrator sessions
and CSRF protect settings and dashboards. A random 256-bit identity code permits
device enrollment only; its SHA-256 digest is stored, and the code is displayed
once. Each enrolled device has its own random upload-only credential and can be
revoked independently. Rotating an enrollment code does not disconnect devices.

Proxy keys may be assigned to an identity. New proxy observations contribute
their original upstream usage to that identity. Historical proxy observations
are not retroactively reassigned. Uploaded direct usage is a separate source;
the installer explicitly asks whether each agent uses Broker or a direct
provider. Broker-routed logs must not also contribute uploaded client counts.
Mixed routes in one source require separate source directories/configuration;
we will not guess from model names or silently combine ambiguous totals.

## Small, durable protocol

- `POST /api/v1/usage/devices`: enroll with the identity code and a persistent
  installation ID. Enrollment retries return the same device credential without
  storing that credential in plaintext (the client supplies its random secret).
- `POST /api/v1/usage/batches`: authenticate with the device token; accept bounded
  gzip JSON batches of numerical usage events. Identity/device ownership comes
  exclusively from authentication, never an identity supplied in a batch.
- Stable source event IDs deduplicate retries and copies across devices within
  one identity. Reconciliation uses original provider identities, not device IDs.
  Device IDs identify uploaders, not billable events. Conflicting event payloads
  fail instead of silently adding or overwriting usage.
- Keep numerical events and deduplication keys permanently. Do not apply the
  seven-day proxy response cleanup to this ledger. Reuse dashboard pricing and
  daily chart code, with identity and device filters and explicit coverage text.

## Client

A standard-library Python helper supports the Codex and Pi formats inspected in
this project, including Pi auxiliary usage. No Node, ccusage, collector, or SSH
is required. One-time interactive setup stores configuration and installs the
current user's systemd timer, macOS LaunchAgent, or Windows scheduled task.
Hourly checks upload changed data and retry outages; startup/login catches up.
HTTPS certificate verification is mandatory; a private CA may be supplied.

First sync scans all retained logs. File offsets and parser state are committed
with a local SQLite outbox before upload. Successful server commits acknowledge
batches, after which the outbox is cleared. Scan file changes regardless of
timestamp; retain partial lines for the next scan, detect rewrites, and let
server deduplication handle rescans. Offline weeks do not limit recovery while
source records or the queued numerical events remain available.

## Implementation and verification

1. Add identity/device/usage schema and transactional services. Test secret
   boundaries, isolation, replay, conflicting duplicates, revocation, and totals.
2. Add settings, enrollment/upload HTTP routes, proxy-key attribution, and
   identity/device dashboard filtering. Test CSRF/authentication, size limits,
   malformed batches, root paths, and existing proxy accounting.
3. Add the helper and installers. Use synthetic transcripts and a local test
   server to verify parsing, copied history, partial writes, restarts, outages,
   acknowledgement loss, retries, and first-sync/backlog behavior.
4. Document installation, route classification, supported formats, limitations,
   credentials, removal, and recovery. Run the repository regression checks,
   commit focused changes, and push the feature branch. Do not deploy production.
