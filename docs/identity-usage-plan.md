# Identity usage: ccusage reports and native sync

## Scope

Keep identities, secret enrollment codes, and independently revocable devices.
Replace Broker's Codex/Pi parsers and Python helper with ccusage 20.0.43 (latest
verified 2026-10-24) and a Go executable. Accept every agent in ccusage's unified
JSON output without an agent allowlist. Include all local usage, including HTTP
proxy requests; remove direct/Broker route choices and identity proxy-key
attribution. Ordinary Broker proxy telemetry remains available separately.

## Accounting contract

ccusage reports aggregates, not individual responses. Run `daily --by-agent
--json --timezone UTC --offline`. Store the raw numerical daily report keyed by
(device, UTC day), plus a monotonically increasing client revision. Repeated
revisions are idempotent; stale revisions cannot overwrite newer data; changing
an existing revision's payload is a conflict. Newer reports replace that day's
snapshot atomically rather than adding cumulative totals. Distinct devices
contribute to the same identity. Copied/forked histories across devices are out
of scope. Never pretend a daily row is a response count.

Persist all historical snapshots independently of seven-day response cleanup.
Absent days never delete history. Reject automatic token decreases (such as
local log cleanup) without discarding other days; an explicit correction flag
permits intentional decreases. Retain old event tables for rollback/export,
but exclude them from new identity totals so a client's first ccusage import
cannot count its previous uploads twice. Remove the old event upload protocol
with a clear upgrade error for obsolete clients.

## Client and distribution

A native Go helper installs itself and a SHA-512-verified pinned native ccusage
binary from npm's platform package. Six targets: Windows/macOS/Linux, amd64 and
arm64. Neither Node, Python, Go, SQLite nor ccusage needs to be preinstalled.
Publish helper artifacts in CI and attach them to tagged releases. Installation
is one downloaded executable's `install` command; optional sh/PowerShell wrappers
only invoke that command.

Resolve source directories/configuration during setup and preserve ccusage's
environment for scheduled runs. Keep credentials and a small durable JSON
snapshot/ack journal in a private per-user directory. Lock per installation
using OS-released file locks. Scan all retained history each run, upload only
changed reports, and retry saved pending snapshots before newer ones. Save
revisions before network calls and acknowledgements after server commits.
An interrupted scan or lost acknowledgement must not create additional usage.
Initial enrollment is retry-safe and erases its code after success.

Use verified outbound HTTPS, reject redirects, support a private CA, and bound
report/download sizes and execution times. Install hourly plus startup/login
catch-up with systemd, launchd, or Windows Task Scheduler. Preserve queues and
history through outages; recovery requires retained logs or saved snapshots.
Do not automatically migrate Python state: enroll a new native device, revoke
the old one, and reimport retained history; old ledger remains outside totals.

## Implementation and checks

1. Add report schema, permissive ccusage JSON projection, monotonic snapshot
   ingestion, and identity summaries using ccusage costs and report-row labels.
2. Remove agent-specific upload handling, proxy identity assignments, Python
   client and route classification; keep identity enrollment/security.
3. Add native downloader, private durable journal, retry/upload client, OS
   scheduling, six-platform builds and simple install documentation.
4. Test with the real pinned ccusage binary and synthetic multi-agent logs;
   verify same/new/stale/conflicting uploads, partial failures, offline recovery,
   retention, malformed/oversized data, revocation, and migration preservation.
   Run Go race/vet/build and existing JS/integration regressions. Commit concise
   changes and push the existing feature branch; do not deploy production.
