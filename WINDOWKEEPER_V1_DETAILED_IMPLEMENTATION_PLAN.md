# Windowkeeper v1 Detailed Implementation Plan

> **Implementation update:** Windowkeeper now pins and installs its supported Codex release inside the image. Operator-supplied Codex package, version-output, and SHA-256 settings described below are superseded and are not required.

**Document status:** Implementation baseline  
**Target release:** Windowkeeper v1  
**Deployment model:** Single-operator, self-hosted, Docker-first service  
**Primary platform:** Linux `amd64` and `arm64`  
**Plan basis:** Original Windowkeeper research pack and master plan, plus the useful technical findings from research reports 02–14. Research-agent product “NO-GO” conclusions are intentionally excluded from the product decision, as directed. Their useful policy, security, protocol, reliability, and implementation findings are retained where applicable.

---

## 1. Executive implementation decision

Windowkeeper v1 remains a **multi-account automatic-activation service**.

The product will:

1. Enroll and independently operate multiple Codex-backed ChatGPT identities owned or explicitly administered by one operator.
2. Read each identity's current Codex rate-limit windows through the official Codex app-server interface.
3. Identify the account's short usage window from reported duration rather than hard-coded slot names.
4. Schedule one fixed minimal activation request after a confirmed or tightly constrained estimated short-window reset.
5. Persist enough evidence to recover after process, container, or host interruption without blindly repeating a possibly accepted request.
6. Present account state through a password-protected LAN dashboard and a local administrative CLI.
7. Encrypt account credentials at rest and materialize plaintext credentials only in a private runtime filesystem.
8. Maintain structured redacted logs, deduplicated incidents, and durable webhook delivery.
9. Ship as one hardened Docker image containing one exact, tested Codex release.

The central correctness promise is:

> For a given account and identified usage window, Windowkeeper will intentionally submit at most one accepted activation turn. If a crash leaves submission irreducibly ambiguous, Windowkeeper may skip that window rather than risk a duplicate.

Windowkeeper does **not** promise that every window will be activated under every failure. It prioritizes duplicate prevention over guaranteed activation after ambiguous transport failure.

---

## 2. Scope baseline

### 2.1 Features included in v1

- Multiple independently authenticated accounts.
- Device-code authentication as the default account-enrollment path.
- Optional Codex access-token authentication only after its release gate passes.
- Account display names and overlapping labels.
- Enable, disable, reauthenticate, replace credentials, and delete account flows.
- Short-window and weekly-window monitoring.
- Five-minute background polling with jitter and single-flight request coalescing.
- Confirmed reset scheduling from app-server rate-limit data.
- Evidence-gated estimated scheduling when authoritative reset data is unavailable.
- One fixed activation prompt per logical activation.
- Persistent activation threads for restart reconciliation.
- Manual activation through the same deduplicated operation path as scheduled activation.
- Server-rendered FastAPI/Jinja dashboard with native JavaScript enhancement.
- Server-Sent Events for committed state updates.
- Accessible desktop table and mobile card renderers from the same view models.
- Local Click CLI with machine-readable JSON output.
- SQLite persistence, forward-only migrations, and migration rollback copies.
- AES-256-GCM credential vault with account-specific derived keys.
- Opaque server-side administrator sessions and CSRF protection.
- Structured JSONL logs, live browser viewing, bounded search, and downloads.
- Generic JSON, Slack, and Discord webhooks.
- Deduplicated incidents and at-least-once webhook outbox delivery.
- Multi-architecture Docker images for Linux `amd64` and `arm64`.
- Exact Codex version pinning, schema compatibility checks, SBOM, provenance, and signed release images.

### 2.2 Conditional v1 features

The following feature may ship only if its dedicated proof passes against the exact release image:

- **Codex access-token enrollment.** It must prove token bootstrap, account read, rate-limit read, credential refresh or replacement, restart persistence, and account isolation. Failure removes this feature from the first v1 release without blocking device-code accounts.

### 2.3 Explicitly excluded from v1

- API-key accounts presented as ChatGPT subscription-window accounts.
- Direct calls to undocumented `wham/usage` endpoints.
- Account rotation, quota pooling, failover to another account, or automatic work routing.
- Shared credential files or copied active `auth.json` lineages.
- Multi-user dashboard access, roles, organizations, or permissions.
- Remote CLI authentication tokens.
- A public HTTP API, public SDK, or compatibility guarantee for internal routes.
- Multiple Windowkeeper replicas or scheduler leadership election.
- Redis, Celery, RQ, Dramatiq, APScheduler, or an external message broker.
- Editable activation prompts.
- Plugins, email notifications, escalation policies, or webhook templates.
- Mobile applications.
- Detailed cost analytics.
- Cloud-hosting support as an advertised deployment target.
- Backup and restore product features.
- A Rust rewrite or native Windowkeeper binary.
- Initial public PyPI or pipx distribution.
- Frontend frameworks, client routers, Node.js, CDN assets, webfonts, or external icon services.

---

## 3. Product principles and non-negotiable invariants

### 3.1 Evidence over inferred health

Every healthy, warning, stale, or failed state must expose the evidence behind it:

- When account authentication was last verified.
- When a complete rate-limit read last succeeded.
- Whether usage values are current, stale, partial, anomalous, or unavailable.
- Whether the next activation time is confirmed, estimated, or unknown.
- Which reset timestamp and window duration produced the schedule.
- Which activation attempt produced the last known result.
- Whether a pending operation is definite, ambiguous, or being reconciled.

A green status cannot be produced solely from process liveness or an old cached snapshot.

### 3.2 Account isolation

For every Windowkeeper account:

- One stable internal UUID is the authoritative local identity.
- One credential lineage belongs to that UUID.
- One encrypted credential bundle exists for the active vault generation.
- One private runtime directory is created when needed.
- One private `CODEX_HOME`, `HOME`, `TMPDIR`, and activation workspace are used.
- At most one Codex app-server process owns the credential lineage at a time.
- All same-account Codex RPCs are serialized.
- Notifications are routed only to that account's actor.
- No account path, file descriptor, environment value, token, thread, or rate-limit state may be reused by another account.

Any observed cross-account state is a release-blocking defect.

### 3.3 At-most-one accepted activation per window

For each `(account_id, window_key)`:

- The database allows at most one logical activation attempt.
- A logical attempt may have several physical operations, such as submission and reconciliation, but only one accepted turn.
- A new submission is forbidden while prior state is `TURN_DISPATCHING`, `TURN_ACCEPTED`, `RUNNING`, `AMBIGUOUS`, or `RECONCILING`.
- A retry is allowed only when non-acceptance has been positively established.
- Unknown submission outcome blocks another attempt for that same window.

### 3.4 Durable truth before external effects

Before any upstream effect, Windowkeeper commits a durable operation marker. After receiving upstream identifiers, it commits them immediately. It never acknowledges restart-sensitive work to the UI or CLI before the corresponding database transaction commits.

### 3.5 No secret leaves the secret boundary

Plaintext credentials, access tokens, refresh tokens, device codes, authorization headers, administrator passwords, session identifiers, CSRF tokens, webhook bearer URLs, signing secrets, and vault keys must never appear in:

- SQLite plaintext columns.
- Standard logs.
- JSONL logs.
- Browser log SSE.
- API problem details.
- CLI JSON output.
- Operation result objects.
- Webhook payloads.
- Crash traces.
- Metrics or diagnostics exports.

### 3.6 Single-instance operation

One data directory is owned by exactly one Windowkeeper process. The service acquires a nonblocking exclusive file lock before opening SQLite. A second instance exits without migrations, writes, scheduling, or Codex startup.

### 3.7 Presentation independence

Dashboard templates, CSS layouts, mobile renderers, and layout experiments consume stable application view models. Backend services cannot branch on a selected dashboard layout.

---

## 4. Terminology and domain definitions

### 4.1 Windowkeeper account

A local record representing one selected Codex/ChatGPT identity or workspace context. It is identified internally by a UUID and may have a user-defined display name and labels.

### 4.2 Credential lineage

The complete lifecycle of one account's authentication material from enrollment through refresh, checkpoint, replacement, logout, and deletion. A lineage is never cloned to create another Windowkeeper account.

### 4.3 Runtime generation

One materialized plaintext instance of an encrypted credential bundle, along with the account's private runtime directories and app-server process. Runtime generations receive a unique ID so stale processes and files can be detected.

### 4.4 Usage snapshot

A complete result of `account/rateLimits/read`, normalized and persisted with raw source values, account attribution, collection time, success/failure status, and selected Codex bucket.

### 4.5 Window key

A deterministic identifier for one scheduled activation opportunity.

Confirmed example:

```text
reported:<reset_unix_seconds>
```

Estimated example:

```text
estimated:<last_successful_activation_unix_ms>:<observed_duration_minutes>
```

### 4.6 Logical activation attempt

The durable record representing Windowkeeper's intent to activate one account once for one window key.

### 4.7 Activation operation

A physical action belonging to a logical activation attempt, such as `SUBMIT` or `RECONCILE`.

### 4.8 Definite failure

A failure where Windowkeeper can prove the activation turn was not accepted, such as local validation failure before transport write, app-server initialization failure, or an explicit pre-acceptance protocol rejection.

### 4.9 Ambiguous submission

A failure after a request may have been written but before acceptance or non-acceptance is known. Examples include EOF, timeout, process crash, malformed response, or Windowkeeper crash around the submission boundary.

### 4.10 Incident

A durable, deduplicated `ACTION_REQUIRED` or `ERROR` condition scoped to an account or the service.

---

## 5. High-level architecture

```text
Browser
  |
  | HTTPS or trusted LAN HTTP
  v
FastAPI / Jinja application, one Uvicorn worker
  |-- administrator session and CSRF middleware
  |-- HTML pages
  |-- private JSON routes under /api/internal/v1
  |-- state SSE stream
  |-- log SSE stream
  |
  v
Application service layer
  |-- AccountService
  |-- AuthenticationService
  |-- UsageService
  |-- SchedulingService
  |-- ActivationService
  |-- OperationService
  |-- IncidentService
  |-- WebhookService
  |-- LogQueryService
  |-- AdminSecurityService
  |-- VaultService
  |
  +----------------------+-----------------------+
  |                      |                       |
  v                      v                       v
SQLite DB worker     Runtime manager         Log fan-out writer
thread               and account actors      thread
  |                      |
  |                      +-- account mailbox
  |                      +-- isolated runtime tree
  |                      +-- one app-server child
  |                      +-- direct stdio JSONL client
  |
  +-- metadata, state, history, operations, incidents, outbox, sessions

Local CLI
  |
  +-- same configuration and application services
  +-- direct short DB/service operations
  +-- durable operations for runtime work
  +-- never starts a competing scheduler or account runtime manager
```

### 5.1 Process topology

The production container runs:

- One Windowkeeper Python process.
- One Uvicorn worker.
- One asyncio event loop.
- One FastAPI lifespan-owned structured task group.
- One dedicated SQLite owner thread.
- One logging writer thread.
- Zero or more on-demand account-scoped Codex app-server child processes.
- At most one app-server child per account.

### 5.2 Lifespan-owned long-running tasks

The structured task group owns:

- Deadline scheduler.
- Usage polling planner.
- Webhook dispatcher.
- Retention and maintenance loop.
- Runtime idle reaper.
- Account actor supervisors.
- SSE broadcaster lifecycle.
- Temporary log-level expiry reconciler.

Detached background tasks are forbidden for durable domain work.

---

## 6. Suggested repository and package layout

```text
windowkeeper/
├── pyproject.toml
├── uv.lock or equivalent exact dependency lock
├── Dockerfile
├── compose.yaml
├── compose.proxy.yaml
├── README.md
├── SECURITY.md
├── LICENSE
├── src/windowkeeper/
│   ├── __init__.py
│   ├── __main__.py
│   ├── version.py
│   ├── config.py
│   ├── errors.py
│   ├── clock.py
│   ├── ids.py
│   ├── domain/
│   │   ├── accounts.py
│   │   ├── authentication.py
│   │   ├── usage.py
│   │   ├── scheduling.py
│   │   ├── activation.py
│   │   ├── operations.py
│   │   ├── incidents.py
│   │   ├── webhooks.py
│   │   └── statuses.py
│   ├── services/
│   │   ├── account_service.py
│   │   ├── auth_service.py
│   │   ├── usage_service.py
│   │   ├── scheduling_service.py
│   │   ├── activation_service.py
│   │   ├── operation_service.py
│   │   ├── incident_service.py
│   │   ├── webhook_service.py
│   │   ├── vault_service.py
│   │   └── diagnostics_service.py
│   ├── repositories/
│   │   ├── db_worker.py
│   │   ├── accounts.py
│   │   ├── credentials.py
│   │   ├── usage.py
│   │   ├── activations.py
│   │   ├── operations.py
│   │   ├── incidents.py
│   │   ├── webhooks.py
│   │   ├── sessions.py
│   │   └── migrations.py
│   ├── codex/
│   │   ├── manifest.py
│   │   ├── schemas/
│   │   ├── protocol_types.py
│   │   ├── transport.py
│   │   ├── client.py
│   │   ├── process.py
│   │   ├── environment.py
│   │   ├── runtime_files.py
│   │   ├── account_adapter.py
│   │   └── fake_server/
│   ├── runtime/
│   │   ├── manager.py
│   │   ├── account_actor.py
│   │   ├── mailbox.py
│   │   ├── scheduler.py
│   │   ├── semaphores.py
│   │   └── shutdown.py
│   ├── vault/
│   │   ├── format.py
│   │   ├── crypto.py
│   │   ├── key_sources.py
│   │   ├── rotation.py
│   │   └── broker.py
│   ├── web/
│   │   ├── app.py
│   │   ├── lifespan.py
│   │   ├── auth.py
│   │   ├── csrf.py
│   │   ├── dependencies.py
│   │   ├── routes_html.py
│   │   ├── routes_api.py
│   │   ├── routes_sse.py
│   │   ├── problems.py
│   │   ├── view_models.py
│   │   ├── templates/
│   │   └── static/
│   ├── cli/
│   │   ├── main.py
│   │   ├── output.py
│   │   ├── accounts.py
│   │   ├── usage.py
│   │   ├── activation.py
│   │   ├── operations.py
│   │   ├── logs.py
│   │   ├── webhooks.py
│   │   ├── vault.py
│   │   └── diagnostics.py
│   ├── logging/
│   │   ├── setup.py
│   │   ├── schema.py
│   │   ├── redaction.py
│   │   ├── queue_handler.py
│   │   ├── writer.py
│   │   ├── files.py
│   │   └── search.py
│   └── webhooks/
│       ├── client.py
│       ├── payloads.py
│       ├── signing.py
│       ├── adapters.py
│       └── dispatcher.py
├── migrations/
├── tests/
│   ├── unit/
│   ├── property/
│   ├── contract/
│   ├── integration/
│   ├── crash/
│   ├── browser/
│   ├── container/
│   └── live/
└── tools/
    ├── generate_codex_schema.py
    ├── verify_codex_manifest.py
    └── release_checks.py
```

No UI-specific module may import runtime, Codex transport, or repository internals directly. It uses application services and versioned view models.

---

## 7. Configuration model

Configuration is loaded once at startup into an immutable validated object. Configuration errors prevent normal readiness but do not expose secrets in diagnostics.

### 7.1 Core environment variables

```dotenv
WINDOWKEEPER_DATA_DIR=/data
WINDOWKEEPER_RUNTIME_DIR=/run/windowkeeper
WINDOWKEEPER_LOG_DIR=/data/logs
WINDOWKEEPER_HOST=0.0.0.0
WINDOWKEEPER_PORT=8787
WINDOWKEEPER_ROOT_PATH=
WINDOWKEEPER_TIMEZONE=UTC
WINDOWKEEPER_TRUSTED_PROXIES=
WINDOWKEEPER_PUBLIC_BASE_URL=

WINDOWKEEPER_VAULT_KEY_FILE=/run/secrets/windowkeeper_vault_key
# Compatibility alternative, discouraged:
# WINDOWKEEPER_VAULT_KEY=wk1_...

WINDOWKEEPER_ADMIN_PASSWORD_FILE=/run/secrets/windowkeeper_admin_password
WINDOWKEEPER_COOKIE_SECURE=auto
WINDOWKEEPER_SESSION_IDLE_MINUTES=15
WINDOWKEEPER_SESSION_ABSOLUTE_HOURS=8
WINDOWKEEPER_REAUTH_MINUTES=5

WINDOWKEEPER_USAGE_POLL_SECONDS=300
WINDOWKEEPER_USAGE_TIMEOUT_SECONDS=15
WINDOWKEEPER_USAGE_REFRESH_CONCURRENCY=4
WINDOWKEEPER_AUTH_CONCURRENCY=2
WINDOWKEEPER_ACTIVATION_CONCURRENCY=3
WINDOWKEEPER_PROCESS_START_CONCURRENCY=2
WINDOWKEEPER_CODEX_IDLE_SECONDS=30

WINDOWKEEPER_ACTIVATION_SAFETY_DELAY_SECONDS=60
WINDOWKEEPER_ACTIVATION_JITTER_MAX_SECONDS=30
WINDOWKEEPER_ESTIMATED_SCHEDULE_ENABLED=true

WINDOWKEEPER_WEBHOOK_CONNECT_TIMEOUT_SECONDS=5
WINDOWKEEPER_WEBHOOK_READ_TIMEOUT_SECONDS=10
WINDOWKEEPER_WEBHOOK_MAX_CONNECTIONS=10

WINDOWKEEPER_LOG_LEVEL=INFO
WINDOWKEEPER_LOG_ROTATE_BYTES=26214400
WINDOWKEEPER_LOG_RETENTION_DAYS=30
WINDOWKEEPER_LOG_HARD_CAP_BYTES=1073741824
```

### 7.2 Configuration validation rules

- `/data` must exist, be writable by the service UID, and be on a local filesystem supported by SQLite WAL and `flock`.
- Runtime and persistent directories must not resolve to the same path.
- The vault key file must not be under `/data`.
- Exactly one vault key source may be configured.
- A vault key must decode to exactly 32 bytes and use the `wk1_` format.
- Trusted proxy configuration cannot contain wildcard `*`.
- A nonempty `root_path` must begin with `/` and must not end with `/` except when exactly `/`.
- Concurrency values must be positive and bounded by conservative maximums.
- Polling must not be configured below one minute in v1.
- The app refuses to start account workers when the Codex manifest, executable hash, or generated schema hash does not match the release manifest.
- Enabling estimated scheduling requires a positive safety delay and a bounded jitter.

### 7.3 Secret-source precedence

Secrets are never merged from multiple sources. For each secret:

1. Explicit file source.
2. Explicit environment value only when no file source exists.
3. Interactive hidden prompt for administrative CLI commands only.
4. Failure when no accepted source exists.

---

## 8. Startup lifecycle

### 8.1 Pre-database startup

1. Parse non-secret configuration.
2. Create or validate runtime directories using restrictive permissions.
3. Acquire `/data/windowkeeper.lock` with `LOCK_EX | LOCK_NB`.
4. Resolve the vault key without logging its source contents.
5. Verify the bundled Codex executable, release version, SHA-256, and schema manifest.
6. Initialize the sanitized logging pipeline.
7. Record a startup correlation ID.

If the singleton lock cannot be acquired, exit nonzero before opening SQLite.

### 8.2 Database startup

1. Open a dedicated migration connection.
2. Apply connection pragmas.
3. Read database schema version.
4. If the schema is newer than supported, close without writing and expose a typed diagnostic.
5. If migrations are required, create a same-directory rollback database using SQLite's backup API.
6. Apply forward migrations one at a time in explicit transactions.
7. Validate schema objects, foreign keys, partial unique indexes, and migration checksums.
8. Start the long-lived database-owner thread and connection.
9. Verify vault sentinel data and credential envelope versions.
10. Initialize or verify administrator password state.

### 8.3 Web startup

1. Construct application services.
2. Construct state and log broadcasters.
3. Start FastAPI serving after core startup is complete.
4. Mark readiness `starting` until runtime managers and scheduler initialization finish.
5. Load accounts and current state from SQLite.
6. Reset process-dependent states to `STARTING` or `STOPPED` rather than trusting pre-crash process markers.

### 8.4 Runtime reconciliation startup

For enabled accounts, bounded by global semaphores:

1. Check for unfinished activation operations.
2. Prioritize `AMBIGUOUS`, `RECONCILING`, `TURN_ACCEPTED`, and `RUNNING` attempts.
3. Restore the credential bundle to a new runtime generation.
4. Start app-server and verify initialization.
5. Verify account evidence.
6. Reconcile unfinished activation threads.
7. Perform a full rate-limit read.
8. Recalculate current status and the next eligible activation.
9. Stop the account runtime after the idle grace unless more work is queued.

Only after this planner starts successfully does readiness become `ok`.

### 8.5 Readiness semantics

`/health/live` returns success when the Python process and event loop can respond.

`/health/ready` returns:

- `ok` when configuration, vault, database, migrations, runtime manager, and scheduler are initialized.
- `starting` while startup reconciliation is in progress.
- `unavailable` for service-wide failures such as unusable vault key or unsupported schema.

Individual account failures do not make the entire service unready.

---

## 9. Shutdown lifecycle

Shutdown is bounded and phase-aware.

### 9.1 Admission stop

1. Mark service state `STOPPING`.
2. Reject new login, activation, credential mutation, webhook-test, and refresh operations with a retriable service-shutdown problem.
3. Stop the scheduler from claiming new deadlines.
4. Stop the webhook dispatcher from claiming new deliveries.
5. Continue serving read-only status briefly.

### 9.2 Durable-state drain

1. Allow short database transactions to finish.
2. Finish persistence of already-received app-server acceptance identifiers.
3. Mark externally uncertain operations `AMBIGUOUS` before process teardown.
4. Flush sanitized logs through the writer queue.

### 9.3 Child-process shutdown

For each account process:

1. Stop writing new RPC requests.
2. Cancel or interrupt active work only where the protocol and state permit.
3. Send termination to the child process group.
4. Wait up to the configured child grace period.
5. Escalate to process-group `SIGKILL`.
6. Drain remaining stdout and stderr without accepting new state transitions.
7. Checkpoint credential changes only when the credential file can be read consistently and the operation contract permits it.
8. Unlink the runtime generation.

### 9.4 Final close

1. Stop account actors.
2. Stop maintenance tasks.
3. Close SSE broadcasters.
4. Commit and close database worker.
5. Stop logging writer.
6. Release the singleton lock.
7. Exit before Docker's outer stop grace expires.

Recommended timings:

```text
Windowkeeper internal drain deadline: 35 seconds
Child TERM grace:                    10 seconds
Compose stop_grace_period:           45 seconds
```

---

## 10. Runtime manager and account actor design

### 10.1 Account actor ownership

One logical actor exists for each active account. It owns:

- Account-scoped mailbox.
- Runtime-generation state.
- App-server process reference.
- RPC client reference.
- Single-flight usage refresh future.
- Current login operation.
- Current activation operation.
- Idle shutdown timer.
- Notification attribution and dispatch.

The actor is the only component allowed to call Codex for that account.

### 10.2 Fixed mailbox lanes

The mailbox uses three lanes rather than a generic priority queue:

#### Control and recovery lane

- Service shutdown.
- Account disable.
- Delete preparation.
- Login cancellation.
- Credential invalidation.
- Startup activation reconciliation.
- Runtime crash handling.

#### State-changing lane

- Device-code enrollment.
- Reauthentication.
- Credential replacement.
- Scheduled activation.
- Manual activation.
- Definite-failure activation retry.

#### Refresh intent

- At most one pending coalesced usage-refresh request.
- Multiple callers share one future.
- A forced request arriving during an active refresh may set one follow-up flag rather than queueing arbitrary duplicates.

### 10.3 Fairness

The actor executes no more than three consecutive high-priority jobs while normal or refresh work is waiting, except for shutdown or destructive cancellation. FIFO ordering applies within each lane.

### 10.4 Global semaphores

- Activation: 3.
- Usage refresh: 4.
- Authentication: 2.
- Process start: 2.

A job acquires the global semaphore only immediately before the expensive action and releases it in a `finally` block. It does not hold a database transaction while waiting.

### 10.5 Runtime reuse

An app-server process is started on demand and reused for a short burst. After the actor becomes idle:

1. Start a 30-second monotonic idle timer.
2. Cancel the timer if new work arrives.
3. At expiry, quiesce app-server.
4. Stop the process.
5. Checkpoint changed credentials.
6. Remove the plaintext runtime generation.

A five-minute usage poll must not keep every process alive continuously.

### 10.6 Crash containment

A child crash affects only its account actor. The actor:

- Fails all pending RPC futures.
- Classifies active operations by phase.
- Marks a possible post-write activation failure ambiguous.
- Opens or updates the relevant account incident.
- Cleans up the process group.
- Starts a fresh runtime only when reconciliation or queued work requires it.

---

## 11. Codex release and compatibility contract

### 11.1 Exact release pin

Every Windowkeeper release bundles exactly one reviewed stable Codex release. The release manifest contains:

```json
{
  "windowkeeper_version": "1.0.0",
  "codex_version": "<exact stable version>",
  "codex_release_asset": "<architecture-specific asset name>",
  "codex_sha256": "<exact digest>",
  "stable_schema_sha256": "<exact digest>",
  "protocol_profile": "windowkeeper-codex-v1",
  "experimental_api": false
}
```

The exact Codex version is selected during Phase 0. Version numbers found in research reports are historical research baselines, not a permanent implementation constant.

### 11.2 Build-time steps

For each architecture:

1. Download the exact official standalone Codex package.
2. Verify its release checksum against a repository-committed digest.
3. Extract only required runtime files.
4. Invoke the exact binary to generate its stable JSON Schema.
5. Compare generated schema against committed fixtures.
6. Run the app-server contract suite.
7. Store release metadata in OCI labels and `windowkeeper version --json`.

### 11.3 Runtime verification

Before account workers start:

- Execute `codex --version` or the equivalent deterministic version query.
- Hash the executable or package files.
- Verify the schema/compatibility manifest.
- Refuse account operations when any value differs.

The web dashboard, CLI diagnostics, vault verification, database inspection, logs, and account configuration remain available in compatibility-blocked mode.

### 11.4 Stable protocol only

Windowkeeper uses:

- stdio transport.
- Newline-delimited JSON framing.
- Stable initialization and account methods.
- Stable rate-limit methods.
- Stable thread/turn methods required by activation.

Windowkeeper does not:

- Enable experimental API capabilities.
- Use experimental WebSocket transport.
- Parse undocumented browser pages.
- Treat private auth-token claims as authoritative product state.
- Assume behavior from a moving `main` branch at runtime.

### 11.5 Upgrade procedure

A Codex upgrade requires:

1. Open a dedicated compatibility branch.
2. Update exact release assets and digests.
3. Regenerate stable schemas.
4. Review schema diffs for every consumed message.
5. Update typed protocol adapters.
6. Run fake-server contract tests.
7. Run container tests on both architectures.
8. Run protected two-account authentication and rate-limit tests.
9. Run one protected activation and crash-recovery test.
10. Publish only as part of a new Windowkeeper release.

There is no independent Codex self-update inside the container.

---

## 12. App-server process and transport implementation

### 12.1 Process spawn

The process manager launches Codex with:

- Executable path from the verified release manifest.
- App-server subcommand.
- A dedicated process group or session.
- Account-specific working directory.
- Explicit file descriptors for stdin, stdout, and stderr.
- A minimal environment allowlist.
- No inherited shell environment wholesale.

Example environment shape:

```text
PATH=<image-controlled path>
HOME=/run/windowkeeper/accounts/<account_uuid>/<generation>/home
CODEX_HOME=/run/windowkeeper/accounts/<account_uuid>/<generation>/codex-home
TMPDIR=/run/windowkeeper/accounts/<account_uuid>/<generation>/tmp
LANG=C.UTF-8
LC_ALL=C.UTF-8
TZ=UTC
NO_COLOR=1
```

Proxy settings, cloud credentials, API keys, host `HOME`, XDG directories, and unrelated service secrets are excluded unless a later compatibility decision explicitly allowlists them.

### 12.2 Runtime directory layout

```text
/run/windowkeeper/accounts/<account_uuid>/<generation_uuid>/
├── home/                    mode 0700
├── codex-home/              mode 0700
│   ├── auth.json            mode 0600
│   └── config.toml          mode 0600
├── tmp/                     mode 0700
├── workspace/               mode 0700, empty for activation
└── metadata.json            non-secret runtime ownership metadata
```

All paths are created with `umask 077`. Symlinks are rejected. Path creation uses directory file descriptors and no-follow semantics where available.

### 12.3 JSONL framing

The stdout reader:

- Reads complete lines with a strict maximum frame size.
- Rejects invalid UTF-8 or malformed JSON.
- Validates message shape against generated protocol models.
- Correlates responses by request ID.
- Routes notifications through account-scoped handlers.
- Never logs raw frames.
- Fails all pending calls on EOF or parse failure.

Recommended maximum frame size begins at 8 MiB and is adjusted only through compatibility testing.

### 12.4 Writer discipline

- One asyncio lock protects writes.
- A request is serialized to one compact JSON line.
- Request metadata is recorded before the write begins.
- Transport instrumentation distinguishes pre-write validation, write-started, and write-completed phases.
- Raw request bodies are never logged.

The transport must expose whether zero bytes were written. A generic connection error without this evidence is not classified as definite non-acceptance.

### 12.5 Request lifecycle

Each request record contains in memory:

- Request ID.
- Method.
- Account UUID.
- Operation UUID.
- Monotonic start time.
- Deadline.
- Write phase.
- Expected response adapter.

On timeout, the RPC future is failed, but the domain layer determines whether the operation is definite or ambiguous based on the method and write phase.

### 12.6 stderr handling

stderr is drained independently to prevent process blockage. It passes through:

1. Byte and line limits.
2. UTF-8 replacement handling.
3. High-confidence secret scrubbing.
4. Structured wrapping with account and runtime IDs.
5. Rate limiting for repeated identical messages.

Raw stderr is never forwarded directly to logs or the browser.

### 12.7 Initialization

After spawn:

1. Wait for process readiness within a startup deadline.
2. Send `initialize` with only stable capabilities.
3. Validate response version and capabilities.
4. Send `initialized` notification.
5. Mark the runtime available.

Failure before successful initialization is a definite pre-acceptance failure for any queued activation.

### 12.8 Process deadlines and termination

Separate deadlines exist for:

- Spawn/readiness.
- Initialization.
- Account read.
- Rate-limit read.
- Login start/cancel.
- Thread creation.
- Turn start.
- Turn terminal completion.
- Reconciliation read.

A hung process is interrupted where supported, then sent `SIGTERM`, then `SIGKILL` after a bounded grace. Signals target the process group so helper processes cannot remain orphaned.

---

## 13. Authentication and account enrollment

### 13.1 Supported account types

#### Device-code account

Required v1 path and default UI choice.

#### Codex access-token account

Conditional path. The user-facing name is **Codex access token**, even if an upstream internal protocol enum uses different terminology.

#### API-key account

Not included as a Windowkeeper subscription-window account. API-key workflows belong to a separate future product mode with different usage and billing semantics.

### 13.2 Account identity model

Windowkeeper's account UUID is always authoritative locally. Upstream account evidence includes:

- Display email when returned.
- Plan type when returned.
- Authentication mode.
- Optional operator-provided workspace ID.
- Forced-workspace verification result.
- Credential fingerprint derived from ciphertext metadata, never from logging token content.

Email and plan are evidence for the operator, not a stable cryptographic upstream identifier.

### 13.3 Device-code enrollment flow

1. Administrator enters display name, optional workspace ID, and labels.
2. Server validates uniqueness and creates a disabled `accounts` row in `ENROLLING` state.
3. Create a durable login operation.
4. Account actor allocates a new runtime generation without credentials.
5. Generate a minimal Codex configuration with file-based credential storage and optional forced workspace ID.
6. Start app-server.
7. Call the supported device-code login-start method.
8. Persist login ID, expiry, non-secret verification URI, and operation state. The user code is treated as sensitive and stored only as needed for the active session; it is never logged.
9. Return login progress to the current authenticated administrator session and state SSE.
10. Poll or consume the documented completion notification according to the app-server contract.
11. On success, call account read and rate-limit read.
12. Verify expected workspace restriction when configured.
13. Stop or quiesce app-server.
14. Read and validate the complete credential file bundle.
15. Encrypt and commit the credential bundle.
16. Delete the plaintext runtime generation.
17. Mark the account enabled or await explicit enable, according to the final UI flow.
18. Schedule its next usage poll and activation decision.

### 13.4 Login timeout and cancellation

- Default device-code enrollment deadline: 10 minutes, subject to the pinned release's observed expiration.
- Administrator cancellation invokes the matching login-cancel method.
- Timeout invokes cancellation, waits briefly for completion, and then terminates the runtime.
- Cancellation or timeout leaves no active credential bundle unless upstream successfully authenticated and the operator explicitly continues the flow.

### 13.5 Reauthentication

Reauthentication is a credential-lineage replacement, not an in-place untracked mutation.

1. Require recent administrator password verification.
2. Disable activation admission for the account.
3. Finish or classify active operations.
4. Create a replacement login operation.
5. Perform device-code login in a new runtime generation.
6. Validate account evidence and optional workspace restriction.
7. Encrypt the new bundle to a shadow credential row.
8. Atomically promote it and retire the old active bundle.
9. Resume account operations.

### 13.6 Access-token enrollment

When enabled by release manifest:

1. Require recent administrator password verification.
2. Accept token through hidden CLI input, stdin, or an authenticated browser request body protected by CSRF and TLS/trusted LAN controls.
3. Never accept it as a URL parameter, command argument, or environment override.
4. Start a private runtime with no active app-server.
5. Invoke the official Codex CLI token-login path with the token on stdin.
6. Confirm process success and credential-file creation.
7. Start app-server in the same isolated home.
8. Validate account read and rate-limit read.
9. Encrypt and checkpoint the resulting bundle.
10. Zero or release process buffers as practical and remove plaintext files.

If rate-limit reads or persistence fail in protected testing, this enrollment path is not exposed in the release.

### 13.7 Logout and credential replacement

Logout:

- Requires recent reauthentication.
- Disables scheduled activation first.
- Stops the account runtime.
- Invokes app-server logout when a runtime can be safely restored.
- Deletes encrypted credential rows transactionally.
- Resolves credential-related incidents with reason `administratively_closed`.
- Leaves anonymized operational history according to deletion choice.

Credential replacement uses a shadow bundle and atomic promotion so a crash cannot leave the account without either the old or new usable credential set.

### 13.8 Multi-workspace handling

- Windowkeeper does not enumerate workspaces.
- One Windowkeeper account represents one selected workspace context.
- When an identity has multiple workspaces, the operator supplies the intended workspace ID.
- Codex configuration enforces that workspace when supported.
- A mismatch fails closed and prevents usage polling and activation.

---

## 14. Credential vault design

### 14.1 Root key

The vault root key is 32 cryptographically random bytes encoded as:

```text
wk1_<unpadded-base64url>
```

Passphrases are not accepted directly as vault keys in v1.

### 14.2 Encryption primitive

- AES-256-GCM.
- Fresh 96-bit nonce for every encryption.
- 256-bit per-account derived key.
- HKDF-SHA-256 for key derivation.

Suggested derivation inputs:

```text
salt = database_instance_id
info = "windowkeeper/credential-bundle/v1" || key_id || account_uuid
```

Associated authenticated data includes:

```text
windowkeeper_instance_id
account_uuid
credential_bundle_id
key_id
envelope_version
payload_schema_version
auth_mode
```

### 14.3 Credential bundle payload

Initial payload schema:

```json
{
  "schema_version": 1,
  "codex_version": "<exact version>",
  "files": [
    {
      "relative_path": "auth.json",
      "mode": 384,
      "sha256": "<plaintext file digest>",
      "content_base64": "<file bytes>"
    }
  ],
  "captured_at": "<UTC timestamp>",
  "workspace_constraint": "<optional workspace ID>"
}
```

The encrypted bundle is opaque to ordinary database migrations. Any future additional credential file must be added through an explicit allowlist and payload schema version.

### 14.4 Envelope columns

A credential row contains:

- Bundle UUID.
- Account UUID.
- Active/shadow/retired state.
- Envelope version.
- Payload schema version.
- Key ID.
- Nonce.
- Ciphertext and authentication tag as returned by the library.
- AAD metadata hash.
- Created and promoted timestamps.
- Non-secret runtime compatibility metadata.

### 14.5 Materialization

1. Load encrypted bundle through the database worker.
2. Derive the account key.
3. Decrypt and validate the GCM tag.
4. Validate payload schema, allowed relative paths, modes, and file digests.
5. Create a new runtime generation using no-follow file operations.
6. Write each file with mode `0600` under a `0700` directory.
7. Re-read and verify digest and mode.
8. Start Codex only after validation passes.

A malformed or partially written runtime is deleted and never used.

### 14.6 Checkpointing refreshed credentials

Because Codex may refresh credentials during operation:

1. Stop or quiesce app-server according to the pinned release contract.
2. Open the allowlisted credential file with no-follow semantics.
3. Enforce maximum size and strict file type.
4. Read exact bytes.
5. Verify JSON parse only for basic integrity; do not depend on private token-field schema.
6. Build a new payload.
7. Encrypt with a fresh nonce.
8. Commit a new active version transactionally while retiring the prior version.
9. Only then report checkpoint-dependent operation success.
10. Remove plaintext runtime files.

### 14.7 Vault-key rotation

Rotation is two-phase:

#### Prepare

- Operator supplies a new valid key through a protected source.
- Service verifies old key and new key.
- Decrypt every active bundle with the old key.
- Encrypt shadow copies under the new key and a new key ID.
- Verify every shadow bundle by decrypting it.
- Persist a prepared rotation sentinel.

#### Promote

- Operator switches the external key source.
- On startup or explicit continuation, service recognizes the prepared sentinel.
- It verifies all shadow bundles with the new key.
- It atomically promotes the shadow generation and retires the old generation.
- It clears the rotation sentinel.

A crash leaves either the old active set or the fully prepared new set usable.

### 14.8 Deletion semantics

Windowkeeper unlinks runtime plaintext promptly but makes no secure-erasure promise for Python memory, tmpfs snapshots, container layers, host RAM, SQLite pages, or storage snapshots. Persistent storage contains ciphertext, not plaintext credential files.

---

## 15. Database architecture and pragmas

### 15.1 Ownership model

A `DatabaseWorker` thread owns one long-lived `sqlite3.Connection` with `check_same_thread=True`. Async code submits callables to a queue and awaits thread-safe futures.

Benefits:

- Explicit connection ownership.
- Serialized transactions.
- No event-loop blocking.
- Simple fault injection.
- No ORM object lifecycle.
- No false expectation of multiple SQLite writers.

### 15.2 Connection initialization

Every database connection runs:

```sql
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = FULL;
PRAGMA trusted_schema = OFF;
```

At database creation, before tables:

```sql
PRAGMA auto_vacuum = INCREMENTAL;
PRAGMA journal_mode = WAL;
```

The service verifies returned values and refuses unsafe fallback.

`secure_delete` remains off normally and may be enabled for targeted secret-row replacement/deletion transactions, while documentation explicitly states its limits under WAL and filesystem behavior.

### 15.3 Transaction rules

- Use explicit `BEGIN IMMEDIATE` for write transactions that claim or transition work.
- Keep transactions short.
- Never await network, subprocess, or sleep operations inside a transaction.
- Use repository methods that make transaction scope visible.
- Do not use arbitrary `executescript()` inside an assumed outer transaction.
- Return domain DTOs, not live SQLite rows, across the thread boundary.

### 15.4 Identifiers and time

- UUIDv7 or another sortable random UUID format for domain IDs.
- UTC Unix milliseconds for durable timestamps.
- Monotonic time only for in-process deadlines and elapsed-time measurement.
- Store timezone preference separately for display.
- Never persist naive local timestamps.

---

## 16. Proposed database schema

The following schema is a detailed starting point. Migration files remain the authoritative implementation.

### 16.1 Schema and instance metadata

```sql
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE instance_metadata (
    singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
    instance_uuid TEXT NOT NULL UNIQUE,
    created_at_ms INTEGER NOT NULL,
    schema_created_by_version TEXT NOT NULL
) STRICT;

CREATE TABLE vault_state (
    singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
    active_key_id TEXT NOT NULL,
    prepared_key_id TEXT,
    rotation_state TEXT NOT NULL,
    sentinel_nonce BLOB NOT NULL,
    sentinel_ciphertext BLOB NOT NULL,
    updated_at_ms INTEGER NOT NULL
) STRICT;
```

### 16.2 Accounts and labels

```sql
CREATE TABLE accounts (
    account_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    auth_mode TEXT NOT NULL,
    workspace_constraint TEXT,
    enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
    lifecycle_state TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    deleted_at_ms INTEGER,
    public_identity_token TEXT NOT NULL UNIQUE
) STRICT;

CREATE UNIQUE INDEX accounts_active_name_uq
ON accounts(lower(display_name))
WHERE deleted_at_ms IS NULL;

CREATE TABLE labels (
    label_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX labels_name_uq ON labels(lower(name));

CREATE TABLE account_labels (
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES labels(label_id) ON DELETE CASCADE,
    PRIMARY KEY (account_id, label_id)
) STRICT;
```

### 16.3 Credential bundles

```sql
CREATE TABLE credential_bundles (
    bundle_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    state TEXT NOT NULL,
    envelope_version INTEGER NOT NULL,
    payload_schema_version INTEGER NOT NULL,
    key_id TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    aad_sha256 BLOB NOT NULL,
    codex_version TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    promoted_at_ms INTEGER,
    retired_at_ms INTEGER
) STRICT;

CREATE UNIQUE INDEX credential_one_active_uq
ON credential_bundles(account_id)
WHERE state = 'ACTIVE';

CREATE UNIQUE INDEX credential_nonce_uq
ON credential_bundles(account_id, key_id, nonce);
```

### 16.4 Current account state

```sql
CREATE TABLE account_state (
    account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
    auth_state TEXT NOT NULL,
    worker_state TEXT NOT NULL,
    overall_state TEXT NOT NULL,
    upstream_email TEXT,
    upstream_plan TEXT,
    workspace_verified INTEGER,
    last_auth_verified_at_ms INTEGER,
    last_runtime_started_at_ms INTEGER,
    last_runtime_stopped_at_ms INTEGER,
    active_operation_id TEXT,
    last_error_code TEXT,
    last_error_summary TEXT,
    state_version INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
) STRICT;
```

### 16.5 Current usage and history

```sql
CREATE TABLE usage_current (
    account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL,
    selected_limit_id TEXT,
    short_raw_slot TEXT,
    short_used_percent_raw INTEGER,
    short_duration_minutes INTEGER,
    short_resets_at_s INTEGER,
    short_anomaly INTEGER NOT NULL DEFAULT 0,
    weekly_raw_slot TEXT,
    weekly_used_percent_raw INTEGER,
    weekly_duration_minutes INTEGER,
    weekly_resets_at_s INTEGER,
    weekly_anomaly INTEGER NOT NULL DEFAULT 0,
    plan_type TEXT,
    rate_limit_reached_type TEXT,
    complete_read_at_ms INTEGER,
    last_attempt_at_ms INTEGER NOT NULL,
    stale INTEGER NOT NULL CHECK (stale IN (0,1)),
    last_error_code TEXT,
    last_error_summary TEXT,
    source TEXT NOT NULL,
    state_version INTEGER NOT NULL
) STRICT;

CREATE TABLE usage_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    attempted_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER,
    success INTEGER NOT NULL CHECK (success IN (0,1)),
    selected_limit_id TEXT,
    normalized_json TEXT,
    raw_shape_summary_json TEXT,
    error_code TEXT,
    error_summary TEXT,
    duration_ms INTEGER NOT NULL
) STRICT;

CREATE INDEX usage_snapshots_account_time_idx
ON usage_snapshots(account_id, attempted_at_ms DESC);
```

`normalized_json` stores complete non-secret normalized evidence, including auxiliary limit buckets when retained for diagnostics. It does not store credentials or raw protocol payloads.

### 16.6 Durable operations

```sql
CREATE TABLE operations (
    operation_id TEXT PRIMARY KEY,
    account_id TEXT REFERENCES accounts(account_id) ON DELETE SET NULL,
    kind TEXT NOT NULL,
    trigger TEXT NOT NULL,
    requested_by_session_hash BLOB,
    state TEXT NOT NULL,
    progress_code TEXT,
    progress_summary TEXT,
    result_json TEXT,
    error_code TEXT,
    error_summary TEXT,
    created_at_ms INTEGER NOT NULL,
    started_at_ms INTEGER,
    completed_at_ms INTEGER,
    lease_token TEXT,
    lease_expires_at_ms INTEGER,
    state_version INTEGER NOT NULL
) STRICT;

CREATE INDEX operations_account_time_idx
ON operations(account_id, created_at_ms DESC);

CREATE INDEX operations_runnable_idx
ON operations(state, created_at_ms)
WHERE state IN ('QUEUED','RETRY_SCHEDULED');
```

### 16.7 Activation attempts and physical operations

```sql
CREATE TABLE activation_attempts (
    activation_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    window_key TEXT NOT NULL,
    trigger TEXT NOT NULL,
    prompt_version INTEGER NOT NULL,
    prompt_sha256 BLOB NOT NULL,
    schedule_source TEXT NOT NULL,
    schedule_confidence TEXT NOT NULL,
    basis_reset_at_s INTEGER,
    basis_duration_minutes INTEGER,
    scheduled_for_ms INTEGER,
    state TEXT NOT NULL,
    upstream_thread_id TEXT,
    upstream_turn_id TEXT,
    client_user_message_id TEXT NOT NULL,
    normalized_result TEXT,
    terminal_status TEXT,
    ambiguity_reason TEXT,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER,
    state_version INTEGER NOT NULL,
    UNIQUE(account_id, window_key)
) STRICT;

CREATE INDEX activation_attempts_account_time_idx
ON activation_attempts(account_id, created_at_ms DESC);

CREATE TABLE activation_operations (
    activation_operation_id TEXT PRIMARY KEY,
    activation_id TEXT NOT NULL REFERENCES activation_attempts(activation_id) ON DELETE CASCADE,
    operation_kind TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    state TEXT NOT NULL,
    request_id TEXT,
    write_started_at_ms INTEGER,
    write_completed_at_ms INTEGER,
    accepted_at_ms INTEGER,
    completed_at_ms INTEGER,
    upstream_thread_id TEXT,
    upstream_turn_id TEXT,
    error_code TEXT,
    error_summary TEXT,
    evidence_json TEXT,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    UNIQUE(activation_id, operation_kind, attempt_number)
) STRICT;

CREATE UNIQUE INDEX activation_one_unfinished_operation_uq
ON activation_operations(activation_id)
WHERE state IN ('STARTED','REQUEST_WRITING','AWAITING_RESPONSE','RECONCILING');
```

### 16.8 Incidents

```sql
CREATE TABLE incidents (
    incident_id TEXT PRIMARY KEY,
    scope_kind TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    problem_type TEXT NOT NULL,
    state TEXT NOT NULL,
    severity TEXT NOT NULL,
    summary TEXT NOT NULL,
    current_error_code TEXT,
    occurrence_count INTEGER NOT NULL,
    opened_at_ms INTEGER NOT NULL,
    last_seen_at_ms INTEGER NOT NULL,
    resolved_at_ms INTEGER,
    resolution_reason TEXT,
    state_version INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX incident_one_open_uq
ON incidents(scope_kind, scope_key, problem_type)
WHERE state = 'OPEN';
```

### 16.9 Webhook destinations and deliveries

```sql
CREATE TABLE webhook_destinations (
    destination_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
    encrypted_url_bundle_id TEXT NOT NULL,
    encrypted_signing_secret_bundle_id TEXT,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE webhook_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    subject TEXT NOT NULL,
    occurred_at_ms INTEGER NOT NULL,
    canonical_body BLOB NOT NULL,
    incident_id TEXT REFERENCES incidents(incident_id) ON DELETE SET NULL,
    created_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE webhook_deliveries (
    delivery_id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES webhook_events(event_id) ON DELETE CASCADE,
    destination_id TEXT NOT NULL REFERENCES webhook_destinations(destination_id) ON DELETE CASCADE,
    state TEXT NOT NULL,
    attempt_count INTEGER NOT NULL,
    next_attempt_at_ms INTEGER NOT NULL,
    immutable_body BLOB NOT NULL,
    content_type TEXT NOT NULL,
    lease_token TEXT,
    lease_expires_at_ms INTEGER,
    last_status_code INTEGER,
    last_error_code TEXT,
    last_response_excerpt TEXT,
    created_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER,
    UNIQUE(event_id, destination_id)
) STRICT;

CREATE INDEX webhook_delivery_due_idx
ON webhook_deliveries(state, next_attempt_at_ms)
WHERE state IN ('PENDING','RETRY');
```

### 16.10 Administrator sessions

```sql
CREATE TABLE admin_credentials (
    singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
    password_hash TEXT NOT NULL,
    password_changed_at_ms INTEGER NOT NULL,
    bootstrap_complete INTEGER NOT NULL CHECK (bootstrap_complete IN (0,1))
) STRICT;

CREATE TABLE admin_sessions (
    session_id_hash BLOB PRIMARY KEY,
    csrf_token_hash BLOB NOT NULL,
    created_at_ms INTEGER NOT NULL,
    last_seen_at_ms INTEGER NOT NULL,
    idle_expires_at_ms INTEGER NOT NULL,
    absolute_expires_at_ms INTEGER NOT NULL,
    reauthenticated_at_ms INTEGER,
    revoked_at_ms INTEGER,
    client_fingerprint_hash BLOB
) STRICT;

CREATE INDEX admin_sessions_expiry_idx
ON admin_sessions(absolute_expires_at_ms, idle_expires_at_ms);

CREATE TABLE admin_login_throttle (
    throttle_key_hash BLOB PRIMARY KEY,
    failure_count INTEGER NOT NULL,
    first_failure_at_ms INTEGER NOT NULL,
    last_failure_at_ms INTEGER NOT NULL,
    blocked_until_ms INTEGER
) STRICT;
```

### 16.11 Settings and temporary overrides

```sql
CREATE TABLE settings (
    setting_key TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE log_level_overrides (
    override_id TEXT PRIMARY KEY,
    level TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_key TEXT,
    created_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    created_by_session_hash BLOB
) STRICT;
```

---

## 17. Migration system

### 17.1 Migration format

Each migration is a numbered Python module or controlled SQL file with:

- Integer version.
- Stable name.
- SHA-256 checksum.
- `apply(connection)` function.
- `validate(connection)` function.
- No downgrade function.

### 17.2 Upgrade transaction

For each migration:

1. Verify checksum against the migration ledger.
2. Begin an explicit transaction.
3. Run statements one by one.
4. Populate or transform data.
5. Validate expected tables, columns, indexes, and invariants.
6. Insert the migration ledger row.
7. Commit.

### 17.3 Rollback copy

Before the first migration:

- Create a temporary same-directory database using `Connection.backup()`.
- Run `quick_check` on the copy.
- Record source schema version and copy digest metadata.
- On any migration failure, close source connections and restore through a controlled replacement procedure.

The rollback copy is an internal upgrade safety mechanism, not a user-facing backup feature.

### 17.4 Credential-schema independence

Database migrations cannot require decryption of every credential bundle unless a dedicated vault migration is being performed. Database schema version, encryption envelope version, and decrypted payload schema version are distinct.

---

## 18. Application service boundaries

### 18.1 AccountService

Responsibilities:

- Create account records.
- Rename and label accounts.
- Enable or disable accounts.
- Coordinate deletion.
- Produce account view models.
- Enforce lifecycle preconditions.

Does not spawn Codex directly.

### 18.2 AuthenticationService

Responsibilities:

- Create login operations.
- Validate auth-mode availability.
- Coordinate device-code or access-token enrollment through the actor.
- Persist non-secret identity evidence.
- Promote replacement credential bundles.
- Mark `AUTH_REQUIRED` states.

### 18.3 UsageService

Responsibilities:

- Request a coalesced refresh.
- Normalize complete app-server responses.
- Preserve prior valid data on failure.
- Mark stale state.
- Persist complete snapshots.
- Publish committed view-model updates.

### 18.4 SchedulingService

Responsibilities:

- Classify windows.
- Calculate confirmed or estimated activation decisions.
- Generate deterministic jitter.
- Enforce one attempt per window key.
- Query the next deadline.
- Cancel obsolete planned attempts.

It contains pure deterministic logic and receives injected clock and jitter dependencies.

### 18.5 ActivationService

Responsibilities:

- Create logical activation attempts.
- Validate admission.
- Construct fixed-prompt execution settings.
- Coordinate thread creation, turn submission, completion, and result normalization.
- Classify definite versus ambiguous failure.
- Start reconciliation.
- Prevent duplicate submission.

### 18.6 OperationService

Responsibilities:

- Create durable operations.
- Claim runnable operations.
- Store progress and result envelopes.
- Enforce operation cancellation rules.
- Expose polling resources.
- Publish state changes over SSE after commit.

### 18.7 IncidentService

Responsibilities:

- Map durable account/service transitions to incident keys.
- Open at most one incident per key.
- Increment occurrence counts.
- Resolve only on fresh affirmative success or administrative closure.
- Create canonical webhook events in the same transaction.

### 18.8 WebhookService

Responsibilities:

- Manage encrypted destinations.
- Render immutable per-destination bodies.
- Claim and finalize outbox deliveries.
- Apply retry policy.
- Expose test operations.

### 18.9 VaultService

Responsibilities:

- Validate key sources.
- Encrypt/decrypt bundles.
- Materialize and checkpoint credentials.
- Rotate vault keys.
- Provide no general-purpose plaintext-secret API.

### 18.10 DiagnosticsService

Responsibilities:

- Return redacted configuration summaries.
- Validate database and migration state.
- Validate Codex compatibility manifest.
- Inspect process and scheduler health.
- Verify vault sentinel and encrypted row readability.
- Never expose credential contents.

---

## 19. Durable operation model

### 19.1 Operation states

```text
QUEUED
CLAIMED
RUNNING
WAITING_FOR_USER
WAITING_FOR_UPSTREAM
RETRY_SCHEDULED
SUCCEEDED
FAILED
CANCELLED
AMBIGUOUS
```

Not every kind uses every state.

### 19.2 Claiming

The service claims an operation in a short transaction:

```sql
UPDATE operations
SET state = 'CLAIMED',
    lease_token = ?,
    lease_expires_at_ms = ?,
    started_at_ms = COALESCE(started_at_ms, ?),
    state_version = state_version + 1
WHERE operation_id = ?
  AND state IN ('QUEUED','RETRY_SCHEDULED')
RETURNING *;
```

External work happens outside the transaction. Finalization checks the lease token to avoid stale workers overwriting current state.

### 19.3 Operation result envelope

```json
{
  "api_version": "windowkeeper.dev/operation/v1",
  "kind": "OperationResult",
  "operation_id": "...",
  "operation_kind": "usage.refresh",
  "state": "SUCCEEDED",
  "completed_at": "...",
  "data": {}
}
```

Result objects contain only non-secret information.

### 19.4 Cancellation

Cancellation is accepted only when the operation kind and phase support it. A request that may already have crossed an external side-effect boundary is not converted to `CANCELLED`; it becomes or remains `AMBIGUOUS` until reconciled.

---

## 20. Usage telemetry implementation

### 20.1 Authoritative source

The only supported authoritative usage read is the stable app-server `account/rateLimits/read` method through the account's isolated runtime.

Windowkeeper never sends direct HTTP requests to the private usage endpoint used internally by Codex.

### 20.2 Full-read selection

When the response contains a map by limit ID:

1. Select key `codex` when present.
2. Otherwise use the app-server's backward-compatible main snapshot.
3. Preserve auxiliary limit summaries for diagnostics.
4. Record which limit ID was selected.

### 20.3 Notification handling

`account/rateLimits/updated` is treated as an invalidation signal:

1. Mark account usage dirty in memory.
2. Debounce for approximately two seconds.
3. Request one coalesced full read.
4. Do not persist the sparse notification as a complete usage snapshot.

This avoids incorrect field clearing or partial-state interpretation.

### 20.4 Window normalization

Input windows retain:

- Raw slot name.
- Raw used percentage.
- Duration minutes.
- Reset Unix timestamp.

Classification algorithm:

1. Detect weekly candidates whose duration is within a small documented tolerance of 10,080 minutes.
2. Exclude those candidates from short-window selection.
3. Among remaining positive durations below 1,440 minutes, select the shortest as `SHORT`.
4. Mark every other window `OTHER`.
5. When no unambiguous short window exists, activation scheduling confidence is `UNKNOWN`.

The exact tolerance is a tested domain constant, initially 5%, and is not used to rewrite raw values.

### 20.5 Percentage handling

- Persist `usedPercentRaw` exactly.
- Display `min(100, max(0, raw))` in progress meters.
- Add an anomaly indicator when raw falls outside 0–100.
- Never reinterpret the field as remaining percentage.

### 20.6 Freshness model

Suggested states:

```text
FRESH:      complete success within 2 polling intervals
AGING:      older than 2 intervals but within 30 minutes
STALE:      older than 30 minutes or latest refresh failed
UNKNOWN:    no successful complete read
```

The UI displays the exact last-success time and latest failed-attempt time. Threshold constants belong in one domain module and must be tested.

### 20.7 Polling planner

For each enabled account:

```text
base_due = last_attempt_at + 300 seconds
jitter = deterministic(account_id, polling_epoch, bounded range)
next_due = base_due + jitter
```

Deterministic jitter avoids synchronized bursts while remaining reproducible in tests.

### 20.8 Refresh coalescing

All triggers use one per-account single-flight mechanism:

- Background poll.
- Dashboard refresh.
- Manual API action.
- CLI action.
- Rate-limit notification.
- Startup verification.

If a complete snapshot is younger than a short freshness reuse threshold, an ordinary request returns it without an upstream call. A force request can schedule one follow-up after an in-flight read.

### 20.9 Successful refresh transaction

In one transaction:

1. Insert `usage_snapshots` success row.
2. Upsert `usage_current` with the complete normalized state.
3. Clear usage error fields.
4. Update account state and state version.
5. Resolve applicable usage/auth incidents only when the success is affirmative.
6. Recompute scheduling evidence or mark scheduler wakeup required.

After commit, publish state SSE events.

### 20.10 Failed refresh transaction

In one transaction:

1. Insert a failed `usage_snapshots` row.
2. Keep the prior successful values in `usage_current`.
3. Update `last_attempt_at`, stale flag, error code, and error summary.
4. Transition account state only according to failure policy.
5. Open or update an incident only after the durable threshold is crossed.

A single transient timeout does not immediately create an incident.

### 20.11 Retention

Keep all actual coalesced refresh attempts for 30 days. Cleanup deletes oldest rows in small ordered batches, commits between batches, yields to foreground work, and performs bounded incremental vacuum only when a free-page threshold is exceeded.

---

## 21. Scheduling implementation

### 21.1 Scheduling inputs

The scheduler consumes only committed domain state:

- Account enabled state.
- Current authentication state.
- Current normalized short-window evidence.
- Last complete usage-read time.
- Last successful activation time.
- Existing activation attempts and window keys.
- Any active ambiguous attempt.
- Configured safety delay and jitter range.
- Current UTC wall clock.

The scheduler does not infer state from a running process, an SSE event, or a browser request.

### 21.2 Confirmed schedule algorithm

A confirmed schedule can be created only when:

- The account is enabled.
- Authentication is not known invalid.
- A complete successful usage read exists.
- The selected short window is unambiguous.
- `resetsAt` is present and in the future at decision time.
- Duration is positive.
- No attempt already exists for the resulting window key.
- No unresolved ambiguous predecessor blocks the account.

Algorithm:

```text
window_key = "reported:" + resetsAt
jitter = SHA256(account_id + ":" + window_key) mod (max_jitter + 1)
run_at = resetsAt + safety_delay + jitter
confidence = CONFIRMED
source = REPORTED_RESET
```

Store the chosen jitter and all basis evidence in the activation attempt. Do not recalculate a different jitter after restart.

### 21.3 Estimated schedule algorithm

Estimated scheduling is permitted only when all conditions hold:

1. No usable authoritative future short-window reset exists.
2. At least two successful complete snapshots have consistently identified the same short-window duration, or another release-tested evidence rule has been satisfied.
3. A last successful activation timestamp exists.
4. The observed duration remains positive and below one day.
5. No unresolved ambiguous activation exists.
6. No attempt exists for the estimated window key.
7. Estimated scheduling is enabled.

Algorithm:

```text
window_key = "estimated:" + last_successful_activation_ms + ":" + observed_duration_minutes
run_at = last_successful_activation
         + observed_duration
         + safety_delay
         + deterministic_jitter
confidence = ESTIMATED
source = OBSERVED_DURATION_FALLBACK
```

A blind constant five-hour assumption is not permitted. Five hours may be the observed duration for a particular account, but it is not a universal product constant.

### 21.4 Unknown schedule

The account's next activation is `UNKNOWN` when:

- No complete usage read exists.
- Window classification is ambiguous.
- Reset time is missing and fallback evidence is insufficient.
- Authentication is unverified.
- A prior attempt is irreducibly ambiguous.
- Upstream data remains anomalous after a fresh read.

Unknown means monitoring continues but no activation is guessed.

### 21.5 Deadline scheduler

The scheduler stores no independent APScheduler jobs. It queries domain tables for the earliest deadline:

- Planned activation times.
- Usage poll deadlines.
- Operation retry times.
- Webhook delivery times.
- Login expirations.
- Incident threshold evaluations where needed.
- Maintenance deadlines.

The loop:

1. Query the earliest due time.
2. Compute monotonic sleep from current wall time.
3. Sleep through an interruptible event.
4. Wake early when state changes.
5. Requery rather than assuming the old deadline remains valid.
6. Claim due work through the appropriate service.

Wall-clock jumps cause a requery. In-process sleep uses monotonic time.

### 21.6 Pre-execution revalidation

Before activating, the actor performs a fresh decision transaction:

- Account still enabled.
- Attempt still active and due.
- No other attempt for the same window key.
- No unresolved predecessor.
- Authentication state permits work.
- Schedule has not been superseded by a newer reported reset.
- Current time is not earlier than the safety-adjusted deadline.
- Runtime compatibility is valid.

For an estimated schedule, request a fresh complete usage read immediately before submission. If it returns a newer authoritative reset or shows the account is already within a different window, cancel or reschedule the estimated attempt.

### 21.7 Downtime and catch-up

After restart:

- Reconcile unfinished activations before scheduling new ones.
- Do not replay every missed deadline.
- At most one catch-up activation may be considered.
- Catch-up requires current evidence that identifies a still-valid window key.
- If a newer window is already known, obsolete planned attempts are cancelled as stale.
- An ambiguous attempt from an older window is terminalized as `UNKNOWN_SKIPPED_AT_WINDOW_ROLLOVER`; it is not resent.

---

## 22. Activation execution

### 22.1 Fixed prompt and prompt identity

The v1 prompt is immutable in product configuration:

```text
Respond with exactly "OK" and perform no other actions.
```

The code defines:

- `prompt_version = 1`.
- Exact UTF-8 bytes.
- SHA-256 digest.

Database rows store version and digest. Logs never contain the full prompt, even though it is fixed.

### 22.2 Activation admission

Activation can originate from:

- Scheduled reset.
- Manual administrator request.
- Definite-failure retry.
- One restart catch-up decision.

All triggers converge into one admission function. The function:

1. Resolves or creates the applicable window key.
2. Checks account enabled/auth/safety state.
3. Checks the unique `(account_id, window_key)` constraint.
4. Checks unresolved attempts and current account operation.
5. Creates one logical activation attempt and one durable operation.
6. Commits before notifying the actor.

Manual activation does not bypass deduplication. When no valid window key can be derived, a manual diagnostic key uses an explicit operator-test namespace and cooldown, such as:

```text
manual:<UTC-date>:<operator-operation-id>
```

The UI clearly states that a manual activation consumes usage.

### 22.3 Activation runtime profile

For each logical activation:

- Start or reuse the owning account runtime.
- Create a newly emptied account-scoped workspace.
- Create a new persistent Codex thread.
- Use the exact pinned model and supported low reasoning effort selected during release qualification.
- Use read-only sandboxing.
- Use approval policy `never`.
- Disable web search, apps, MCP, plugins, skills, hooks, and capability roots.
- Configure no inherited project instructions.
- Provide an explicit shell environment allowlist of none/minimal values.
- Register handlers that reject all server-initiated tool and approval requests.

### 22.4 Persistent thread policy

Use one new persistent thread per logical activation.

Do not use:

- Ephemeral threads, because they cannot be reconciled after restart.
- One long-lived account thread, because prior context may affect the fixed prompt.
- Thread reuse for later windows.

Persist the thread ID immediately after successful thread creation.

### 22.5 Turn-submission sequence

1. Confirm the logical attempt is in `THREAD_CREATED` or the equivalent dispatch-ready state.
2. Insert a `SUBMIT` activation operation in `STARTED` state.
3. Transition attempt to `TURN_DISPATCHING` and commit.
4. Construct `clientUserMessageId = activation_id`.
5. Mark write-started timestamp immediately before transport write.
6. Send `turn/start` once.
7. Mark write-completed when the entire JSONL request line is written.
8. Await response within the turn-start deadline.
9. On successful response, persist turn ID and `TURN_ACCEPTED` immediately.
10. Continue consuming notifications until terminal completion or failure.

The first application-level acceptance proof is the successful `turn/start` response containing a turn ID.

### 22.6 Tool and approval event handling

Any activation-related item indicating:

- Command execution.
- File change.
- MCP call.
- App use.
- Web search.
- Dynamic tool request.
- User-input request.
- Approval request.

causes:

1. Immediate rejection or interruption where possible.
2. Attempt transition to `SAFETY_VIOLATION`.
3. Account automatic activation pause.
4. Durable incident opening.
5. No automatic retry.

A rejected request still counts as a safety violation because the requirement is not merely “no successful tool action”; the release profile must avoid tool requests entirely in protected tests.

### 22.7 Completion and result normalization

Terminal turn statuses are mapped to domain results.

For a completed textual response:

1. Extract only the final assistant text through a strict adapter.
2. Normalize Unicode to NFC.
3. Trim leading/trailing Unicode whitespace.
4. Accept exact case-sensitive `OK` as `COMPLETED_OK` unless product tests explicitly choose case-insensitive normalization.
5. Any other completed textual response becomes `COMPLETED_WARNING`.
6. Do not resend immediately after a warning.

Windowkeeper stores a small normalized classification and safe length/hash metadata, not the full response body.

### 22.8 Successful activation transaction

In one transaction:

- Finalize activation operation.
- Set attempt `COMPLETED_OK` or `COMPLETED_WARNING`.
- Store terminal timestamps and safe result classification.
- Update current account state.
- Update last-successful-activation basis only according to defined policy.
- Resolve activation-related incidents on affirmative success.
- Wake scheduler for next decision.

Publish SSE after commit.

### 22.9 Warning behavior

A completed non-`OK` response:

- Counts as an accepted and completed activation.
- Does not cause a second turn in the same window.
- Sets account overall state to warning if otherwise healthy.
- Records response length and digest, not full text.
- May open an incident only if product policy classifies persistent warning as action required. The default v1 behavior is warning without webhook incident.

---

## 23. Failure classification and retry policy

### 23.1 Definitely not accepted

A submission can be retried only when evidence proves no turn was accepted. Examples:

- Configuration or validation failure before transport invocation.
- Database failure before the submission marker commits.
- App-server spawn or initialization failure.
- Thread creation failure before a thread exists.
- Explicit protocol rejection of `turn/start` before turn creation.
- Ingress overload response documented as pre-acceptance.
- Transport implementation proves zero request bytes were written.

### 23.2 Ambiguous

Examples:

- Timeout after any request bytes may have been written.
- EOF after write but before response.
- App-server crash after write.
- Windowkeeper crash during submission.
- Malformed response after request write.
- Lost response with no turn ID.
- Database failure after upstream acceptance but before local turn-ID commit.

Ambiguous state never schedules a blind resend.

### 23.3 Accepted but incomplete

When turn ID is known but no terminal completion was received:

- Attempt is `TURN_ACCEPTED` or `RUNNING`.
- Restart reconciliation reads the thread and turn.
- No second submission is permitted.

### 23.4 Retry schedule for definite failures

Suggested bounded retry policy within the same still-valid window:

```text
attempt 1: immediate original submission
retry 1:  15 seconds + deterministic jitter
retry 2:  60 seconds + deterministic jitter
retry 3:  5 minutes + deterministic jitter
```

Retry stops when:

- The window key is obsolete.
- A newer authoritative reset supersedes the attempt.
- Authentication becomes invalid.
- Safety violation occurs.
- Configured maximum attempts are reached.
- Failure becomes ambiguous.

The exact retry count and intervals are release-tested constants.

### 23.5 Authentication failures

Authentication or workspace failures:

- Transition account to `AUTH_REQUIRED` or `CONFIG_BLOCKED`.
- Cancel future automatic activation plans.
- Open one deduplicated incident.
- Require operator action.
- Do not consume generic activation retries.

### 23.6 Rate-limit and overload failures

- App-server ingress overload known to be pre-acceptance may retry with bounded exponential delay.
- Upstream account limit reached after turn acceptance is not replayed.
- Usage-read rate limiting preserves cached state and schedules a later refresh.
- Retry headers or documented delays are honored where available.

---

## 24. Activation reconciliation

### 24.1 Reconciliation priority

Startup and runtime recovery prioritize attempts in:

```text
TURN_DISPATCHING
TURN_ACCEPTED
RUNNING
AMBIGUOUS
RECONCILING
```

No new activation for the account is admitted until these are resolved or terminalized.

### 24.2 Reconciliation prerequisites

- Same account credential lineage.
- Same exact or explicitly compatible Codex release.
- Persistent thread ID when known.
- Activation UUID used as `clientUserMessageId`.
- Valid account authentication.

### 24.3 Known thread and turn

When thread ID and turn ID are known:

1. Call thread read with turns included.
2. Locate exact turn ID.
3. If terminal, adopt terminal result.
4. If in progress, subscribe/resume only when needed and supported.
5. If interrupted or failed, classify according to upstream status without resubmitting.
6. If missing, repeat within a bounded materialization grace before escalating to ambiguity.

### 24.4 Known thread, unknown turn

1. Read the persistent thread with turns.
2. Search user-message items for `clientId == activation_id`.
3. If exactly one match exists, bind its turn ID.
4. If more than one exists, mark duplicate-safety violation and pause account.
5. If no match exists, retry reads through a bounded materialization grace.
6. Only conclude definite non-acceptance if protected testing establishes a reliable evidence pattern for the pinned version.
7. Otherwise remain ambiguous.

### 24.5 Unknown thread

A failure before thread ID persistence requires examining transport and database phase:

- If thread creation was never sent or explicitly failed pre-creation, retry thread creation.
- If thread creation may have succeeded but ID is unknown, there is no safe general thread enumeration/correlation guarantee in the current plan. The activation becomes ambiguous and is skipped unless the pinned protocol provides a tested correlation path.

This is why thread ID is persisted immediately before turn submission.

### 24.6 Materialization grace

The grace is a bounded sequence of reads, for example:

```text
0 seconds
1 second
3 seconds
10 seconds
30 seconds
```

The exact sequence is determined by Phase 0 experiments. It cannot extend beyond the relevant window without operator-visible state.

### 24.7 Window rollover

When a newer confirmed window key is observed and an older ambiguous attempt remains unresolved:

- Mark old attempt `UNKNOWN_SKIPPED_AT_WINDOW_ROLLOVER`.
- Preserve all ambiguity evidence.
- Resolve or reclassify the incident according to whether operator action remains useful.
- Permit planning for the new window only after confirming the old attempt cannot represent the same window key.

### 24.8 Reconciliation acceptance criteria

The release must prove through a fake server with an external submission witness that every crash point produces one of:

- Safe retry because zero acceptance occurred.
- Adoption of the one accepted turn.
- Fail-closed ambiguity with no second accepted turn.

No test may hide duplicate submissions by relying only on Windowkeeper's own database.

---

## 25. Account and service status model

### 25.1 Independent status dimensions

#### Authentication

```text
UNCONFIGURED
ENROLLING
VERIFIED
AUTH_REQUIRED
WORKSPACE_MISMATCH
CREDENTIAL_ERROR
```

#### Runtime worker

```text
STOPPED
STARTING
READY
BUSY
STOPPING
CRASHED
COMPATIBILITY_BLOCKED
```

#### Usage freshness

```text
UNKNOWN
FRESH
AGING
STALE
ERROR
```

#### Activation

```text
UNSCHEDULED
CONFIRMED_SCHEDULE
ESTIMATED_SCHEDULE
DUE
RUNNING
WARNING
AMBIGUOUS
SAFETY_BLOCKED
```

### 25.2 Overall-state derivation

Overall state is derived through an explicit priority table:

1. `DISABLED` when account is disabled.
2. `ACTION_REQUIRED` for auth required, workspace mismatch, safety violation, or operator-resolvable compatibility issue.
3. `ERROR` for durable runtime/database/account failures not requiring credential input.
4. `STARTING` during startup verification with no stronger state.
5. `WARNING` for stale usage, estimated scheduling, or non-`OK` completed activation.
6. `HEALTHY` only with verified auth, sufficiently fresh usage, no open error/action incident, and no ambiguous activation.

### 25.3 Presentation requirements

- Every state has a text label.
- Color is supplementary.
- Icons include accessible text.
- Dynamic status changes use appropriate live-region semantics without moving focus.
- Exact timestamps and evidence are available in account detail.

---

## 26. Incident lifecycle

### 26.1 Incident key

```text
(scope_kind, scope_key, problem_type)
```

Examples:

```text
(account, <account_uuid>, authentication_required)
(account, <account_uuid>, usage_refresh_failed)
(account, <account_uuid>, activation_ambiguous)
(account, <account_uuid>, activation_safety_violation)
(service, windowkeeper, database_unavailable)
(service, windowkeeper, codex_incompatible)
(service, windowkeeper, webhook_dispatch_degraded)
```

### 26.2 Opening threshold

Only durable transitions to `ACTION_REQUIRED` or `ERROR` open incidents. Warning-only and transient conditions do not.

Examples:

- One usage timeout: no incident.
- Repeated usage failures crossing stale/error threshold: incident.
- Auth rejection: immediate incident.
- Ambiguous activation: immediate incident.
- Completed non-`OK`: warning, no default incident.

### 26.3 Repeated observations

Repeated evidence updates:

- `last_seen_at`.
- `occurrence_count`.
- Summary.
- Current error code.

It does not enqueue another opened event.

### 26.4 Resolution

A fresh affirmative success from the owning subsystem resolves the incident:

- Successful account verification resolves auth incident.
- Successful full usage read resolves usage incident.
- Reconciled terminal activation resolves ambiguity incident when no operator action remains.

Disablement or deletion closes incidents administratively with a `CLOSED` message rather than `RECOVERED`.

### 26.5 Canonical event types

```text
com.windowkeeper.incident.opened.v1
com.windowkeeper.incident.resolved.v1
com.windowkeeper.webhook.test.v1
```

Incident mutation and event/outbox creation occur in the same transaction.

---

## 27. Webhook subsystem

### 27.1 Destination types

- Generic JSON.
- Slack incoming webhook.
- Discord webhook.

No arbitrary custom headers, templates, routing expressions, or escalation chains.

### 27.2 Secret storage

The entire destination URL is encrypted because Slack and Discord URLs contain bearer secrets. Optional generic signing secrets are encrypted separately. The UI displays only a safe hostname/path summary or a fixed masked label.

### 27.3 URL validation

Accept only `http` and `https`.

Reject:

- URL userinfo.
- Fragments.
- Unsupported schemes.
- Empty or malformed host.
- Unspecified, multicast, or link-local destinations.
- Redirect responses.

Private, loopback, and LAN destinations are allowed as a deliberate self-hosted use case. Documentation states the SSRF tradeoff and recommends network segmentation.

### 27.4 HTTP client

Use a dedicated `httpx.AsyncClient` configured with:

- Redirects disabled.
- TLS certificate verification enabled.
- `trust_env=False`.
- No cookies.
- No `.netrc` inheritance.
- Explicit connect/read/write/pool timeouts.
- Bounded connection pool.
- No user-configurable headers.

This client is separate from any trusted upstream client.

### 27.5 Canonical generic envelope

```json
{
  "specversion": "1.0",
  "id": "<stable-event-id>",
  "source": "urn:windowkeeper:<instance-uuid>",
  "type": "com.windowkeeper.incident.opened.v1",
  "subject": "account/<public-identity-token>",
  "time": "<UTC timestamp>",
  "datacontenttype": "application/json",
  "data": {
    "status": "OPEN",
    "severity": "ERROR",
    "problem_type": "activation_ambiguous",
    "summary": "Activation outcome could not be proven",
    "account_display_name": "Account A",
    "occurrence_count": 1
  }
}
```

No local filesystem path, email address by default, credential data, raw error body, or internal database ID beyond safe public tokens is included.

### 27.6 Generic signature

Header:

```text
X-Windowkeeper-Signature: t=<unix-seconds>,v1=<hex-hmac-sha256>
```

Signed bytes:

```text
<timestamp>.<exact-request-body>
```

Use constant-time comparison in receiver examples. Event body bytes remain immutable across retries.

### 27.7 Slack adapter

- JSON body with `text` only.
- Conservative maximum 1,800 characters.
- No attempt to override channel, username, or icon.
- Stable event ID included in text.
- Honor `429 Retry-After`.

### 27.8 Discord adapter

- JSON body with `content` and `allowed_mentions` disabling mentions.
- Conservative maximum 1,800 characters.
- Request confirmation mode as supported by the endpoint.
- Treat missing webhook response as permanent according to provider semantics.

### 27.9 Retry policy

Attempts:

```text
1. Immediate
2. +1 minute
3. +5 minutes
4. +30 minutes
5. +2 hours
```

Classification:

- 2xx: delivered.
- 429: retry using `Retry-After`, bounded by policy.
- 5xx and transport timeout: retry.
- Most malformed/auth/not-found 4xx: permanent failure.
- Redirect: permanent failure.

### 27.10 At-least-once semantics

A remote endpoint can accept the request immediately before Windowkeeper crashes. A retry after restart may duplicate delivery. Stable event and delivery IDs allow receiver deduplication. Windowkeeper does not claim exactly-once webhooks.

### 27.11 Delivery claiming

1. Claim due row with a short database lease.
2. Decrypt destination secrets only in dispatcher memory.
3. Send outside the database transaction.
4. Read at most a bounded response body.
5. Redact and truncate the response excerpt.
6. Finalize conditionally using the lease token.

---

## 28. Administrator authentication and browser security

### 28.1 Password setup

First startup requires an administrator password through a mounted secret file or an explicit initialization command. The password is not persisted in Compose files by default.

Password policy:

- Minimum 15 characters.
- Accept at least 128 characters.
- No arbitrary composition rule.
- Reject a local list of common/compromised choices.
- Normalize and encode consistently.

### 28.2 Argon2id profile

Initial explicit profile:

```text
memory:      64 MiB
iterations:  3
parallelism: 1
salt:        16 random bytes
hash:        32 bytes
```

Benchmark on minimum supported hardware. Serialize expensive verification to one concurrent operation and perform cheap throttling before hashing.

### 28.3 Session tokens

- Generate 32 random bytes.
- Send only as an opaque cookie value.
- Store SHA-256 hash in SQLite.
- Rotate on login.
- Revoke on logout and password reset.
- Idle expiry: 15 minutes.
- Absolute expiry: 8 hours.
- Rate-limit last-seen database updates.

Cookie defaults:

```text
HttpOnly
SameSite=Lax
Path=<root_path or />
Secure when HTTPS is configured/detected through a trusted proxy
```

### 28.4 CSRF

Each pre-authenticated or authenticated session receives a separate random CSRF token. Store its hash server-side.

Every state-changing request requires:

- Form field or `X-CSRF-Token` value.
- Constant-time token validation.
- Same-origin `Origin` validation when present.
- `Referer` fallback validation where appropriate.
- Non-GET method.

### 28.5 Recent reauthentication

Require a password verification within five minutes for:

- Account deletion.
- Credential replacement.
- Reauthentication promotion.
- Vault rotation.
- Administrator password change.
- Webhook destination creation or secret replacement.
- Log download when future policy marks it sensitive.

### 28.6 Login throttling

Use both:

- Cheap in-memory/IP-based rate limiting before Argon2.
- Persisted progressive throttle state keyed by a privacy-preserving hash.

Return a generic failure message regardless of whether the password or throttle caused rejection.

### 28.7 Proxy trust

Forwarded scheme, host, and client IP are accepted only when the immediate peer matches an explicit trusted address/CIDR/Unix-socket configuration. Wildcard trust is forbidden. The reverse proxy must overwrite client-provided forwarding headers.

### 28.8 Security headers

Recommended headers:

```text
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Permissions-Policy: camera=(), microphone=(), geolocation=()
```

Use HSTS only in documented HTTPS deployments.

---

## 29. Web dashboard implementation

### 29.1 Technology choices

- FastAPI.
- Jinja2.
- Locally packaged CSS.
- Small native ES modules.
- Native EventSource.
- No HTMX in v1.
- No client-side router.
- No Node build step.

### 29.2 View-model boundary

Backend services return stable Pydantic view models, such as:

```text
DashboardView
AccountSummaryView
AccountDetailView
UsageWindowView
ScheduleView
OperationView
IncidentView
LoginProgressView
LogEventView
```

The same resource shape is used for:

- Initial server rendering.
- JSON API responses.
- SSE update payloads.

Templates never receive repository rows or Codex protocol objects.

### 29.3 Initial page load

1. Authenticate session.
2. Read cached state from SQLite.
3. Render HTML immediately.
4. Do not wait for upstream Codex.
5. Native JavaScript opens state SSE.
6. JavaScript submits a CSRF-protected refresh request for visible accounts.
7. Server coalesces the request with all other refresh triggers.
8. Committed updates arrive through SSE.

When JavaScript is unavailable, cached HTML, normal navigation, and form actions remain usable.

### 29.4 Dashboard routes

Suggested HTML routes:

```text
GET  /login
POST /login
POST /logout
GET  /
GET  /accounts/new
GET  /accounts/<public-token>
GET  /operations/<operation-id>
GET  /incidents
GET  /logs
GET  /settings
```

No state change occurs on GET.

### 29.5 Dashboard summary

Desktop table columns:

- Account/display name.
- Labels.
- Overall status.
- Authentication state.
- Short usage percentage.
- Short reset time.
- Weekly usage percentage.
- Weekly reset time.
- Schedule confidence.
- Next activation.
- Last successful refresh.
- Active operation.
- Actions menu.

Mobile cards render the same semantic fields in a separate template optimized for narrow screens rather than transforming table markup with inaccessible CSS tricks.

### 29.6 Account detail

Sections:

- Identity and workspace evidence.
- Authentication lifecycle.
- Current short and weekly windows.
- Freshness and source evidence.
- Next activation basis.
- Last activation result.
- Active and recent operations.
- Open and recent incidents.
- Recent usage snapshots.
- Safe account-specific logs filter.
- Administrative actions.

### 29.7 Account actions

- Refresh usage.
- Manual activate with confirmation and usage warning.
- Retry only when domain state says retryable.
- Enable/disable.
- Reauthenticate.
- Replace credential.
- Delete with typed display-name confirmation and recent password verification.

Controls render disabled with an explanatory reason when preconditions fail.

### 29.8 Login progress UI

Always display:

- Verification URI as text/link.
- User code as selectable text.
- Expiry countdown.
- Current operation status.
- Cancel control.

A QR code or complete URI may be an enhancement only if generated locally and accessible; it never replaces the text code.

### 29.9 Filters

Dashboard filters:

- Label.
- Overall state.
- Authentication mode.
- Schedule confidence.
- Enabled/disabled.
- Text match on display name.

Filter state may be represented in query parameters because it is non-secret and read-only.

### 29.10 Accessibility

Target WCAG 2.2 AA.

- Semantic headings and landmarks.
- Proper table headers.
- Visible focus indicators.
- Keyboard-operable actions.
- Minimum target sizes.
- Text and icons in addition to color.
- `aria-live` for relevant asynchronous status changes.
- No focus theft during SSE updates.
- Reduced-motion preference honored.
- All timestamps include accessible absolute values even when relative time is displayed.

### 29.11 Layout experimentation

A startup/development setting selects a Jinja layout variant. Variants share:

- Same routes.
- Same view models.
- Same semantic data attributes.
- Same JS modules.
- Same accessibility and action contracts.

No layout selection changes service behavior.

---

## 30. Private HTTP API

### 30.1 Prefix and versioning

```text
/api/internal/v1
```

The API is private to the bundled dashboard. Production OpenAPI and interactive documentation are disabled.

### 30.2 Resource routes

Suggested read routes:

```text
GET /api/internal/v1/dashboard
GET /api/internal/v1/accounts
GET /api/internal/v1/accounts/<public-token>
GET /api/internal/v1/operations/<operation-id>
GET /api/internal/v1/incidents
GET /api/internal/v1/logs
```

Suggested action routes:

```text
POST /api/internal/v1/accounts
POST /api/internal/v1/accounts/<public-token>/login/device
POST /api/internal/v1/accounts/<public-token>/login/cancel
POST /api/internal/v1/accounts/<public-token>/refresh
POST /api/internal/v1/accounts/<public-token>/activate
POST /api/internal/v1/accounts/<public-token>/enable
POST /api/internal/v1/accounts/<public-token>/disable
POST /api/internal/v1/accounts/<public-token>/reauthenticate
DELETE /api/internal/v1/accounts/<public-token>
POST /api/internal/v1/webhooks
POST /api/internal/v1/webhooks/<id>/test
POST /api/internal/v1/operations/<id>/cancel
```

### 30.3 Long-running responses

Actions accepted for asynchronous execution return:

```http
HTTP/1.1 202 Accepted
Location: /api/internal/v1/operations/<operation-id>
Content-Type: application/json
```

Body:

```json
{
  "api_version": "windowkeeper.dev/http/v1",
  "kind": "Operation",
  "data": {
    "operation_id": "...",
    "state": "QUEUED",
    "poll_url": "/api/internal/v1/operations/..."
  }
}
```

### 30.4 Problem details

Use `application/problem+json` with stable type URNs:

```json
{
  "type": "urn:windowkeeper:problem:activation-ambiguous",
  "title": "Activation outcome is ambiguous",
  "status": 409,
  "detail": "Windowkeeper cannot safely submit another activation for this window.",
  "instance": "/api/internal/v1/operations/...",
  "code": "ACTIVATION_AMBIGUOUS",
  "correlation_id": "..."
}
```

Details are redacted and do not include raw upstream messages.

### 30.5 Concurrency control

State-changing forms may include expected `state_version`. A stale version returns `409 Conflict` so the UI does not overwrite a newer account state.

---

## 31. State SSE implementation

### 31.1 Stream

```text
GET /api/internal/v1/events/state
Content-Type: text/event-stream
```

Session authentication is required.

### 31.2 Event envelope

```json
{
  "api_version": "windowkeeper.dev/state-event/v1",
  "resource_type": "account",
  "resource_id": "<public-token>",
  "resource_version": 42,
  "occurred_at": "...",
  "data": {}
}
```

SSE fields:

```text
id: <monotonic-in-process-event-id>
event: account.updated
data: <JSON>
retry: 3000
```

### 31.3 Event types

```text
snapshot
account.updated
operation.updated
login.updated
incident.updated
queue.updated
service.updated
gap
```

### 31.4 Replay

- Keep an in-memory bounded replay ring, initially 2,000 events or 15 minutes.
- Honor `Last-Event-ID` when the ID remains in the ring.
- If the ring cannot satisfy the request or the process restarted, send a complete `snapshot` event.
- Database state remains canonical.

### 31.5 Backpressure

Each client receives a bounded queue. A slow client:

1. Does not block event publication.
2. Receives a `gap` signal when entries are dropped.
3. Is disconnected after repeated overflow.
4. Reconnects and obtains a snapshot.

### 31.6 Heartbeat

Send comment heartbeat frames approximately every 15 seconds to keep intermediaries and disconnect detection active.

### 31.7 Polling fallback

If EventSource repeatedly fails, client JavaScript polls the dashboard resource with bounded exponential backoff and stops when the page is hidden for a prolonged interval. Forms and navigation remain functional without live transport.

---

## 32. CLI implementation

### 32.1 Framework

Use Click 8.x, pinned to a compatible major range. The CLI entry point is `windowkeeper`.

Primary Docker invocation:

```bash
docker compose exec windowkeeper windowkeeper <command>
```

Scripts use `docker compose exec -T`.

### 32.2 Command tree

```text
windowkeeper
├── serve
├── version [--json]
├── health [--json]
├── diagnostics
│   ├── run
│   ├── database
│   ├── vault
│   ├── codex
│   └── redact-test
├── account
│   ├── list
│   ├── show
│   ├── add-device
│   ├── add-token
│   ├── enable
│   ├── disable
│   ├── reauthenticate
│   ├── replace-token
│   ├── logout
│   └── delete
├── label
│   ├── list
│   ├── create
│   ├── delete
│   ├── attach
│   └── detach
├── usage
│   ├── show
│   └── refresh
├── activation
│   ├── list
│   ├── show
│   ├── run
│   └── retry
├── operation
│   ├── list
│   ├── get
│   ├── wait
│   └── cancel
├── webhook
│   ├── list
│   ├── add
│   ├── disable
│   ├── delete
│   └── test
├── logs
│   ├── show
│   ├── follow
│   └── export
├── vault
│   ├── generate-key
│   ├── verify
│   ├── rotate-prepare
│   └── rotate-promote
└── admin
    ├── password-set
    ├── password-reset
    └── sessions-revoke
```

Conditional commands such as `account add-token` are hidden or return a clear unsupported result when the release manifest does not enable them.

### 32.3 Service ownership

The CLI does not start a second runtime manager. Runtime-dependent commands:

1. Open the same configuration and database safely.
2. Verify the primary service lease/heartbeat as designed.
3. Insert a durable operation.
4. Signal or rely on the running service to claim it.
5. Optionally wait by polling the operation row.

If the primary service is not running, runtime-dependent commands fail. Read-only diagnostics and carefully scoped offline maintenance commands remain available under explicit rules.

### 32.4 Human and JSON output

Human output uses plain tables and messages without a Rich dependency.

`--json` envelope:

```json
{
  "api_version": "windowkeeper.dev/cli/v1",
  "kind": "AccountList",
  "generated_at": "...",
  "data": []
}
```

Streaming commands use JSON Lines with one independently versioned object per line.

### 32.5 Exit codes

Suggested stable mapping:

```text
0  success
1  general operation failure
2  command usage/validation error
3  authentication or authorization required
4  conflict or unsafe state
5  primary service unavailable
6  compatibility blocked
7  timeout
8  partial result
```

### 32.6 Secret entry

Passwords, tokens, and new vault keys enter through:

- Hidden prompt.
- stdin.
- Protected file.

Never through a command argument or ordinary environment option displayed by process listings.

---

## 33. Structured logging and diagnostics

### 33.1 Logging stack

- `structlog` for event construction and context binding.
- Standard-library logging for ecosystem integration.
- Custom sanitizing bounded queue handler.
- One fan-out writer thread.
- Compact UTF-8 JSONL serialization.

### 33.2 Producer pipeline

Before enqueue:

1. Normalize event name and level.
2. Attach UTC timestamp.
3. Attach service version, correlation ID, account ID, operation ID, activation ID, and runtime generation where applicable.
4. Convert exceptions to safe type/code/stack metadata.
5. Recursively redact secret-bearing keys.
6. Replace exact known secret values and safe encoded variants.
7. Scrub high-confidence token patterns and sensitive URL query values.
8. Remove CR/LF injection from untrusted scalar fields.
9. Truncate fields and total event size.
10. Run final canary/no-secret checks.
11. Enqueue without blocking.

### 33.3 Event schema

```json
{
  "schema": "windowkeeper.log/v1",
  "ts": "...",
  "level": "INFO",
  "event": "usage.refresh.completed",
  "message": "Usage refresh completed",
  "correlation_id": "...",
  "account_id": "...",
  "operation_id": "...",
  "duration_ms": 123,
  "result": "success"
}
```

### 33.4 Forbidden log content

Never log:

- Raw JSON-RPC payloads.
- Full prompts or responses.
- Authorization/device URLs with secrets.
- Device codes.
- Headers.
- Environment dumps.
- Credential-file contents.
- Session or CSRF values.
- Vault keys.
- Webhook URLs or signing secrets.

### 33.5 Rotation and retention

- One global active JSONL file.
- Rotate at 25 MiB or UTC day boundary.
- Retain 30 days.
- Enforce 1 GiB hard cap.
- Repair an incomplete final line after crash by truncating to the last newline.
- One writer thread is the only file appender.

### 33.6 Queue overflow

The producer queue is bounded. On overflow:

- Do not block critical event-loop work.
- Increment an in-memory and periodically persisted drop counter.
- Prefer dropping lower-severity repetitive logs before warning/error events.
- Emit a synthesized overflow event when capacity returns.
- Never recursively log the overflow through the same failing path.

### 33.7 Browser log viewer

Features:

- Newest-first pagination.
- Filters by time, level, event, account, operation, and correlation ID.
- Literal substring search over bounded files/time.
- Row expansion.
- Copy sanitized event.
- Download sanitized filtered JSONL.
- Live SSE tail.

No persistent full-text index in v1.

### 33.8 Cursor design

Historical cursors contain a signed opaque payload with:

- Server-assigned file token.
- Byte offset.
- Sort direction.
- Filter hash.
- Expiry.

Never accept arbitrary filesystem paths from clients.

### 33.9 Live log SSE

- Global sanitized ring: 2,000 events.
- Per-client queue: 256 events.
- Slow-client overflow drops oldest events and sends a gap with a historical resume cursor.
- Repeated overflow disconnects the client.

### 33.10 DEBUG and TRACE

- INFO default.
- Temporary DEBUG maximum: 24 hours.
- Temporary TRACE maximum: 1 hour.
- Overrides persist in SQLite with expiration so restart cannot make them permanent.
- TRACE includes lifecycle metadata only, not raw content.

### 33.11 Diagnostic bundle

A downloadable diagnostic bundle may include only:

- Windowkeeper and Codex versions/digests.
- Redacted configuration summary.
- Database schema version and integrity results.
- Account counts by state, not credentials.
- Recent sanitized logs within an explicit bounded range.
- Scheduler and runtime summaries.

It excludes database files, credential ciphertext unless explicitly required for support, session rows, webhook URLs, and raw Codex homes.

---

## 34. Docker image and deployment

### 34.1 Image design

- Multi-stage Dockerfile.
- Digest-pinned Python slim Debian base.
- Python application dependencies built as wheels in builder stage.
- Exact architecture-specific Codex standalone package.
- No Node.js or npm runtime.
- Fixed unprivileged UID/GID 10001.
- Exec-form entry point.

### 34.2 Filesystem

Persistent:

```text
/data
├── windowkeeper.db
├── windowkeeper.db-wal
├── windowkeeper.db-shm
├── windowkeeper.lock
├── migration-backups/
└── logs/
```

Ephemeral writable:

```text
/run/windowkeeper   tmpfs
/tmp                tmpfs
```

Root filesystem is read-only by default.

### 34.3 Compose hardening

```yaml
services:
  windowkeeper:
    image: ghcr.io/<owner>/windowkeeper:<version>
    user: "10001:10001"
    read_only: true
    init: true
    restart: unless-stopped
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /run/windowkeeper:uid=10001,gid=10001,mode=0700
      - /tmp:uid=10001,gid=10001,mode=0700
    volumes:
      - windowkeeper-data:/data
    secrets:
      - windowkeeper_vault_key
      - windowkeeper_admin_password
    stop_grace_period: 45s
```

### 34.4 Direct-LAN deployment

- Publish `8787:8787`.
- Do not trust forwarded headers.
- Recommend firewall restriction to the trusted LAN.
- Administrator password remains required.

### 34.5 Reverse-proxy-only deployment

- Publish no host port.
- Expose 8787 only on a private Compose network.
- Configure explicit proxy IP or subnet.
- Configure root path and external base URL where needed.
- Proxy must disable response buffering for SSE and overwrite forwarded headers.

### 34.6 Healthcheck

Built-in dependency-free command:

```bash
windowkeeper health --url http://127.0.0.1:8787/health/ready
```

The health command does not require curl in the image.

### 34.7 Resource limits

Documentation provides conservative examples but does not hard-code limits that break NAS deployments. Phase 0 and load testing establish recommended memory for 1, 10, and 25 accounts.

### 34.8 Release metadata

OCI labels include:

- Source repository.
- Windowkeeper version.
- Git revision.
- License.
- Build timestamp.
- Bundled Codex version.
- Codex digest.
- Stable schema digest.

### 34.9 Supply-chain controls

Release pipeline:

1. Pin GitHub Actions to full commit SHAs.
2. Build and test architecture images.
3. Generate SPDX SBOM.
4. Generate maximum BuildKit/SLSA provenance.
5. Scan for fixable high/critical vulnerabilities.
6. Generate GitHub artifact attestation.
7. Push a multi-platform manifest.
8. Sign image digest with keyless Cosign.
9. Verify signatures and attestations in release documentation.

---

## 35. Testing architecture

### 35.1 Test pyramid

1. Pure domain unit tests.
2. Property/state-machine tests.
3. Repository and migration tests.
4. Schema-validated fake app-server contract tests.
5. Crash and subprocess tests.
6. FastAPI/HTTPX integration tests.
7. One focused Chromium Playwright smoke suite.
8. Container and architecture tests.
9. Protected real-account tests.

### 35.2 Injected time and randomness

A `Clock` interface exposes:

- UTC wall time.
- Monotonic time.
- Cancellable sleep.

A separate jitter source is seeded in tests. Tests independently move wall time forward/backward and monotonic time forward, allowing immediate simulation of multi-hour and weekly windows.

### 35.3 Fake app-server

The fake is a real subprocess speaking stdio JSONL. It:

- Enforces initialize/initialized ordering.
- Validates incoming and outgoing messages against the committed Codex schema.
- Implements only consumed stable methods.
- Supports scripted device login, account read, rate-limit reads, thread start/read, turn start, notifications, and completion.
- Emits sparse updates.
- Supports delays, malformed messages, overload, disconnects, EOF, and crashes.
- Persists a separate fsynced witness journal recording accepted turn submissions.

The witness journal, not Windowkeeper's database, proves duplicate behavior.

### 35.4 Unit tests

Cover:

- Window classification independent of slot name.
- Raw percentage anomalies.
- Freshness transitions.
- Deterministic jitter.
- Confirmed/estimated/unknown scheduling.
- One attempt per window key.
- Retry admissibility.
- Result normalization.
- Overall-status derivation.
- Incident deduplication.
- Webhook signature bytes.
- Redaction processors.
- Session expiry.

### 35.5 Property-based state machines

Activation model actions:

```text
plan
queue
create_thread
begin_write
accept_turn
disconnect
crash_windowkeeper
crash_app_server
restart
read_thread
complete_turn
roll_window
disable_account
retry
```

Invariants after every action:

- No more than one accepted turn per `(account, window_key)`.
- No retry while ambiguous.
- Persisted phase never moves backward illegally.
- Account isolation remains intact.

Additional state machines cover:

- Scheduling under clock jumps.
- Sparse usage invalidation and full reads.
- Incident open/update/resolve cycles.
- Vault rotation crash points.
- Webhook lease and retry behavior.

### 35.6 Crash tests

Run Windowkeeper in a child process using a temporary data directory. Named failpoints terminate through `os._exit()` or an external `SIGKILL` at:

- Before attempt insert commit.
- After attempt commit.
- Before thread-start write.
- After thread-start write.
- After thread ID received, before persistence.
- After thread ID persistence.
- Before turn-start write.
- During turn-start write.
- After write, before response.
- After acceptance, before turn ID commit.
- After turn ID commit.
- During terminal-result transaction.
- During credential checkpoint.
- During vault rotation prepare/promote.
- During incident/outbox transaction.
- After webhook remote success, before local finalize.

After restart, assert database integrity and domain invariants.

### 35.7 Database tests

- Every migration from an empty database.
- Sequential upgrade from each historical fixture.
- Migration rollback restoration.
- Newer-schema refusal with no write.
- Foreign-key enforcement on every connection.
- Partial unique-index enforcement.
- WAL crash recovery.
- Retention batching.
- Incremental vacuum thresholds.

### 35.8 Web tests

FastAPI/HTTPX tests cover:

- Login, logout, session rotation, expiry.
- CSRF token failures and origin validation.
- Trusted and untrusted proxy behavior.
- No state-changing GET routes.
- `202` operation semantics.
- Problem Details format.
- SSE snapshot, replay, heartbeat, gap, and disconnect.
- Root-path URL generation.
- XSS-safe log rendering.
- Disabled OpenAPI/docs.

### 35.9 Browser smoke suite

One Chromium suite covers:

1. Login.
2. Three-account dashboard.
3. Overlapping labels.
4. Desktop table and mobile viewport cards.
5. Account detail.
6. Refresh operation.
7. Manual activation confirmation.
8. SSE update.
9. Device login progress fixture.
10. Log viewer filtering/live tail.
11. Destructive confirmation.
12. Logout.

No broad visual-regression or cross-browser matrix in v1.

### 35.10 Container tests

For both amd64 and arm64:

- Image starts as UID 10001.
- Read-only root works.
- Required paths are writable and only those paths.
- Codex app-server initializes.
- Healthcheck works.
- Signals reach Windowkeeper and children.
- Shutdown leaves no orphan process.
- Restart preserves database and encrypted credentials.
- Incorrect vault key fails safely.
- Second replica is refused.

### 35.11 Load and soak tests

Blocking release scales:

- 1 account.
- 10 accounts.
- 25 accounts.

Measure:

- Memory and file descriptors.
- Event-loop responsiveness.
- Process startup concurrency.
- Refresh completion distribution.
- SQLite transaction latency.
- SSE clients.
- Log queue drops.
- Webhook backlog.
- Shutdown time.

A 100-account test is non-blocking characterization only and must show bounded degradation rather than supported capacity.

### 35.12 Protected real-account tests

Use disposable/authorized accounts and strict usage controls. Validate:

- Two concurrent device-code logins.
- Independent completion and cancellation.
- Account read and rate-limit attribution.
- Credential checkpoint/delete/restore/restart.
- Logout isolation.
- Optional access-token path.
- One fixed activation per account.
- Persistent thread read after restart.
- No tool items.
- Exact result classification.

Do not repeatedly activate, perform load testing, exhaust quotas, or wait for real reset windows. Reset scheduling is tested with fake time and fake server evidence.

---

## 36. Release gates

### Gate A: Codex integration

- Exact binary and schema verified.
- Stable protocol contract tests pass.
- Device-code login works in release container.
- Two accounts remain isolated.
- Rate-limit reads are correctly attributed.

### Gate B: Activation safety

- No protected activation produces a tool or approval item.
- Crash matrix proves zero duplicate accepted turns.
- Ambiguous state always fails closed.
- Persistent thread reconciliation works where evidence exists.

### Gate C: Credential security

- No persistent plaintext credentials.
- Correct `0700`/`0600` permissions.
- Wrong key and corrupted ciphertext fail closed.
- Checkpoint and rotation crash tests pass.
- No secret appears in any sink.

### Gate D: Service security

- Session/CSRF/proxy tests pass.
- Administrator throttling works.
- Public health endpoints reveal no account details.
- Security headers and CSP pass browser tests.

### Gate E: Reliability

- All database migrations and crash tests pass.
- Second replica refusal passes.
- 25-account soak has no starvation, deadlock, unbounded task growth, orphan process, or integrity failure.

### Gate F: Packaging

- amd64 and arm64 images pass.
- SBOM/provenance/attestation/signature exist.
- No fixable high or critical vulnerability remains without documented exception.

### Gate G: Optional access-token support

- Token bootstrap only through stdin/protected input.
- Account and rate-limit reads succeed.
- Replacement and restart work.
- Isolation and redaction pass.

Failure of Gate G removes only access-token support.

---

## 37. Implementation phases and work breakdown

### Phase 0: Feasibility and integration kernel

Deliverables:

- Exact Codex release manifest.
- Schema generator and compatibility fixtures.
- Direct stdio JSONL client.
- Production-like process environment allowlist.
- Runtime tree materializer.
- Device-code harness for two accounts.
- Rate-limit normalization harness.
- One persistent-thread activation harness.
- External submission witness and fault injector.
- Tool-safety proof.
- Access-token decision.

Exit criteria: Gates A and the core of Gate B pass in a standalone harness.

### Phase 1: Core service skeleton

Deliverables:

- Configuration and validation.
- Singleton file lock.
- Database worker.
- Initial schema and migrations.
- Vault crypto and key sources.
- Structured logging pipeline.
- FastAPI lifespan.
- Health endpoints.
- Administrator password, sessions, and CSRF.
- Click skeleton.

Exit criteria: service starts in hardened Compose, login works, database and vault survive restart.

### Phase 2: Account runtime and authentication

Deliverables:

- Account/label repositories and services.
- Runtime manager and actor mailboxes.
- App-server process supervision.
- Device-code enrollment UI/API/CLI.
- Credential checkpointing.
- Identity/workspace evidence.
- Enable/disable/reauthentication/deletion.
- Conditional token enrollment.

Exit criteria: three isolated test accounts can be managed without activation.

### Phase 3: Usage and scheduling

Deliverables:

- Rate-limit adapter.
- Full-read persistence.
- Notification invalidation.
- Polling planner and coalescer.
- Freshness/status derivation.
- Confirmed scheduling.
- Evidence-gated estimated scheduling.
- Dashboard summary/detail usage views.

Exit criteria: fake-time and fake-server tests prove stable scheduling and no blind assumptions.

### Phase 4: Activation and reconciliation

Deliverables:

- Activation schema and operations.
- Fixed runtime profile.
- Persistent thread and turn submission.
- Result normalization.
- Failure classifier.
- Reconciliation engine.
- Manual and scheduled trigger convergence.
- Safety-violation handling.

Exit criteria: complete crash matrix and property invariants pass.

### Phase 5: Product surfaces

Deliverables:

- Modular dashboard layouts.
- State SSE and fallback polling.
- Full account controls.
- Operation views.
- Complete CLI.
- Accessibility verification.

Exit criteria: Chromium smoke suite passes.

### Phase 6: Operations

Deliverables:

- Incident service.
- Webhook destinations/outbox/adapters/signing.
- Log viewer, search, download, and live stream.
- Retention and maintenance.
- Diagnostics.

Exit criteria: webhook and logging failure tests pass with no secret leaks.

### Phase 7: Release hardening

Deliverables:

- Load/soak results.
- amd64 and arm64 validation.
- Release pipeline.
- SBOM, provenance, attestations, signatures.
- Security and operator documentation.
- Final acceptance evidence.

---

## 38. Frozen v1 acceptance scenario

The final release candidate must demonstrate:

1. Start through hardened Docker Compose.
2. Pass liveness and readiness checks.
3. Log in from another trusted LAN device.
4. Add three independently authenticated device-code accounts.
5. Assign overlapping labels.
6. Display fresh short and weekly usage with raw evidence and timestamps.
7. Show confirmed, estimated, or unknown schedule confidence accurately.
8. Manually activate each account once with confirmation.
9. Classify exact `OK` correctly.
10. Display activation history and next activation basis.
11. Restart the container during idle operation and restore all accounts.
12. Restart during controlled activation failpoints and reconcile without duplicate acceptance.
13. Run a fake-time confirmed reset and automatically activate once.
14. Preserve stale usage after a forced refresh failure.
15. Surface an auth failure through dashboard, CLI, logs, incident, and webhook.
16. Resolve the incident after successful reauthentication.
17. Demonstrate log filtering, live follow, and sanitized download.
18. Demonstrate account disable and typed-confirmation deletion.
19. Refuse a second Windowkeeper instance.
20. Verify no plaintext credentials or known secret canaries exist under `/data`, logs, API outputs, SSE payloads, or webhooks.

---

## 39. Failure-mode matrix

| Failure | Required behavior |
| --- | --- |
| Invalid startup configuration | Service not ready; precise redacted diagnostic; no account workers |
| Singleton lock held | Exit nonzero before opening database |
| Wrong vault key | Service unavailable; no destructive writes or credential deletion |
| Corrupted credential ciphertext | Account action required; no plaintext materialization |
| Codex digest/version mismatch | Compatibility blocked; dashboard and diagnostics remain available |
| App-server fails before initialize | Definite operation failure; bounded retry where appropriate |
| Usage read timeout | Preserve prior usage; mark stale; coalesced later retry |
| Sparse rate-limit notification | Debounced full read; never persist partial snapshot as complete |
| Dashboard refresh burst | One upstream read per account plus at most one forced follow-up |
| Crash before activation submission write | Safe definite retry |
| Crash after possible write, before response | Ambiguous; reconcile; no blind retry |
| Crash after turn ID receipt, before DB commit | Reconcile by activation client ID; fail closed if not provable |
| Turn completes non-`OK` | Warning; no resend in same window |
| Tool request appears | Reject/interrupt; safety incident; pause automatic activation |
| Auth expires | `AUTH_REQUIRED`; cancel future activation; operator action |
| Account disabled during queued activation | Cancel before submission; stop runtime after safe drain |
| New reset supersedes planned attempt | Cancel stale plan; create at most one new window attempt |
| Webhook 429 | Persist retry time using provider delay |
| Webhook accepted then local crash | Possible duplicate retry with stable event/delivery IDs |
| Slow SSE client | Gap signal/disconnect; never block producers |
| Log queue full | Bounded drops and synthesized overflow event; no event-loop block |
| Migration fails | Restore verified rollback copy; service unavailable until consistent |
| Host clock jumps | Scheduler wakes/requeries; persisted UTC evidence remains canonical |
| Child ignores SIGTERM | Kill process group after grace; classify active operation safely |

---

## 40. Security threat model

### 40.1 Protected against

- Theft of the SQLite database or persistent `/data` volume without the vault key.
- Accidental secret leakage through normal logging and diagnostics.
- CSRF against state-changing browser actions.
- Session-token database disclosure by storing hashes only.
- Cross-account state reuse caused by shared homes or process ownership.
- Duplicate activations caused by ordinary restart and known crash boundaries.
- Split-brain operation from two service instances on the same local volume.
- Basic webhook redirect and malformed-URL SSRF paths.
- Untrusted forwarded-header spoofing under correct deployment configuration.

### 40.2 Not protected against

- Host root compromise.
- Docker daemon administrator compromise.
- Kernel compromise.
- Reading live process memory by a privileged attacker.
- Malicious replacement of the release image by a trusted registry administrator without signature verification.
- Arbitrary code execution inside the Windowkeeper supervisor process.
- Guaranteed secure deletion from all storage layers.
- A malicious LAN reverse proxy explicitly configured as trusted.

### 40.3 Child-process containment limitation

One service UID and filesystem permissions are not a complete security boundary between concurrently running Codex children. The design minimizes overlap, uses private runtime paths, and starts at most one child per account. Phase 0 must inspect whether same-UID sibling access is a practical issue in the hardened container. If strict child compromise isolation becomes a release requirement, per-account UIDs or containers are a post-v1 architectural change unless required to pass the security gate.

---

## 41. Performance targets

Initial targets are provisional release gates, not upstream guarantees.

### 41.1 Dashboard

- Cached dashboard HTML p95 below 500 ms on minimum supported hardware for 25 accounts.
- Cached account API p95 below 250 ms.
- No request waits for a Codex usage read.

### 41.2 Usage

- A simultaneous 25-account poll is bounded by concurrency 4.
- Dashboard bursts produce one execution per account.
- Account refresh completion p95 remains within a documented window based on upstream latency.

### 41.3 Database

- Short write transactions.
- No foreground request blocked by retention for more than a small bounded interval.
- `quick_check` passes after crash suites.

### 41.4 Runtime

- No more than configured process starts concurrently.
- Idle app-server children terminate after grace.
- No orphan children after shutdown.
- File descriptors and memory return to a stable baseline during soak.

### 41.5 Logging and SSE

- Log producers never block the event loop on disk I/O.
- Slow clients cannot increase global queue memory without bound.
- Drop/gap counters are observable.

---

## 42. Operator documentation requirements

Documentation must include:

- Independent/unofficial project notice.
- Account ownership and authorization requirement.
- No credential sharing or quota pooling.
- Exact supported deployment model.
- Vault key generation and safe storage.
- Administrator password bootstrap.
- Direct-LAN and reverse-proxy examples.
- Trusted-proxy warning.
- Upgrade and rollback behavior.
- Codex version compatibility policy.
- Meaning of confirmed, estimated, and unknown schedules.
- Ambiguous activation fail-closed behavior.
- At-least-once webhook behavior.
- No secure-erasure guarantee.
- Log redaction limits and diagnostic-bundle contents.
- Recovery steps for wrong key, auth expiry, incompatible Codex version, and database migration failure.

---

## 43. Open implementation questions and decision deadlines

### 43.1 Exact Codex release

**Decision deadline:** End of Phase 0.  
Select one stable release based on complete compatibility and protected tests.

### 43.2 Access-token support

**Decision deadline:** End of Phase 0.  
Ship only if rate-limit reads, persistence, replacement, and isolation pass.

### 43.3 Complete credential file allowlist

**Decision deadline:** Phase 0 authentication experiment.  
Confirm whether forced file storage mutates only `auth.json` or additional credential-critical files.

### 43.4 Strict no-tool profile

**Decision deadline:** Before ActivationService implementation is accepted.  
If the pinned runtime cannot avoid all tool requests with the proposed profile, automatic activation is blocked until a supported profile is found.

### 43.5 Turn materialization timing

**Decision deadline:** Phase 0 fault injection.  
Measure when a submitted turn becomes discoverable by `thread/read` and set the bounded reconciliation grace.

### 43.6 Upstream stable identity evidence

No public stable account/workspace ID is assumed. v1 uses local UUID, display evidence, and optional forced workspace restriction. A future stable upstream identifier can be added without changing local primary keys.

### 43.7 Exact freshness and retry thresholds

Finalize through fake-server and container latency tests. Threshold changes require a documented decision rather than silent test relaxation.

---

## 44. Decision register

| Area | v1 decision |
| --- | --- |
| Product | Multi-account automatic activation retained |
| Duplicate contract | At most one accepted turn per account/window; ambiguous fails closed |
| Usage source | App-server `account/rateLimits/read` only |
| Private endpoint | Removed |
| Window semantics | Classify by duration, not primary/secondary name |
| Sparse notification | Dirty signal followed by full read |
| Schedule | Confirmed reset first; evidence-gated observed-duration estimate |
| Universal 5-hour fallback | Removed |
| Activation thread | One new persistent thread per logical attempt |
| Turn correlation | `clientUserMessageId = activation_id`; correlation only, not idempotency |
| Same-account concurrency | Fully serialized |
| Cross-account concurrency | Bounded semaphores |
| Runtime reuse | On-demand with 30-second idle grace |
| Service topology | One Python process, one Uvicorn worker, one event loop |
| SQLite | WAL, FULL, explicit SQL, dedicated owner thread |
| Migrations | Custom numbered forward-only with backup-API rollback copy |
| Vault | AES-256-GCM, HKDF account keys, mounted key file preferred |
| Sessions | Opaque server-side SQLite sessions |
| CSRF | Synchronizer token plus same-origin validation |
| UI | FastAPI/Jinja/native JS/SSE, no frontend framework |
| SSE | One state stream plus separate log stream |
| CLI | Click, local-only, same services, durable operations |
| Logs | structlog + stdlib, sanitized once, JSONL, bounded live stream |
| Incidents | One open incident per scope/problem key |
| Webhooks | Generic JSON canonical; Slack/Discord adapters; at-least-once |
| Docker | Initial and primary distribution, amd64/arm64 |
| PyPI | Deferred |
| Scale | Supported through 25 enabled accounts |
| Replicas | One only, enforced with `flock` |

---

## 45. Source-to-plan mapping

This plan is synthesized from the following attached materials:

- `00_README.md` — research-pack purpose, scope discipline, evidence hierarchy.
- `01_MASTER_PLAN.md` — original product definition, frozen features, activation purpose, dashboard/CLI/webhook goals.
- `02_WINDOWKEEPER_POLICY_AND_PRODUCT_VIABILITY_REPORT.md` — useful account-ownership, no-pooling, supported-interface, naming, and private-endpoint cautions; its no-go disposition is intentionally ignored.
- `03_RESEARCH_CODEX_AUTH_AND_ACCOUNT_ISOLATION_REPORT(1).md` — direct app-server boundary, isolated runtime homes, device login, token bootstrap gate, workspace constraints, exact-version pinning.
- `04_RESEARCH_USAGE_TELEMETRY_AND_SCHEDULING_REPORT(1).md` — authoritative rate-limit source, duration classification, notification invalidation, coalescing, stale preservation, scheduling evidence.
- `05_RESEARCH_ACTIVATION_EXECUTION_AND_RECONCILIATION_REPORT(1).md` — persistent thread per activation, acceptance boundary, lack of documented turn idempotency, fail-closed ambiguity, tool-safety gate.
- `06_Windowkeeper_Security_Vault_Admin_Auth_Research_REPORT.md` — AES-GCM/HKDF vault, runtime materialization, Argon2id, sessions, CSRF, proxy trust, health minimization.
- `07_RESEARCH_RUNTIME_ARCHITECTURE_AND_CONCURRENCY_REPORT(1).md` — one-process topology, account actors, fixed lanes, semaphores, short runtime reuse, process supervision, singleton lock.
- `08_RESEARCH_DATA_MODEL_MIGRATIONS_AND_RETENTION_COMPLETED(1).md` — SQLite settings, explicit repository, durable state boundaries, activation operation evidence, migration backup, retention.
- `09_RESEARCH_DASHBOARD_API_AND_UX_COMPLETED(1).md` — server rendering, native JS, SSE, stable view models, accessibility, modular layouts.
- `10_RESEARCH_LOGGING_AND_DIAGNOSTICS_COMPLETED(1).md` — sanitizing queue, JSONL fan-out, browser log viewer, bounded search, TRACE restrictions.
- `11_RESEARCH_WEBHOOKS_AND_INCIDENTS_COMPLETED(1).md` — incident deduplication, transactional outbox, HTTP client isolation, retries, signing, provider adapters.
- `12_RESEARCH_CLI_API_AND_SERVICE_BOUNDARIES_COMPLETED(1).md` — Click CLI, local execution, durable operation resources, internal HTTP API, Problem Details.
- `13_RESEARCH_DOCKER_PACKAGING_NETWORKING_RELEASE_COMPLETED(1).md` — hardened Docker image, multi-architecture packaging, tmpfs, release supply-chain controls.
- `14_RESEARCH_TESTING_RELIABILITY_AND_COMPATIBILITY_REPORT(1).md` — fake app-server, schema validation, crash witness, injected clock, property tests, browser/container/live release gates.
- `crash_phase_experiment_results(1).json` — evidence that committed SQLite phase markers survive forced process termination conservatively.
- `runtime_research_experiment_results(1).json` — evidence for singleton lock behavior, refresh coalescing, bounded fairness, subprocess escalation, and adequate SQLite throughput in a local experiment.

---

## 46. Final implementation statement

Windowkeeper v1 will be built as a small, single-instance, evidence-driven service rather than a distributed automation platform. Its complexity is concentrated only where correctness requires it:

- Strict account isolation.
- Encrypted credential lifecycle.
- Durable operation state.
- At-most-one activation behavior.
- Crash reconciliation.
- Stable protocol compatibility.
- Secret-safe observability.

The product retains the original automatic activation objective. The implementation avoids the unsafe assumptions discovered during research: direct private endpoint access, shared credentials, hard-coded slot meanings, universal five-hour timing, blind ambiguous retries, floating Codex versions, multiple service replicas, and UI-coupled backend behavior.

This document is the v1 implementation baseline. Any scope addition must be deferred unless it is necessary to satisfy an invariant, close a security defect, or complete one of the explicitly included features. Any change to activation acceptance, account isolation, credential persistence, scheduling evidence, or protocol compatibility requires an explicit architecture decision and corresponding regression tests.
