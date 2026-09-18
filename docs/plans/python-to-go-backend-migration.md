# Python-to-Go backend migration plan

Status: Go implementation complete; copied-production-volume canary and release publishing remain operator release gates

Baseline commit: `32fe1e1` (`main`)

Audit date: 2026-09-18

Scope: replace the Python backend and CLI with Go while preserving all deployed data, protocols, integrations, and operator workflows

## 1. Outcome and non-negotiable constraints

The final repository will ship one Go binary named `codex-broker`. That binary will provide the private broker server, isolated public-enrollment server, CLI, volume preparation, migrations, background workers, and offline maintenance commands currently implemented in Python.

The migration is complete only if all of the following are true:

1. An existing `windowkeeper-data` volume starts under the Go image without account relogin, manual database edits, key conversion, or credential re-enrollment.
2. Existing Pi, Hermes, and T3 Code installations continue using their current URL, client key, CA, request bodies, response parsing, and failover behavior without modification.
3. `docker compose up --build -d` still starts the private broker and public enrollment service with the same ports, volume, certificates, environment variables, health behavior, and command names.
4. The Go implementation can read every durable value written by Python, and the last Python release can read values written by Go during the rollback window.
5. The credential-authority invariant remains intact: exactly one mutable `ACTIVE` lineage per account, every authenticated Codex runtime is checkpointed before plaintext cleanup, and uncertainty quarantines instead of guessing.
6. The public enrollment process remains isolated from the database, vault, broker runtime, administrator routes, and lease authority.
7. The administrator HTML UI, internal browser API, machine API, SSE stream, webhooks, CLI output, and HTTP security boundaries remain behaviorally compatible.
8. Python is absent from the runtime image and is not required for development, tests, bootstrap, health checks, or release builds after the cutover.
9. Go implementation and test source is smaller than the replaced Python implementation and test source. This is a release gate, not a reason to compress readable code or hide behavior in generated code.
10. Performance claims are supported by before/after measurements. The rewrite must improve idle memory, startup time, and broker request overhead without weakening SQLite durability or credential safety.

“Same functionality” means observable and durable compatibility. Python module import paths, FastAPI/Pydantic internals, Click decorators, and asyncio-specific implementation details are not public contracts and will not be recreated.

## 2. Repository audit summary

### 2.1 Product topology

The repository contains four cooperating surfaces:

- The Python backend is the credential authority, usage tracker, account router, administrator UI, public-enrollment coordinator, webhook sender, and maintenance CLI.
- `packages/pi-extension` is a TypeScript client. It requests an access-only lease at each Pi user-turn boundary, calls Codex directly, and reroutes on quota/auth failures.
- `integrations/hermes-agent` is a pinned Git submodule containing the reviewed Hermes core integration. Its synchronous lease manager consumes the same machine API and never persists broker leases.
- T3 Code consumes the documented broker API through an external fork. Only its documentation is present here.

The broker is a control plane. It does not proxy inference traffic. Revoking a broker client key prevents future leases but cannot revoke an already issued OpenAI access token.

### 2.2 Startup and ownership flow

The private process currently starts in this order:

1. Load `WINDOWKEEPER_*` configuration and `.env`.
2. Create persistent and runtime directories.
3. acquire `windowkeeper.lock` with a non-blocking process lock;
4. open `windowkeeper.db`, apply ordered SQL migrations, create a pre-migration database backup, seed instance metadata, and run `PRAGMA quick_check`;
5. read the vault key, derive the instance vault, and verify or create the `windowkeeper:<instance>` sentinel;
6. inspect the managed `codex` executable and require exactly `codex-cli 0.145.0` for readiness;
7. construct the runtime manager, application services, administrator security, client-key service, credential authority, router, event broadcaster, logbook, and webhook dispatcher;
8. bootstrap the administrator password only if no password already exists;
9. reconcile interrupted credentials, logins, operations, public enrollments, and webhook deliveries;
10. declare readiness only when the vault, administrator, and compatible Codex executable are all present;
11. start usage polling, window pulses, maintenance, and webhook delivery only when ready.

Shutdown reverses ownership: stop service tasks, stop webhooks, close/quarantine runtimes, close SQLite, flush logging, and release the singleton lock.

### 2.3 Main runtime flows

#### Machine routing

`POST /api/v1/route` authenticates a hash-only `cbk_` client key, validates the request, removes expired per-client exclusions, and selects only enabled, non-deleted, verified, stopped accounts with an `ACTIVE` credential. Ranking is:

1. earliest future weekly reset;
2. earliest future short reset;
3. preferred public account ID;
4. account creation time;
5. internal account ID.

A reported quota failure refreshes authoritative usage before exclusion. An auth failure first forces a managed token refresh and can return the same account with a fresh lease; if that fails, it marks authentication required and excludes the account. Exhausted accounts require known reset evidence. If the pool has a reliable future reset/exclusion, the endpoint returns a padded 429 wait; otherwise it returns `POOL_RESET_UNKNOWN` instead of inventing a retry time.

The lease contains only the account public ID, label, access token, upstream ChatGPT account ID, expiry, remaining percentages, and reset timestamps. It never contains the ID token or refresh token.

#### Managed credential use

Every operation capable of refreshing credentials follows one per-account critical section:

1. decrypt the current `ACTIVE` payload;
2. atomically transition `worker_state` from `STOPPED` to `CREDENTIAL_IN_USE`;
3. materialize `auth.json` into a new isolated runtime generation;
4. start and initialize `codex app-server` with a constrained environment;
5. perform the requested RPC;
6. regardless of RPC success, cancellation, or failure, stop Codex and capture `auth.json`;
7. promote a new encrypted `ACTIVE` generation only if its fingerprint changed;
8. remove plaintext only after the checkpoint is durable;
9. transition back to `STOPPED` when safe;
10. preserve the runtime, set `CREDENTIAL_QUARANTINED`, require reauthentication, and open an incident if checkpoint safety is uncertain.

Go cancellation must preserve this ordering. A canceled caller may stop waiting for ordinary work, but it may not cancel the checkpoint, cleanup, state transition, or incident write.

#### Login and credential creation

Administrator and public enrollment both use isolated Codex login runtimes. The broker stores interaction secrets only in memory, binds each interaction to a session hash and nonce hash, validates browser callback scheme/host/port/path/state, verifies the returned ChatGPT identity and optional workspace, and checkpoints the login source before deleting plaintext.

After the source is durable, the service asks Codex to rotate it into the managed `ACTIVE` lineage. When no immutable `EXPORT` exists, it makes a second best-effort rotated snapshot. Export failure does not invalidate the safe managed account. Identity mismatch never promotes a candidate. Public enrollment additionally reserves the normalized email before promotion, rejects managed or pending duplicates, renames/enables the placeholder on success, and soft-deletes uncredentialed placeholders on failure.

#### Usage and window pulses

Usage refreshes coalesce per account, run under a configured semaphore, use `account/rateLimits/read`, normalize a short window by duration and a weekly window near 10,080 minutes, record raw/sanitized evidence, update current usage, and publish SSE events. Authentication and checkpoint failures update durable account state and incidents.

A window pulse is a fixed ephemeral minimal turn. Eligible accounts are claimed transactionally, pulse operations are coalesced, known-exhausted windows are skipped, retries are bounded by `window_pulse_retry_seconds`, and the next pulse is scheduled at the earliest future normalized reset.

#### Public process

The public process has only `/`, `/start`, `/status`, `/static/*`, and `/health/live`. Before every visitor request except liveness it calls the private availability endpoint over verified HTTPS with the dedicated enrollment key. It uses an opaque secure cookie containing high-entropy enrollment capabilities, applies per-source throttles, validates the OpenAI verification URL, and returns an empty 404 while disabled.

#### Webhooks

Webhook URLs and optional signing secrets are vault-encrypted. Creation accepts only absolute HTTPS public hosts. Delivery resolves DNS first, rejects non-global addresses, pins one resolved address while retaining the original Host header and TLS SNI, forbids redirects, bounds the response excerpt, signs immutable bodies with HMAC-SHA256, and retries on the durable schedule. Restart converts `LEASED` deliveries back to `RETRY_SCHEDULED`.

### 2.4 Durable state model

Migrations 001 through 011 create and evolve these tables:

- metadata and configuration: `schema_migrations`, `instance_metadata`, `vault_state`, `settings`, `log_level_overrides`;
- accounts: `accounts`, `account_state`, `labels`, `account_labels`;
- credentials and enrollment: `credential_bundles`, `login_attempts`, `public_enrollments`;
- usage and scheduling: `usage_current`, `usage_snapshots`, `window_pulse_state`;
- client routing: `client_api_keys`, `account_exclusions`;
- operations and incidents: `operations`, `incidents`;
- webhooks: `webhook_destinations`, `webhook_events`, `webhook_deliveries`;
- administrator auth: `admin_credentials`, `admin_sessions`.

Migration 009 intentionally removed activation tables and `account_state.activation_state`. Historical migration files must remain byte-for-byte unchanged because their names/checksums are persisted and they are also the direct upgrade path from older installations.

The important state machines are:

- account lifecycle: `ENROLLING` -> `ACTIVE` -> `DELETED`;
- auth: `ENROLLING`/`UNCONFIGURED` -> `VERIFIED` or `AUTH_REQUIRED`;
- credential worker: `STOPPED` -> `CREDENTIAL_IN_USE` -> `STOPPED`, or `CREDENTIAL_QUARANTINED` on uncertainty;
- login: `CREATED` -> `STARTING_RUNTIME` -> `STARTING_LOGIN` -> `WAITING_FOR_USER` -> `VERIFYING_ACCOUNT` -> `CHECKPOINTING_CREDENTIAL` -> `COMPLETED`, with cancellation, retryable/action-required failure, expiry, restart, and superseded terminals;
- operation: `QUEUED` -> `RUNNING`/`WAITING_FOR_USER` -> `SUCCEEDED`, `FAILED`, or `CANCELLED`;
- incident: `OPEN` -> `RESOLVED` or administrative `CLOSED`;
- public enrollment: `ACTIVE` -> `COMPLETED` or `FAILED`;
- webhook delivery: `PENDING` -> `LEASED` -> `SUCCEEDED`, `RETRY_SCHEDULED`, or `FAILED`.

## 3. Compatibility ledger

These are protocol identifiers, not cleanup opportunities.

### 3.1 Filesystem and deployment

Preserve exactly:

- database file: `windowkeeper.db`;
- singleton lock: `windowkeeper.lock`;
- Docker volume: physical name `windowkeeper-data`;
- persistent root default: `.windowkeeper/data` locally and `/data` in the image;
- runtime root default: `.windowkeeper/run` locally and `/run/windowkeeper` in the image;
- active log: `windowkeeper.jsonl` and rotated `windowkeeper-*.jsonl`;
- runtime account tree: `accounts/<internal-account-id>/<generation>/` with `home`, `codex-home`, `tmp`, and `workspace`;
- container identity: UID/GID `10001:10001`;
- private port `8787`, public port `8788`, and Compose service names/network alias;
- certificate mount paths and read-only/tmpfs/capability settings;
- Compose commands `serve` and `public-serve`.

The Go entrypoint must perform the current root-only volume ownership change and then drop supplementary groups, GID, and UID before starting application goroutines.

### 3.2 Configuration

The Go loader will continue to accept `.env` and the complete `WINDOWKEEPER_*` namespace. It must preserve defaults, bounds, and validation for:

- data/runtime/log directories, host, port, root path, trusted proxies, TLS cert/key;
- public-enrollment key, broker origin, CA, host/port/TLS, global cap, and attempts per hour;
- vault/admin secret values and protected secret-file overrides;
- cookie mode and session idle/absolute limits;
- usage polling/concurrency;
- window-pulse enablement, polling, retry, and concurrency;
- auth and process-start concurrency;
- reset padding;
- browser OAuth mode, pinned callback ports, callback size, and login timeout;
- Codex executable/version and log level.

The configuration parity table is:

| Field / environment suffix | Current default | Validation or derived behavior |
| --- | --- | --- |
| `DATA_DIR` | `.windowkeeper/data` | persistent root |
| `RUNTIME_DIR` | `.windowkeeper/run` | must differ from data root |
| `LOG_DIR` | unset | becomes `<data>/logs` |
| `HOST` / `PORT` | `127.0.0.1` / `8787` | port 1-65535; non-loopback requires TLS |
| `ROOT_PATH` | empty | empty or leading `/`, never trailing `/` |
| `TRUSTED_PROXIES` | empty | comma-separated IP/CIDR; wildcard forbidden |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | unset | used together for private HTTPS |
| `PUBLIC_ENROLLMENT_KEY` | unset | when present, at least 32 characters |
| `PUBLIC_ENROLLMENT_BROKER_URL` | `https://codex-broker:8787` | HTTPS origin only, no credentials/path/query/fragment |
| `PUBLIC_ENROLLMENT_CA_CERT` | unset | verified private-broker CA |
| `PUBLIC_ENROLLMENT_HOST` / `PORT` | `127.0.0.1` / `8788` | port 1-65535; non-loopback requires public TLS |
| `PUBLIC_ENROLLMENT_TLS_CERT_FILE`, `PUBLIC_ENROLLMENT_TLS_KEY_FILE` | unset | used together for public HTTPS |
| `PUBLIC_ENROLLMENT_MAX_ACTIVE` | `4` | 1-32 |
| `PUBLIC_ENROLLMENT_ATTEMPTS_PER_HOUR` | `3` | 1-20 |
| `VAULT_KEY_FILE`, `VAULT_KEY` | unset | key file cannot be inside data root |
| `ADMIN_PASSWORD_FILE`, `ADMIN_PASSWORD` | unset | protected file overrides environment |
| `COOKIE_SECURE` | `auto` | `auto`, `true`, or `false` |
| `SESSION_IDLE_MINUTES` | `44,640` | at least 1 |
| `SESSION_ABSOLUTE_HOURS` | `2,160` | at least 1 |
| `USAGE_POLL_SECONDS` | `300` | at least 60 |
| `USAGE_REFRESH_CONCURRENCY` | `4` | 1-16 |
| `WINDOW_PULSE_ENABLED` | `true` | boolean |
| `WINDOW_PULSE_POLL_SECONDS` | `60` | at least 10 |
| `WINDOW_PULSE_RETRY_SECONDS` | `900` | at least 60 |
| `WINDOW_PULSE_CONCURRENCY` | `2` | 1-8 |
| `AUTH_CONCURRENCY` | `2` | 1-8 |
| `PROCESS_START_CONCURRENCY` | `2` | 1-8 |
| `RESET_PADDING_SECONDS` | `10` | 0-300 |
| `BROWSER_OAUTH_MODE` | `manual` | `disabled`, `manual`, or `host-loopback` |
| `BROWSER_OAUTH_CALLBACK_PORTS` | `1455,1457` | values may only be the pinned 1455/1457 set |
| `LOGIN_TIMEOUT_SECONDS` | `900` | 60-3600 |
| `BROWSER_CALLBACK_MAX_BYTES` | `16,384` | 1,024-65,536 |
| `CODEX_EXECUTABLE` | `codex` | exact version inspected at startup |
| `CODEX_VERSION` | `unknown` | replaced by observed compatible version |
| `LOG_LEVEL` | `INFO` | logger level |

`CODEX_BROKER_BIND_ADDRESS`, `CODEX_BROKER_PORT`, and public equivalents remain Compose interpolation variables. They are not silently conflated with application `WINDOWKEEPER_*` variables.

Secret files must remain protected regular files, must not be symlinks, and must take precedence over environment values. The vault-key file must remain outside the data directory. Persistent and runtime directories must differ after path resolution.

### 3.3 Cryptography and credentials

Preserve exactly:

- vault key text format `wk1_` plus unpadded base64url for 32 random bytes;
- AES-256-GCM with a 12-byte nonce;
- HKDF-SHA256, salt equal to UTF-8 instance UUID;
- HKDF info `windowkeeper/credential-bundle/v1:<key_id>:<account_id>`;
- AAD fields and meanings: instance, account, bundle, key, envelope version, payload schema version;
- payload schema version 1 and envelope version 1;
- allowed captured paths `auth.json` and legacy `config.toml`, while only `auth.json` is materialized;
- maximum captured credential file size of 2 MiB;
- SHA-256 content fingerprints;
- sentinel plaintext `windowkeeper:<instance>` and scope `vault-sentinel`;
- webhook scopes `webhook:<destination-id>:url` and `webhook:<destination-id>:secret`;
- credential states `ACTIVE`, `EXPORT`, and `RETIRED` and their uniqueness rules.

Cross-language fixtures must prove all four directions: Python encrypt/Go decrypt, Go encrypt/Python decrypt, Python Argon2 verify/Go login, and Go Argon2 hash/Python verify. Go-generated JSON does not need byte-identical key ordering, but stored AAD must authenticate, payloads must round-trip, and rollback software must parse every new envelope.

### 3.4 Tokens, IDs, hashes, and cookies

Preserve:

- client key prefix `cbk_`, 32 random bytes encoded as unpadded URL-safe base64, 12-character display prefix, and SHA-256-only storage;
- constant-time comparison across active key hashes and immediate revocation;
- vault key prefix `wk1_`;
- public account tokens from 18 random bytes of unpadded URL-safe base64;
- sessions, CSRF tokens, login nonces, and public enrollment tokens from 32 random bytes;
- SHA-256 hashes for sessions, CSRF, login/public capabilities, and client keys;
- administrator Argon2id PHC strings with the current parameters: time 3, memory 65,536 KiB, parallelism 1, 16-byte salt, 32-byte hash;
- cookie names `wk_session`, `wk_csrf`, and `cb_public_enrollment`;
- cookie paths, `HttpOnly`, `Secure`, `SameSite`, and `Max-Age` behavior;
- browser session idle-touch behavior and the absolute-expiry ceiling.

The internal ID generator should retain the existing time-hex plus eight-random-byte shape so IDs remain sortable enough for current diagnostics without changing column expectations.

### 3.5 SQLite behavior

The Go store must enforce on every connection:

- `foreign_keys=ON`;
- `busy_timeout=5000`;
- `synchronous=FULL`;
- `trusted_schema=OFF`;
- WAL and incremental auto-vacuum as established by migration 001;
- one owning process and one serialized writer;
- `BEGIN IMMEDIATE` for state transitions that currently depend on write ownership;
- pre-migration backups named `windowkeeper.pre-v<version>.db`;
- ordered migration discovery, persisted name/checksum/application time, and current max-version behavior;
- `PRAGMA quick_check` after migration and `integrity_check` for backup/restore;
- existing foreign keys, partial indexes, strict tables, nullability, and epoch units.

Do not start Python and Go against the same volume, even though the singleton lock should reject the second process. All drills explicitly stop one before starting the other.

### 3.6 HTTP machine API

Preserve methods, paths, authentication, media types, status codes, field names, nullability, and retry headers for:

- `GET /health/live`;
- `GET /health/ready`;
- `GET /api/v1/health`;
- `POST /api/v1/route`;
- `POST /api/private/v1/public-enrollments/availability`;
- `POST /api/private/v1/public-enrollments`;
- `POST /api/private/v1/public-enrollments/status`.

Route validation continues to reject unknown JSON fields, enforce current length bounds, accept only `quota`, `auth`, or `rate_limit`, and require `failed_account_id` and `failure_kind` together. Errors raised as broker problems retain `application/problem+json`, `urn:codex-broker:problem:*`, `code`, `detail`, and request-path instance. Validation remains 422.

Timestamps remain RFC 3339 UTC ending in `Z`. To minimize textual drift, the formatter will match Python's millisecond-derived convention: no fraction at exact seconds and six fractional digits otherwise. `Retry-After` remains a non-negative integer equal to the JSON wait calculation.

### 3.6.1 Operational constants

The following constants are behavior and must either remain exact or be changed in a separately reviewed compatibility decision:

| Area | Current value |
| --- | --- |
| supported managed Codex | `codex-cli 0.145.0` |
| app-server maximum JSON-line frame | 8 MiB |
| app-server notification queue | 256 messages; oldest is dropped on overflow |
| app-server spawn/initialize timeout | 15 seconds |
| normal app-server RPC timeout | 30 seconds |
| app-server graceful close before kill | 10 seconds |
| access-token refresh skew | 5 minutes before expiry |
| captured `auth.json` maximum | 2 MiB |
| administrator password | 15-128 characters |
| account/client-key display name | normalized whitespace, 1-80 characters |
| account labels | at most 20 distinct normalized labels, each at most 40 characters |
| route session/turn IDs | 1-200 characters |
| route preferred/failed public IDs | at most 100 characters |
| private enrollment capabilities | session/nonce 32-200 characters; enrollment/attempt IDs 32-100 characters |
| login/client auth throttle | 5 attempts per source per 60 seconds, cleared on success |
| private enrollment-key throttle | 20 attempts per source per 60 seconds, cleared on success |
| public status throttle | 120 requests per source per 60 seconds |
| browser callback forward | no redirects, 2-second connect and 5-second total timeout |
| SSE replay/client buffers | 2,000 events / 256 events |
| SSE heartbeat | 15 seconds |
| log in-memory recent buffer | 2,000 events |
| log writer queue | 10,000 events, with a later dropped-count warning |
| log active-file/directory limits | 25 MiB / 1 GiB |
| rotated-log retention | 30 days |
| maintenance cadence/failure retry | 1 hour / 5 minutes |
| operations, snapshots, webhook-event, retired-credential retention | 30 days, pruned 250 rows per pass |
| resolved incident retention | 90 days, pruned 250 rows per pass |
| incremental vacuum | 64 pages per maintenance pass |
| scheduled usage jitter | 0-30 seconds in addition to configured poll interval |
| route exclusion without reset | 60 seconds for `rate_limit`; 300 seconds otherwise |
| webhook delivery lease | 30 seconds |
| webhook retry seconds | 60, 300, 1,800, 7,200, 21,600, 43,200, 86,400, 86,400 |
| webhook connect/total timeout | 3 / 10 seconds |
| webhook response excerpt | 512 bytes before redaction |
| redacted/logged string bound | 8,192 characters after CR/LF flattening |
| public-process broker connect/total timeout | 3 / 10 seconds |
| Pi response bound / request timeout | 64 KiB / 60 seconds; client behavior remains unchanged |
| weekly-window target/tolerance | 10,080 minutes / 5 percent |
| short-window eligibility | greater than 0 and less than 1,440 minutes; unique shortest duration |
| pulse prompt/tier | `Reply OK.` / `default`; prefer visible text mini model and minimal then low effort |

### 3.7 Administrator web and internal browser API

Preserve all existing routes listed below. GETs require an administrator session where they do today; mutations require the session plus CSRF; login remains source-address throttled.

- `/login`, `/logout`, `/`;
- `/accounts/new`, `/accounts`, `/accounts/{public}`;
- account auth export, refresh, reauthenticate, labels, enabled, and delete mutations;
- `/operations/{id}`, `/incidents`, `/logs`, `/logs/export`, `/settings`;
- settings mutations for public enrollment, client keys, and webhooks;
- `/api/internal/v1/dashboard`;
- `/api/internal/v1/operations/{id}`;
- login interaction, browser callback, and cancellation endpoints;
- `/api/internal/v1/events/state` SSE.

Keep 303 redirects, root-path rewriting, template-visible fields, form field names, current CSP/security headers, API no-store headers, SSE event names/payloads/replay-gap semantics, 15-second heartbeats, static asset paths, and log export format. Preserve trusted-proxy behavior only for explicitly configured IP/CIDR ranges; wildcard trust remains forbidden.

### 3.8 Public web

Preserve the five-route surface, empty disabled 404s, middleware availability check, strict security headers including HSTS, start/status throttles, opaque-cookie contents and limits, polling states, safe `auth.openai.com` verification URL check, and absence of broker state mounts.

### 3.9 CLI

The Go binary must preserve these commands and meaningful exit codes/output shapes:

- `serve`, `public-serve`;
- `init`, `password-set`, `version`, `health`, `status`, `doctor`;
- `client-key create|list|revoke`;
- `backup`, `restore --confirm RESTORE`;
- `vault generate-key|verify|rotate`.

Keep `--json`/`--json-output`, `codex-broker.dev/cli/v1`, offline lock ownership, protected-file rules, atomic replacement/fsync behavior, all-or-nothing vault rotation, one-time key display, and the `windowkeeper` command alias for one compatibility release.

### 3.10 External clients

The TypeScript Pi extension, pinned Hermes submodule, Hermes installer, and T3 Code contract should not need functional changes. Their existing tests become acceptance tests for the Go server. No shared SDK or new client protocol is introduced.

## 4. Target Go architecture

Use a small dependency graph and avoid a one-file translation of `services.py`.

```text
cmd/codex-broker/
  main.go                    command dispatch and process exit codes
internal/config/
  config.go                  env/.env loading, defaults, validation
internal/platform/
  files.go                   protected files, atomic writes, fsync
  lock_unix.go               singleton flock
  privilege_linux.go        volume ownership and privilege drop
internal/store/
  db.go                      connection ownership and pragmas
  migrate.go                 embedded unchanged migrations and backups
  models.go                  row/value types with explicit null handling
  *.go                       cohesive repositories by table family
internal/vault/
  vault.go                   HKDF/AES-GCM envelopes and sealed text
  credential.go              safe capture/materialize/auth extraction
internal/codex/
  client.go                  JSON-line app-server transport
  adapter.go                 login/account/rate-limit/pulse RPCs
  runtime.go                 isolated process trees and quarantine
internal/auth/
  admin.go                   Argon2id sessions and CSRF
  clients.go                 hash-only client keys
internal/broker/
  broker.go                  lifecycle and dependency assembly
  accounts.go                account queries and mutations
  login.go                   login interaction state machine
  credentials.go             managed checkpoint and lease authority
  usage.go                   normalization, polling, pulses
  routing.go                 eligibility, ranking, exclusions, waits
  operations.go              durable operation transitions
  incidents.go               incident/webhook lifecycle
  enrollment.go              private public-enrollment coordination
internal/webhook/
  dispatcher.go              queue claim/retry/delivery
  destination.go             encrypted destinations and SSRF-safe dialing
internal/events/
  broadcaster.go             bounded SSE replay and slow-client gaps
internal/logbook/
  logbook.go                 redaction, queue, rotation, recent view
internal/httpserver/
  private.go                 private/admin machine server and middleware
  handlers_*.go              route groups with narrow dependencies
internal/publicsite/
  server.go                  isolated public process and broker client
internal/cli/
  *.go                       offline and server commands
web/
  templates/                 Go html/template equivalents
  static/                    existing CSS and JavaScript
  public_templates/
  public_static/
```

Dependency direction is one-way:

```text
config/core types
       ↓
platform  store  vault  codex  events  logbook
       ↓      ↓      ↓      ↓
        auth   webhook   broker
                 ↓        ↓
             publicsite  httpserver
                    ↓     ↓
                       cli/cmd
```

`internal/broker` is one package split by responsibility, allowing private coordination helpers without creating circular micro-packages. Files should stay focused and functions should normally stay below 80 lines. Comments explain only invariants that are not evident from types and control flow, especially checkpoint cancellation and DNS-pinned TLS.

### 4.1 Dependency policy

Prefer the standard library for HTTP, TLS, JSON, HTML templates, embedding, subprocesses, crypto, synchronization, logging primitives, and command parsing where practical. Expected external modules are limited to:

- a pure-Go SQLite driver with strict-table, WAL, backup, and ARM64 support;
- `golang.org/x/crypto/argon2` for compatible administrator password hashes;
- `golang.org/x/term` for hidden administrator password prompts;
- a well-tested dotenv parser if exact quoted `.env` compatibility would otherwise require custom parsing.

Do not add an ORM, web framework, dependency-injection framework, migration framework, job queue, Redis, code generator, or alternate database. Pin all module versions and commit `go.mod`/`go.sum`.

### 4.2 Concurrency model

- SQLite uses a bounded pool configured as one owning connection/writer, with explicit transactions for compare-and-set transitions.
- A keyed mutex serializes every refresh-capable action for one account. Different accounts may run concurrently under auth/usage/pulse/process semaphores.
- The Codex client has one writer mutex, one read goroutine, a pending-response map, and a bounded notification channel.
- Login interactions remain an in-memory map guarded by a mutex; restart reconciliation remains authoritative.
- Background loops are owned by one lifecycle context and `WaitGroup`.
- Critical checkpoint work uses a cancellation-detached, bounded internal context. Shutdown waits for it or quarantines evidence; it never deletes ambiguous plaintext.
- Webhook delivery remains single-claimer initially. Parallel delivery is out of scope because it changes ordering and load behavior.
- SSE subscribers use bounded channels and receive a gap event rather than blocking publishers.

## 5. File-by-file migration map

### 5.1 Python modules

| Current file | Go destination and treatment |
| --- | --- |
| `config.py` | `internal/config/config.go`; reproduce every field/default/validator and `.env` behavior. |
| `database.py` | `internal/store/db.go` and `migrate.go`; replace closure jobs with typed repositories and explicit transactions. |
| `domain/models.py` | small value types in `internal/store`, `broker`, and HTTP response files; avoid a generic model dumping ground. |
| `domain/usage.py` | `internal/broker/usage.go`; table-driven window selection and anomaly handling. |
| `domain/status.py` | retain only if used by Go state derivation; otherwise cover current results in compatibility tests and remove dead production code. |
| `errors.py` | one typed broker error with code/detail/status and HTTP problem rendering. |
| `ids.py`, `clock.py` | focused helpers in `internal/platform`/core; inject clock/random sources into tests. |
| `secret_types.py` | remove duplicate wrapper; keep secrets in unexported fields with redacted `String` methods only where formatting is possible. |
| `compatibility.py` | `internal/codex/compatibility.go`; exact executable/version inspection and safe messages. |
| `security.py` | `internal/auth/admin.go`; parse and produce compatible Argon2id PHC values. |
| `client_auth.py` | `internal/auth/clients.go`; preserve token format, constant-time scan, metadata, revocation. |
| `credential_authority.py` | merge lease parsing/refresh decision into `internal/broker/credentials.go`; never expose refresh/ID tokens. |
| `oauth.py` | remove the currently duplicated implementation; one callback contract implementation lives in `broker/login.go`. |
| `events.py` | `internal/events/broadcaster.go`. |
| `logbook.py`, `redaction.py` | `internal/logbook`; preserve file names, bounds, rotation, repair, retention, and recursive redaction. |
| `singleton.py` | `internal/platform/lock_unix.go`. |
| `views.py` | explicit HTTP view constructors; no reflection-based generic serialization. |
| `runtime.py` | `internal/codex/runtime.go`; preserve paths, modes, environment allowlist, generation uniqueness, archive/quarantine semantics. |
| `router.py` | `internal/broker/routing.go`; preserve SQL eligibility and deterministic rank tuple. |
| `codex/client.py` | `internal/codex/client.go`; bounded 8 MiB JSON-line frames, request IDs, auth classification, write evidence, close/kill semantics. |
| `codex/adapter.py` | `internal/codex/adapter.go`; preserve exact RPC method/parameter shapes and pulse model choice. |
| `services.py` | split across the `internal/broker` files above; port behavior by state machine and transaction, never line-by-line. |
| `vault.py` | `internal/vault`; exact cross-language crypto and safe Linux file operations. |
| `webhooks.py` | `internal/webhook`; preserve immutable provider bodies, retry schedule, signature, SSRF controls, and SNI. |
| `web/app.py` | `internal/httpserver`; standard `net/http` middleware and grouped handlers. |
| `web/public.py` | `internal/publicsite`; separate process mode with no store/vault imports in its dependency closure. |
| `cli/main.py` | `internal/cli` plus `cmd/codex-broker`; keep command names, flags, exit codes, and atomic offline operations. |
| `container_entrypoint.py` | main process preflight in `internal/platform/privilege_linux.go`. |
| `version.py` | build-injected version with a deterministic development fallback. |
| empty `__init__.py` files and `py.typed` | remove at final cutover. |

### 5.2 SQL, templates, and static assets

- Move migrations under `internal/store/migrations` only if `go:embed` requires it; preserve their contents and filenames exactly.
- Convert Jinja templates to `html/template` once, keeping the rendered DOM hooks, form names, links, visible wording, escaping, and root-path behavior.
- Reuse CSS and JavaScript unchanged except for changes proven necessary by template rendering. Embed all production assets into the binary.
- Add template compile tests and representative escaped-value rendering tests.
- Preserve downloadable `auth.json` bytes exactly; do not reserialize the export.

### 5.3 Tests

- Port every existing Python unit/integration behavior to Go tests.
- Replace `tests/fake_codex.py` with a small Go test binary implementing the same JSON-line protocol and fault markers, so the final test suite requires no Python.
- Keep the Python suite temporarily as the reference oracle during development; delete it only after every behavior has a Go equivalent and differential tests pass.
- Keep Pi tests unchanged and point their HTTP acceptance cases at the Go binary.
- Add a narrow broker API acceptance test in the Hermes submodule workflow without modifying its pinned production commit during this backend rewrite.

### 5.4 Root and deployment files

- Replace `pyproject.toml`, `uv.lock`, `pyrightconfig.json`, wheel build logic, Ruff/Mypy/Pyright/Pytest jobs, and Python cache ignores with `go.mod`, `go.sum`, `go test`, `go vet`, formatting, race, vulnerability, and static-analysis gates.
- Rewrite the Dockerfile as a pinned multi-stage Go build. The runtime still installs the pinned Codex npm package and CA certificates, but contains no Python/pip/wheel.
- Replace Python Docker health checks with a binary TCP/liveness probe that does not weaken TLS verification for normal clients.
- Keep Compose services, volume name, mounts, ports, commands, resource limits, and security options stable.
- Rewrite `scripts/bootstrap.sh` to avoid Python while retaining refusal-to-overwrite, file modes, CA/server/public certificate generation, and displayed output.
- Keep `scripts/install-hermes-integration.sh` functionally unchanged; rerun it against the Go server.
- Update README, Product, Context, Operations, Security, public enrollment, integration docs, and release gates only after commands and behavior are final.
- Keep `plan.md` as the implemented historical product-rewrite/API rationale and link this migration plan rather than overwriting it.
- Leave the TypeScript workspace and pinned Hermes gitlink in place.

## 6. Implementation phases

Each phase ends in a reviewable, passing commit. The production Compose target remains Python until Phase 7.

### Phase 0 — freeze the actual contract

1. Record Python baseline CPU, RSS, startup, image size, health latency, route latency, and 50-way lease behavior.
2. Make tests hermetic by always supplying an explicit fake Codex executable when readiness is under test.
3. Generate sanitized fixtures for schema versions 1, 3, 4-drift, 6, 10, and 11.
4. Generate cross-language vault, sealed webhook, Argon2, session, client-key, route, webhook-body, and timestamp golden cases.
5. Capture normalized HTTP and CLI transcripts, excluding volatile IDs/timestamps/secrets.
6. Build a requirement-to-test matrix from every Python and Pi test.
7. Decide documented-vs-actual discrepancies before coding. Do not silently preserve or silently fix them.

Known discrepancies to resolve in this phase:

- `scripts/bootstrap.sh` currently writes an unprefixed base64 vault key, while the backend accepts only `wk1_...`.
- `codex-broker health` currently constructs HTTP even for normal TLS deployments.
- the readiness failure test changes outcome when a compatible `codex` exists on `PATH`;
- CI still references removed `src/windowkeeper` package paths and old wheel contents;
- release documentation says migration coverage through 010 although schema 011 exists.

The intended fixes are: emit canonical `wk1_` keys for new bootstraps, add an explicit compatibility decision for already generated raw keys, make health honor configured TLS/CA behavior, isolate tests from host tools, and correct stale CI/docs. None may alter stored credentials or client API contracts.

### Phase 1 — Go skeleton, configuration, and platform primitives

1. Add the Go module, command dispatcher, build version, clock/random seams, typed errors, protected-file helpers, singleton lock, privilege drop, and atomic fsync helpers.
2. Implement configuration loading and exhaustive table tests against Python defaults/validation.
3. Implement command help/version and compatibility aliases.
4. Add binary embedding for assets and migrations.
5. Establish line-count, lint, race, and benchmark reporting in CI.

Exit gate: no production switch; config and platform golden tests pass on Linux AMD64 and ARM64.

### Phase 2 — storage and cryptographic compatibility

1. Implement SQLite ownership, pragmas, typed repositories, transactions, migration execution, pre-version backup, quick check, backup, and restore.
2. Open every versioned fixture and migrate it to 11 without changing account/credential meaning.
3. Implement vault key decoding/generation, HKDF, AES-GCM, credential capture/materialization, sealed text, and vault rotation.
4. Implement Argon2id PHC parsing/hashing, admin sessions, CSRF, and client keys.
5. Run Python/Go bidirectional compatibility tests, including rollback after Go writes and rotates values.

Exit gate: a copied Python v11 data directory can be opened, verified, listed, backed up, restored, rotated, and reopened by Python with no relogin.

### Phase 3 — Codex subprocess and checkpoint engine

1. Implement the app-server transport, adapter, process-group lifecycle, bounded frames, notifications, authentication error classification, and deterministic close.
2. Implement runtime directory creation, safe materialization, config generation, concurrency limits, deletion, archive, and quarantine.
3. Implement the per-account credential coordinator and cancellation-detached checkpoint protocol.
4. Port login source promotion, managed rotation, immutable export creation, lease parsing, near-expiry refresh, and startup quarantine reconciliation.
5. Run fault injection for spawn failure, RPC error, auth error, corrupt auth file, transport exit, cancellation at every checkpoint boundary, cleanup failure, and shutdown.

Exit gate: success, failure, cancellation, and restart cannot lose or fork the authoritative credential; the race detector is clean.

### Phase 4 — broker domain services

1. Port accounts, labels, details, enable/disable/delete, operations, and state views.
2. Port login state transitions, interaction capabilities, callback forwarding, identity/workspace verification, cancellation, and public identity reservation.
3. Port usage normalization, current/snapshot writes, polling, coalescing, window pulses, and retention.
4. Port deterministic routing, exclusions, exact wait calculation, and credential leases.
5. Port incidents, event publishing, webhook event creation, destination management, claim/retry/delivery, and recovery.
6. Test all state transitions with SQL postconditions, not only returned values.

Exit gate: all former service/router/vault/runtime/webhook tests have Go equivalents and 50 concurrent near-expiry routes cause exactly one credential mutation.

### Phase 5 — private and public HTTP surfaces

1. Port security/header/root-path/proxy middleware and typed request decoding.
2. Port health, machine, private enrollment, admin HTML, internal JSON, SSE, static, and export handlers.
3. Convert and embed templates while preserving DOM contracts used by JavaScript.
4. Port the isolated public server with a minimal dependency graph and verified internal HTTPS client.
5. Run normalized differential requests against Python and Go for every route, including failures, redirects, cookies, media types, and headers.
6. Run Pi, Hermes, and T3 Code contract tests against Go.

Exit gate: external clients are unchanged; public image/process inspection proves no database/vault/runtime authority is reachable.

### Phase 6 — CLI and operations

1. Port all online/offline commands and exit behavior.
2. Verify protected key/backup files, symlink rejection, permissions, atomic replacement, fsync, WAL-safe backups, schema validation, and all-or-nothing rotation.
3. Add TLS-aware health while retaining machine-readable output.
4. Rewrite bootstrap without Python and test both IPv4 and IPv6 certificate SANs.
5. Port operator documentation and recovery drills.

Exit gate: every documented command works from the Go binary against both a fresh store and a copied Python store.

### Phase 7 — container cutover and Python removal

1. Switch Docker build/entrypoint/healthcheck to Go while preserving the final runtime filesystem, Codex version, UID, ports, and Compose contract.
2. Run fresh Compose, old-volume upgrade, public enrollment, account login, lease, usage checkpoint, webhook, backup/restore, vault rotation, and restart smoke tests.
3. Stop Go and boot the last Python image against the same canary volume to prove rollback compatibility; then boot Go again.
4. Remove Python production and test sources, packaging, lockfiles, caches, and runtime dependencies.
5. Confirm the final source line-count gate and repository inventory.

Exit gate: `docker compose up --build -d` upgrades an existing deployment without relogin and all release gates pass.

### Phase 8 — live canary and release

1. Back up a real deployment database and vault key separately.
2. Restore them into an isolated canary using identical secrets and certificates.
3. Verify account listing, export decryption, route leasing, forced near-expiry refresh, usage refresh, checkpoint promotion, public enrollment toggle, webhook delivery, and restart reconciliation.
4. Compare performance and logs to baseline, scan the image/SBOM, and test AMD64/ARM64.
5. Roll back the canary once, then repeat the upgrade.
6. Release only after the operator signs off. Never test first on the only production volume.

## 7. Verification matrix

### 7.1 Automated correctness

- `go test ./...` and race-enabled tests for concurrent packages;
- formatting, vet, static analysis, dependency vulnerability scan, and module verification;
- migration fixtures through v11, idempotency, foreign keys, strict tables, backups, and v4 drift recovery;
- old sentinel and every credential state decrypt without relogin;
- bidirectional vault/sealed-text/password compatibility;
- success/failure/cancellation/restart checkpoint cases;
- concurrent valid leases do not mutate credentials;
- concurrent near-expiry leases mutate once;
- usage poll, window pulse, login, and lease refresh serialize per account;
- deterministic ranking, preference, failed-account exclusion, exhaustion, unknown reset, and padding;
- client key one-time display, constant-time authentication, revocation, deletion, and redaction;
- persistent admin sessions, idle/absolute limits, CSRF, logout, password-change revocation, throttling;
- all private/admin/public routes, headers, cookies, redirects, root path, proxies, SSE replay/gaps;
- public enrollment caps, duplicate identity, restart cleanup, disabled empty 404, and process isolation;
- webhook encryption, immutable bodies, provider formatting, signature, DNS pinning, SNI, no redirects, response bound, retries, restart recovery;
- CLI JSON/text/exit behavior, backup/restore, key verification/rotation;
- template compile/escaping and existing browser JavaScript behavior;
- Pi tests/typecheck and Hermes broker-manager acceptance.

### 7.2 Fuzz and adversarial tests

Fuzz at least:

- JWT segment decoding and claim typing;
- vault envelope/AAD/base64/JSON parsing;
- credential bundle paths, hashes, sizes, symlinks, and partial writes;
- callback authorization URLs and forwarded callback URLs;
- redaction of nested structures, tokens, and hostile URLs;
- app-server oversized/partial/malformed frames and response IDs;
- route/public JSON with unknown fields, extreme lengths, booleans-as-numbers, nulls, and duplicate keys;
- enrollment handle decoding;
- webhook URLs, DNS answers, IPv4/IPv6 literals, credentials, ports, and redirects;
- migration scripts and interrupted migration restoration.

### 7.3 Performance benchmarks

Measure Python and Go using the same host, store, fake Codex, and request set:

- cold startup to liveness and readiness;
- idle RSS for private and public processes;
- image size and dependency/CVE count;
- authenticated health requests per second and p50/p95 latency;
- route latency with a valid token and no mutation;
- 50/250 concurrent valid routes;
- near-expiry route burst with one Codex refresh;
- dashboard render with 1, 25, and 250 accounts;
- SSE fanout to slow and normal clients;
- webhook queue claim/delivery overhead;
- migration, backup, restore, and vault rotation on representative databases.

Release thresholds:

- no correctness or durability tradeoff for speed;
- Go idle RSS and startup time must improve materially;
- non-mutating route p95 must not regress;
- exactly one refresh remains the concurrency result;
- Codex/network-bound operations may be unchanged, which is acceptable if broker overhead falls.

## 8. Rollout and rollback

### 8.1 Upgrade

1. Stop both Compose services.
2. Back up `windowkeeper.db`, vault key, `.env`, and certificates separately.
3. Build the Go image and run it first against a copied volume.
4. Verify `health/ready`, administrator login, account list, one lease, one managed checkpoint, export, webhook, and backup.
5. Start production Go services with the same Compose project and physical volume.
6. Watch readiness, checkpoint incidents, logs, webhook retries, and client routing.

No account relogin is an acceptable upgrade step.

### 8.2 Rollback

The initial Go release should add no schema migration unless a separately reviewed requirement makes one unavoidable. With schema 11 unchanged and bidirectional crypto/password tests passing, rollback is:

1. stop Go completely;
2. preserve any quarantined runtime evidence;
3. start the pinned last Python image with the same volume and vault key;
4. verify readiness, account list, lease, and checkpoint;
5. if any persisted format is not readable, stop and restore the pre-upgrade database/key pair together.

Never restore only the database after vault rotation. Never delete a quarantined runtime merely to make rollback look clean.

## 9. Principal risks and mitigations

| Risk | Mitigation and release evidence |
| --- | --- |
| Go SQLite driver changes locking, types, backup, or pragma semantics | Pure-Go driver spike first; single connection; explicit `BEGIN IMMEDIATE`; fixture and WAL backup tests; real-volume canary. |
| Crypto is mathematically correct but byte/protocol incompatible | Golden fixtures in both directions; retain all legacy literals; Python rollback decrypts Go output. |
| Go Argon2 implementation cannot verify stored Python hashes | Parse PHC explicitly and test both generation directions before any web work. |
| Context cancellation deletes or fails to persist rotated credentials | Detach checkpoint/cleanup from caller cancellation; fault injection at every await-equivalent boundary; quarantine on uncertainty. |
| Process termination leaves Codex or plaintext behind | start a separate process group, TERM then KILL with bounds, wait/reap, checkpoint before removal, startup reconciliation. |
| `database/sql` null scanning or integer conversions change state | typed nullable fields and table-driven fixture reads; never scan arbitrary rows into maps in critical code. |
| Go JSON/time formatting breaks Pi/Hermes/T3 parsing | normalized golden HTTP tests plus unchanged client suites; Python-compatible UTC formatting. |
| `html/template` escaping or truthiness changes the UI | representative snapshots with hostile values; browser smoke for every form and login flow. |
| Proxy/root-path behavior changes redirects or secure cookies | CIDR-aware forwarded-header tests and root-path route/asset/cookie tests. |
| SSRF protection regresses through DNS rebind or TLS handling | custom pinned dialer, global-IP validation, original SNI/Host, redirects disabled, adversarial resolver tests. |
| Go map iteration makes routing, JSON, or webhooks nondeterministic | ordered SQL, explicit slices, sorted keys where contracts need stability, no map iteration in selection logic. |
| Pure-Go binary loses Linux file-safety flags | Linux-specific `openat`/`O_NOFOLLOW`, mode and symlink tests, explicit Linux support boundary. |
| Privilege drop occurs after goroutines start | volume preparation and setgroups/setgid/setuid happen at the first lines of main before runtime initialization. |
| Public binary mode accidentally imports private authority | dependency-closure test and container inspection; public Compose service still has no data mount. |
| Existing clients see stricter validation than FastAPI provided | capture valid and invalid request corpus from Python; match accepted values/status codes before cutover. |
| Logging leaks secrets through Go error formatting | central redaction before queue/storage, typed safe errors, forbidden-secret tests across logs/problem responses/webhooks. |
| Fewer lines becomes unreadable code | count is a final architecture check; no minification, generation, or multi-purpose god files; reviews enforce package/function limits. |
| Rewrite expands into protocol redesign | no API v2, schema redesign, router-policy change, proxy, RBAC, HA, or client SDK in this effort. |

## 10. Explicitly out of scope

- changing Codex version or app-server protocol;
- changing account selection policy;
- adding a reverse inference proxy;
- changing refresh-token/export semantics;
- schema normalization or renaming legacy identifiers;
- Redis, distributed workers, multi-process database access, or HA;
- client key scopes/RBAC or mTLS;
- redesigning Orbit UI or public enrollment;
- rewriting the Pi extension, Hermes integration, or T3 fork;
- fixing unrelated product behavior without a frozen compatibility test and separate review.

## 11. Definition of done

- [x] Every backend route, CLI command, background worker, state transition, and durable table has a Go owner.
- [x] The unchanged migrations upgrade legacy databases through schema 11 and preserve credential generations.
- [x] Go decrypts the frozen Python vault fixture and verifies the frozen Python administrator hash fixture.
- [ ] Fault-injection coverage exercises every checkpoint failure boundary and quarantine transition.
- [x] Fifty concurrent near-expiry routes cause exactly one refresh and one new `ACTIVE` generation.
- [x] Pi tests and type checking pass without client changes.
- [x] Public enrollment remains a separate no-state process and disabled routes are empty 404s.
- [x] Webhook SSRF address filtering and provider body contracts have Go coverage; delivery retries are durable in schema 11.
- [x] Runtime image contains no Python runtime, pip, or wheel; the binary-name shim preserves unchanged Compose health checks.
- [x] Bootstrap and the full test/release workflow require no Python runtime.
- [x] The Go implementation plus tests is 8,464 lines versus 10,251 replaced Python implementation plus tests.
- [x] Documentation describes Go commands and retains the legacy compatibility warnings.
- [ ] Linux AMD64 and ARM64 release images pass published vulnerability gates.
- [ ] Performance results demonstrate the expected runtime improvement on the release host.
- [ ] A copied production volume passes canary, rollback to the pinned Python image, and second-upgrade drills.
