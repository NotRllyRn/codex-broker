# Codex Broker — Rewrite and Monorepo Plan

Status: **implemented on this branch; production-volume and live Hermes validation remain external release gates**
Date: 2026-09-03  
Inputs audited:

- this repository at commit `d4d336b` — the 60-test Windowkeeper baseline;
- `sources/pi-0.84.4.zip` — current Pi monorepo source and extension/provider APIs;
- `sources/hermes-agent-2026.8.31.zip` — current Hermes source, plugin hooks, middleware, provider runtime, and credential pools.

There is no Pi Relay or Hermes pool-plugin checkout in `sources/`. References to porting those repositories were removed: the Pi package is a small extension built directly against Pi 0.84.4, while Hermes changes are specified in a patch document for implementation and live validation in the external Hermes checkout.

## 1. Decision

Rename **Windowkeeper** to **Codex Broker**.

- Repository/package name: `codex-broker`
- Primary CLI: `codex-broker`
- Short description: **Central Codex auth, quota, and account-routing service.**
- Optional tagline: **One credential authority for every Codex client.**

The product stops being a usage-window activation supervisor and becomes the **single authority for Codex account credentials**. The former automatic activation/model-turn feature is removed: it spends quota, adds an unnecessary token-mutating runtime, and is not needed for usage tracking or routing.

The architectural invariant is:

> **One Codex account has one mutable refresh-token lineage, and Codex Broker is the only process allowed to mutate it. Pi, Hermes, and future clients are consumers.**

This directly removes the failure class that exists today, where Windowkeeper, Pi Relay, and Hermes can each independently refresh copies of the same rotating refresh token.

### Why this is the correct boundary

OAuth refresh-token rotation intentionally invalidates the previous refresh token. RFC 9700 describes rotation as issuing a new refresh token on refresh and invalidating the prior one so replay can be detected. Independent programs holding copies are therefore inherently capable of invalidating one another even when each program is locally race-safe.

OpenAI's current Codex App Server documentation now exposes experimental `chatgptAuthTokens` specifically for a **host application that owns the user's ChatGPT auth lifecycle**. The host supplies an access token and ChatGPT account ID, and Codex can ask the host for a fresh token after an authorization failure. That is the same ownership model proposed here.

## 2. What will not be built in v1

YAGNI is a hard requirement for this rewrite. Do **not** add:

- a full `/v1/responses` reverse proxy;
- Redis, a queue, a second database, or distributed locks;
- Kubernetes/service discovery;
- mTLS;
- public-Internet exposure or tunnel management;
- client RBAC/scopes;
- complex weighted/least-used routing;
- quota reservations or predicted token accounting;
- automatic HA/failover of the broker;
- refresh tokens in Pi or Hermes;
- separate Git repositories nested inside the main repository;
- automatic activation/model turns or reset-time model submissions.

A full inference proxy would make Codex Broker own SSE/streaming, cancellation, backpressure, request replay semantics, and upstream protocol changes. Pi Relay already demonstrates why post-output failover is application-specific. The broker should remain a **control plane**, not become the data plane unless a later requirement makes that necessary.

## 3. Target topology

```text
                         home LAN

   Pi + adapter ----------------------------+
      |                                     |
      | HTTPS: route/lease                  |
      v                                     |
   Codex Broker                             |
      |                                     |
      +-- encrypted ACTIVE credential A     |
      +-- encrypted ACTIVE credential B     |
      +-- encrypted ACTIVE credential C     |
      +-- usage/reset state                 |
      +-- stable account router             |
      +-- client access keys                |
      |                                     |
      +-- returns ACCESS token only --------+
                                            |
   Pi calls Codex directly <----------------+

   Hermes + adapter follows the same model if the compatibility spike passes.
```

Refresh tokens never leave Codex Broker. Access tokens are handed to authenticated LAN clients and used directly against Codex.

### Important security limitation

Revoking a **Codex Broker client key** immediately blocks that client from obtaining future leases. It cannot claw back an OpenAI access token that the client already received; that token may remain useful until upstream expiry. Immediate revocation of already-issued upstream tokens would require keeping access tokens out of clients entirely, which means building the full reverse proxy deliberately excluded from v1.

For the intended trusted home-LAN deployment, accept this limitation and document it. If immediate revocation becomes a real requirement later, reconsider the proxy architecture then.

## 4. Repository shape

This is one Git monorepo with independently installable adapter packages, **not three nested Git repositories**.

```text
codex-broker/
├── src/codex_broker/              # renamed/evolved Windowkeeper service
│   ├── credential_authority.py    # NEW: only refresh-token owner
│   ├── router.py                  # NEW: account selection + retry timestamps
│   ├── client_auth.py             # NEW: machine/client access keys
│   └── ...                        # existing service, UI, usage, vault, etc.
├── packages/
│   └── pi-extension/              # small Pi 0.84.4 extension package
├── tests/
├── docs/
├── Dockerfile
├── compose.yaml
├── pyproject.toml
├── README.md
└── plan.md
```

Do not create a shared cross-language SDK in v1. The broker API is tiny; duplicating a small HTTP client in TypeScript and Python is simpler than inventing another package.

## 4.1 Baseline implementation audit

The current service is not a skeleton. Preserve these tested seams rather than replacing them:

- `ApplicationServices._credential_locks`, `_run_managed_locked()`, `_replace_active_row()`, and `_promote_active_payload()` already provide the only safe checkpoint/promotion path.
- `RuntimeManager.start_fresh()` materializes one isolated `CODEX_HOME`; `Vault.capture()` accepts only `auth.json`; the legacy HKDF info, sentinel, database, lock, cookies, and Docker volume are on-disk protocol identifiers.
- `usage_current` stores reset times in **epoch seconds**, while web/domain views use milliseconds. Broker routing must convert once at the API boundary.
- account ordering is currently by lowercase display name. Broker routing instead uses `accounts.created_at_ms, accounts.account_id`, independent of dashboard sorting.
- a machine lease needs data not represented in the domain models. Add dedicated `Lease`, route-request, and pool-wait types rather than overloading `AccountSummary`.
- current `auth.json` is opaque vault payload containing a JSON object whose ChatGPT tokens normally live at `tokens.access_token`, `tokens.id_token`, and `tokens.refresh_token`. Lease extraction must validate this shape, decode JWT payloads without trusting them as authorization decisions, read `exp`, and read the upstream account id from `https://api.openai.com/auth.chatgpt_account_id`. Never return the ID or refresh token.
- App Server has no dedicated “refresh access token” RPC in this code. `account/read` with `refreshToken=false`, followed by `account/rateLimits/read` when necessary, is the minimal authenticated operation; every run is checkpointed before the lease is returned.
- the baseline test command is `uv sync --extra dev && uv run pytest` (60 tests at the audited commit).

Implementation structure:

1. mechanically rename `src/windowkeeper` to `src/codex_broker` first and keep compatibility literals explicit;
2. put client-key persistence in `ClientKeyService` and inject it into web state;
3. make `CredentialAuthority` a collaborator of `ApplicationServices` using the existing managed-run callbacks/lock, not a second lock map or direct SQL promotion implementation;
4. make `Router` query eligibility/usage and call the authority; it never decrypts credentials itself;
5. keep `/health/live` and `/health/ready`; `/api/v1/health` is the authenticated-client-facing readiness alias;
6. use RFC 3339 UTC strings ending in `Z`, integer non-negative `Retry-After`, and reject inconsistent failure fields (`failure_kind` without `failed_account_id` or vice versa) with 422.

---

# 5. Existing code audit and exact changes

## 5.1 Windowkeeper / future Codex Broker

### `pyproject.toml` — MODIFY

Current project name is `codex-windowkeeper` and the CLI is `windowkeeper = "windowkeeper.cli.main:cli"`.

Change to:

- project: `codex-broker`;
- description: `Central Codex auth, quota, and account-routing service.`;
- package namespace: `codex_broker`;
- primary CLI: `codex-broker = "codex_broker.cli.main:cli"`;
- optionally retain `windowkeeper = "codex_broker.cli.main:cli"` for one compatibility release only;
- update type-check/package paths mechanically.

Do not mix this rewrite with unnecessary dependency upgrades. Keep the currently pinned Codex version until the broker behavior passes migration/integration tests, then test a Codex upgrade separately.

### `src/windowkeeper/` → `src/codex_broker/` — RENAME

Physically rename the Python package and mechanically update imports.

Visible class/error names such as `WindowkeeperError` should become `BrokerError` where inexpensive. Historical database/crypto identifiers are exceptions and are listed in the migration section below.

### `src/codex_broker/config.py` — MODIFY

Current relevant defaults:

- `session_idle_minutes = 15`
- `session_absolute_hours = 8`
- `reauth_minutes = 5`

New defaults:

- `session_idle_minutes = 44_640` (**31 days**)
- `session_absolute_hours = 2_160` (**90 days**)
- remove `reauth_minutes`
- add `reset_padding_seconds = 10`
- add `tls_cert_file: Path | None`
- add `tls_key_file: Path | None`
- add `tls_ca_file: Path | None` for CLI health checks only when a private CA is not in system trust

Configuration naming:

- document new `CODEX_BROKER_*` variables;
- cheaply accept legacy `WINDOWKEEPER_*` aliases for one migration release where Pydantic allows it;
- do not introduce dozens of knobs. Selection policy is fixed in v1.

### `src/codex_broker/security.py` — SIMPLIFY

Keep:

- Argon2 administrator login password;
- DB-backed sessions;
- CSRF tokens;
- login throttling integration;
- session idle/absolute expiry;
- logout;
- password bootstrap/set-password behavior;
- password changes revoking existing sessions.

Delete:

- `reauth_minutes` from settings protocol;
- `AdminSecurity.reauthenticate()`;
- `AdminSecurity.require_recent_reauth()`;
- all step-up-password state transitions.

Do **not** destructively alter the old `admin_sessions.reauthenticated_at_ms` column. Leave it unused so migration remains additive and rollback remains easy.

Security consequence, intentionally accepted: possession of a valid logged-in browser session grants all administrator capabilities. CSRF and TLS still protect the session, but there is no second password challenge for destructive/sensitive actions.

### `src/codex_broker/web/app.py` — MODIFY HEAVILY

#### Persistent browser login

Current login code sets `wk_session` and `wk_csrf` without `Max-Age`/`Expires`, making them browser-session cookies even though the database tracks longer expiries.

Change both login cookies to persistent cookies:

- `Max-Age`: match the 90-day absolute session lifetime;
- `Secure`: true under the normal HTTPS deployment;
- session cookie: `HttpOnly=true`, `SameSite=Lax`;
- CSRF cookie: remains readable by the page, `SameSite=Lax`;
- preserve cookie names `wk_session` / `wk_csrf` for the first renamed release so upgrading does not gratuitously log the administrator out.

The DB still enforces 31-day idle and 90-day absolute expiration; the cookie merely survives browser restarts.

#### Remove administrator re-password prompts

Delete `require_reauthentication()` and the dedicated `reauth_throttle`.

For every mutation below, keep `require_form()` so authentication + CSRF still apply, but remove `admin_password: Form()` and the reauthentication call:

- `POST /accounts` — add/sign in account;
- `POST /accounts/{public}/auth-export` — download `auth.json` export;
- `POST /accounts/{public}/ambiguity/acknowledge`;
- `POST /accounts/{public}/reauthenticate` — this route is **re-signing into the upstream Codex account**, not browser admin reauthentication; rename the handler internally to avoid semantic confusion;
- `POST /accounts/{public}/delete` — retain the typed account-name confirmation, remove only the admin-password prompt;
- `POST /settings/webhooks`;
- `POST /settings/webhooks/{id}/enabled`;
- `POST /settings/webhooks/{id}/delete`.

Do not remove destructive-action confirmation dialogs or typed account-name confirmation. The request was to remove repeated password entry, not all safety UI.

#### Delete dashboard variants

Current `VARIANTS` contains:

- Orbit cockpit
- Evidence ledger
- Command rail
- Reset timeline
- Account focus

`dashboard.html` explicitly states Orbit is the assigned command-center design. **Keep Orbit. Delete the other four.**

Then:

- delete `VARIANTS` entirely;
- remove the `variant` query parameter from `/`;
- stop passing `variant`/`variants` to the template;
- render the Orbit markup unconditionally.

#### Add machine API

Add only two public machine-facing endpoints initially:

- `GET /api/v1/health`
- `POST /api/v1/route`

Do not split these into a second FastAPI application yet.

Browser `/api/internal/v1/...` endpoints can remain session-authenticated. Rename visible API-version strings from `windowkeeper.dev/internal/v1` to `codex-broker.dev/internal/v1` because there are no external consumers that justify preserving that branding.

### `src/codex_broker/web/templates/dashboard.html` — DELETE PROTOTYPES / KEEP ORBIT

Current file contains all five layouts in conditional branches and a keyboard/UI variant switcher.

Rewrite it to contain:

- the common filters/header;
- the current Orbit layout only;
- no variant hidden input;
- no `variant-*` body class;
- no prototype explanation comment;
- no switcher markup.

### `src/codex_broker/web/static/app.css` — DELETE DEAD VARIANT CSS

Keep the common styles and Orbit block. Remove Ledger, Rail, Timeline, Focus selectors and their responsive overrides.

### `src/codex_broker/web/static/app.js` — DELETE VARIANT SWITCHER

Current code at the `data-variant-switcher` block cycles layouts. Delete the entire block and leave unrelated behavior untouched.

### `src/codex_broker/web/templates/account_new.html` — MODIFY

Delete the `Administrator password` field and reauthentication wording. Account creation still requires a valid logged-in session and CSRF token.

### `src/codex_broker/web/templates/account_detail.html` — MODIFY

Delete password inputs from:

- ambiguity acknowledgement;
- auth.json export;
- upstream account re-login;
- account deletion.

Keep:

- upstream login-method chooser;
- download warning explaining that exported credentials are externally owned;
- typed display-name confirmation for deletion;
- actual upstream sign-in functionality.

Rename UI wording from `reauthenticate` where it means upstream account auth to a clearer **Sign in again**.

### `src/codex_broker/web/templates/settings.html` — MODIFY + ADD CLIENT KEYS UI

Delete password inputs from webhook create/enable/disable/delete.

Add one compact section: **Client access keys**.

Capabilities:

- create a key with a human-readable name (`Pi desktop`, `Hermes server`, etc.);
- show the raw secret exactly once after creation;
- list name, prefix, created time, last-used time, status;
- revoke;
- delete an already-revoked key.

No scopes/roles in v1. Fine-grained control is achieved by giving each client installation its own key and revoking that key when needed.

### `src/codex_broker/migrations/007_client_api_keys.sql` — NEW

Add one additive table. Recommended minimal schema:

```sql
CREATE TABLE client_api_keys (
    key_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    key_prefix TEXT NOT NULL,
    secret_hash BLOB NOT NULL UNIQUE,
    created_at_ms INTEGER NOT NULL,
    last_used_at_ms INTEGER,
    revoked_at_ms INTEGER
) STRICT;
CREATE INDEX client_api_keys_active_idx
    ON client_api_keys(revoked_at_ms, created_at_ms);
```

Do not add scopes, expiration, IP allowlists, or per-key rate limits yet.

### `src/codex_broker/client_auth.py` — NEW

One small machine-auth module.

Key format:

- server-generated 256-bit random value;
- recognizable prefix such as `cbk_`;
- raw key shown once;
- database stores only SHA-256 of the high-entropy secret plus a display prefix;
- authenticate with `Authorization: Bearer cbk_...`;
- constant-time comparison;
- update `last_used_at_ms` in the same database-worker queue after successful authentication; the route need not wait for a separate background task;
- never accept the key in query strings;
- never log the raw key.

Argon2 is unnecessary for a server-generated 256-bit random token; a fast cryptographic hash is sufficient because offline guessing is infeasible.

### `src/codex_broker/credential_authority.py` — NEW

This becomes the most important component in the repository.

Responsibilities:

1. Read/decrypt the one `ACTIVE` credential bundle for an account.
2. Extract access token, ChatGPT account ID, and access-token expiry.
3. Return the current access token immediately if safely valid.
4. If near expiry or explicitly forced after a 401, acquire the existing **per-account credential-mutation lock**.
5. Re-read `ACTIVE` after acquiring the lock.
6. Refresh through the existing managed Codex/App Server path rather than inventing a second OAuth implementation.
7. Capture the resulting mutated `auth.json`.
8. Atomically promote the new encrypted bundle to `ACTIVE` using the existing checkpoint/promotion semantics.
9. Return **access token + ChatGPT account ID + expiry**; never return the refresh token through the machine API.

Use a five-minute refresh skew. Access-token expiry comes from the JWT `exp` claim. A missing/malformed token, account claim, or expiry is not leased: run one managed authenticated refresh/checkpoint, re-read ACTIVE, then fail with explicit `CREDENTIAL_FORMAT_INVALID` if it is still invalid.

Do not hold the mutation lock while a Pi/Hermes model request is running. Access tokens can be used concurrently; only operations capable of rotating the refresh token must serialize.

### `src/codex_broker/services.py` — REFACTOR, DO NOT REWRITE FROM SCRATCH

The current code already has useful invariants:

- `_credential_locks` per account;
- fresh isolated managed Codex runtimes;
- checkpointing changed `auth.json` back into encrypted storage;
- one unique `ACTIVE` credential bundle per account;
- usage polling;
- recovery around managed credentials.

Preserve those.

Refactor credential payload/refresh/promotion helpers behind `CredentialAuthority` so these broker-internal operations share the same mutation lock:

- periodic usage refresh;
- manual usage refresh;
- client token refresh after expiry/401;
- upstream account verification/re-login checkpointing.

The existing `_run_managed()` lock currently surrounds an entire managed runtime operation. That remains appropriate for **broker-owned runtimes that can mutate auth state**, but must not be extended to client model-request duration.

Keep `EXPORT` credential bundles separate from `ACTIVE`. Browser `auth.json` download remains a legacy/manual export feature; Pi/Hermes never use `EXPORT` and never receive a refresh token.

### `src/codex_broker/router.py` — NEW

Keep routing intentionally boring.

Inputs:

- optional `preferred_account_id` from the client's previous turn;
- optional `failed_account_id`;
- optional `failure_kind`: `quota`, `auth`, `rate_limit`.

Algorithm:

1. Consider only non-deleted, enabled accounts with an `ACTIVE` credential and no hard auth failure.
2. If the request reports a failure, exclude that account from this selection.
3. For `quota`, immediately refresh that account's usage state using the existing usage mechanism before final pool-exhaustion calculation when practical.
4. For `auth`, ask `CredentialAuthority` to force one refresh attempt; if that fails, exclude the account and mark it as requiring login through existing state/error mechanisms.
5. If `preferred_account_id` remains usable, keep it.
6. Otherwise choose the first usable account in stable creation order; after a failed account, choose the next usable account in circular stable order.
7. Obtain a valid access token from `CredentialAuthority` and return the lease.

This means clients contact the broker on **every user turn**, as requested, while the broker normally returns the same healthy account. That preserves account/prompt-cache affinity instead of pointlessly rotating credentials each turn. Hermes itself documents that credential rotation resets prompt cache.

Do not add round-robin/least-used configuration in v1.

### Pool exhaustion calculation

An account is known exhausted when current Codex usage data says a relevant window has reached its limit. Existing `usage_current` already stores short/weekly used percentages and reset timestamps.

When no usable account remains:

- calculate the earliest known future reset timestamp across the exhausted enabled accounts;
- add `reset_padding_seconds` (default 10 seconds);
- return it as `next_retry_at`;
- also return `retry_after_seconds` and an HTTP `Retry-After` header.

If no trustworthy reset is known, return a 503/explicit unknown-reset error rather than inventing a timestamp.

Do not maintain a quota reservation ledger. Multiple clients can concurrently consume the same account and may race into its cap; v1 reacts to authoritative upstream failure and reroutes. Predictive reservations would add complexity without knowing the eventual request cost.

### `POST /api/v1/route` — CONTRACT

Authentication:

```http
Authorization: Bearer cbk_<secret>
```

Request:

```json
{
  "session_id": "client-session-id",
  "turn_id": "client-turn-id",
  "preferred_account_id": "optional-public-account-id",
  "failed_account_id": "optional-public-account-id",
  "failure_kind": "quota|auth|rate_limit"
}
```

Only `session_id` and `turn_id` are normally sent on an initial turn request. Failure fields are sent when asking for replacement credentials.

Success `200`:

```json
{
  "status": "ok",
  "account_id": "broker-public-account-id",
  "access_token": "<access-token>",
  "chatgpt_account_id": "<upstream-account-id>",
  "expires_at": "RFC3339 timestamp"
}
```

Pool exhausted `429`:

```json
{
  "status": "wait",
  "code": "POOL_EXHAUSTED",
  "next_retry_at": "RFC3339 timestamp",
  "retry_after_seconds": 1234
}
```

Other status codes:

- `401` invalid/revoked client key;
- `429` no account currently has usage and a reset is known;
- `503` vault/runtime/unrecoverable broker state or no reliable reset timestamp.

Do not expose refresh tokens, encrypted bundles, raw `auth.json`, or account-management CRUD through this API.

### `src/codex_broker/runtime.py` and `src/codex_broker/codex/client.py` — SMALL MODIFICATIONS ONLY IF REQUIRED

Reuse the existing managed App Server process and auth-checkpoint path. Add only the smallest primitive needed by `CredentialAuthority`, such as:

- read current account/auth state;
- force an authenticated operation that causes Codex to refresh near-expired credentials;
- return/checkpoint resulting auth state.

Do not implement a parallel raw OAuth refresh client if the existing Codex runtime can remain the authority for upstream token mechanics.

### `src/codex_broker/views.py` — MODIFY

Add client-key view data for Settings, but never include raw secret hashes. Rename user-facing Windowkeeper labels to Codex Broker.

### `src/codex_broker/redaction.py` — EXTEND

Add patterns/fields for:

- `cbk_...` client keys;
- broker `Authorization` headers;
- returned `access_token` fields.

Add tests proving these never appear in logs/exported logs.

### `src/codex_broker/cli/main.py` — MODIFY

Rename commands/help/output to Codex Broker.

Add only useful operational commands if the web UI is unavailable:

- `codex-broker client-key create NAME`
- `codex-broker client-key list`
- `codex-broker client-key revoke ID`

The UI remains the primary management path. Do not duplicate a large management UI in CLI.

Keep old storage/sentinel compatibility described below.

### `src/codex_broker/container_entrypoint.py` — MODIFY

Support direct TLS inputs and launch the app with the configured certificate/key. For non-loopback binding, fail startup when TLS is missing.

### `Dockerfile` — MODIFY

Rename visible package/user labels as appropriate, but avoid changing storage semantics during the first migration release.

Do not automatically generate a CA/certificate inside the image. Certificate trust is a deployment concern and should be explicit.

### `compose.yaml` / `compose.host-network.yaml` — MODIFY CAREFULLY

Expose the service on the selected LAN address/port over HTTPS.

Critical migration rule: preserve the existing Docker volume identity. Either keep the physical volume name `windowkeeper-data` or declare the renamed logical volume with:

```yaml
name: windowkeeper-data
```

Otherwise Compose would create a fresh empty volume and make the existing accounts appear lost.

Mount local TLS certificate/key files read-only.

Replace the current HTTP health check with a local process/readiness check that does not require disabling TLS verification on the network-facing path.

---

# 6. Storage and credential migration: no account relogin

This rewrite must boot directly on the user's existing Windowkeeper data.

## Preserve these identifiers even though the product is renamed

### Vault KDF/AAD/domain-separation strings

`src/windowkeeper/vault.py` currently uses a literal of the form:

```text
windowkeeper/credential-bundle/v1:<key_id>:<account_id>
```

**Do not rename this string.** It is part of the cryptographic derivation/context for existing encrypted credentials. Changing it would make old bundles undecryptable.

### Vault sentinel plaintext

CLI/bootstrap code uses a sentinel beginning with:

```text
windowkeeper:<instance>
```

**Do not rename it.** Treat it as an on-disk format identifier, not product branding.

### Database and singleton lock

For the first Codex Broker release, keep the existing physical database and lock names, including `windowkeeper.db` / `windowkeeper.lock` if currently configured that way.

Keeping the old lock name has an extra safety benefit: an old Windowkeeper binary and a new Codex Broker binary pointed at the same data directory cannot accidentally run as independent credential authorities at the same time.

### Credential rows

Existing `credential_bundles` already enforces one `ACTIVE` row per account and contains encrypted credentials. Do not copy/re-enroll them. Codex Broker reads the same rows using the same vault key and immediately becomes their owner.

Migrations 007 and 008 add client keys and temporary account exclusions. Migration 009 drops only the retired activation tables and `account_state.activation_state`; no account or credential row is copied, re-enrolled, or rewritten.

## Upgrade procedure

1. Stop old Windowkeeper.
2. Back up the data volume + vault key.
3. Start Codex Broker against the **same** volume and vault key.
4. Apply migrations 007-009; retain the automatic pre-v9 database backup.
5. Verify vault sentinel with the unchanged legacy format.
6. Decrypt/read every existing `ACTIVE` credential without writing it.
7. Display existing accounts and usage.
8. Confirm only retired activation schema/history was removed.
9. Perform one controlled usage refresh on one account and verify checkpoint promotion.
10. Only then enable Pi/Hermes broker consumers.

Expected result: **zero ChatGPT account sign-ins required.**

## Rollback

Migrations preserve every credential format, but migration 009 removes schema expected by old Windowkeeper. Roll back by restoring the automatic pre-v9 database backup, then start the old image with the same vault key. Never run old and new binaries concurrently.

---

# 7. LAN transport security

The home LAN is trusted enough to avoid tunnels/reverse-proxy infrastructure, but plain HTTP is not sufficient. Both the broker client key and the OpenAI access token are bearer credentials: anyone who captures them can use them.

RFC 6750 requires confidentiality/integrity protection for bearer-token transport, and OWASP recommends TLS for all authenticated/sensitive web-service traffic.

## Minimal solution: HTTPS directly from Uvicorn

Use FastAPI/Uvicorn's native TLS support:

- bind to `0.0.0.0:8787` or the chosen LAN interface;
- configure `ssl_certfile` and `ssl_keyfile`;
- clients connect to `https://<broker-lan-ip>:8787`;
- certificate SAN includes the broker IPv4 address;
- Pi/Hermes host trusts the local CA/certificate.

For a home network, use a local CA tool such as `mkcert` to create a certificate for the broker's LAN IP and install that CA on the Pi/Hermes machines. This avoids Nginx/Caddy/Tailscale/mTLS while still preventing passive sniffing and ordinary MITM token capture.

Operational rule:

> **Codex Broker refuses a non-loopback bind unless TLS certificate and key are configured.**

No HTTP redirect is necessary for the machine API. The normal documented endpoint is HTTPS only.

---

# 8. Pi adapter plan

The Pi 0.84.4 provider and extension APIs are the starting point; no relay code is available or needed.

Target package directory: `packages/pi-extension/`  
Package name: `@codex-broker/pi-extension`

Pi's current extension API exposes `before_agent_start` after each submitted user prompt and `agent_start`/`agent_end` once per user prompt. It also supports custom providers. That gives a clean per-user-turn lease boundary.

## Workflow per Pi user turn

1. User submits a prompt.
2. `before_agent_start` calls `POST /api/v1/route`.
3. It sends current Pi session ID, a turn ID, and the prior `account_id` as `preferred_account_id` when available.
4. Broker makes a fresh routing decision and returns the access-token lease.
5. Pi stores that lease **in memory only** for this turn.
6. Pi's Codex provider uses that lease for all LLM/tool-loop requests belonging to the turn.
7. New user prompt => broker is called again even if the old access token is still valid.

This exactly meets the requested “ask every new user turn” behavior without asking on every inner tool-loop request.

## Failover

Use conservative failure behavior:

- **before meaningful model output:** a quota/auth failure may request another lease and retry once;
- **after meaningful model output:** do not replay automatically. Surface the failure; a future continuation design requires separate proof.

On quota/auth failure, the adapter calls `/api/v1/route` again with `failed_account_id` + `failure_kind`.

If broker responds `POOL_EXHAUSTED`:

1. display a concise waiting status;
2. sleep until broker's exact `next_retry_at` (the broker already included padding);
3. call `/route` again;
4. automatically continue/resume.

Do not calculate reset windows independently inside Pi anymore. Broker is authoritative.

## Pi files to port/modify/delete

Build directly against Pi 0.84.4; there is no relay source to port.

Minimal package:

- `src/index.ts` — extension lifecycle, one lease fetch in `before_agent_start`, in-memory current/preferred account state, status command, and a narrow override of the built-in `openai-codex` provider;
- `src/client.ts` — tiny HTTPS `/api/v1/route` client, broker types, strict response validation, abort support, and optional CA file.

The provider override delegates to Pi's exported `openai-codex-responses` stream and replaces `SimpleStreamOptions.apiKey` with the in-memory lease token. This keeps Pi's model catalog and Codex wire implementation while avoiding its OAuth store and refresh path.

Do not rely on `before_provider_headers` to replace Codex auth: Pi 0.84.4's Codex adapter applies its `Authorization` and `chatgpt-account-id` headers after additional headers and derives account id from the JWT. A provider stream wrapper is the supported minimal seam.

Pi's built-in provider retry repeats a failed request with the same already-resolved options. The adapter must disable transport retries for broker-routed 401/429 and own a bounded pre-output replacement attempt. Do not promise post-output continuation in v1 unless an end-to-end test proves it against Pi 0.84.4; the source archive contains no prior relay implementation to preserve. On a streamed failure after meaningful output, fail clearly and never replay automatically.

Adapter configuration is only:

- `CODEX_BROKER_URL`;
- `CODEX_BROKER_CLIENT_KEY`;
- optional `CODEX_BROKER_CA_CERT` when the local CA is not installed in OS trust.

No refresh token, account list, selection policy, or quota state lives in Pi.

---

# 9. Hermes external patch: compatibility-gated

No Hermes adapter is implemented in this repository. Write `docs/integrations/hermes-agent-patch.md` for the maintainer of the live Hermes checkout and pin it to the audited `hermes-agent-2026.8.31` source snapshot.

The archived source has no uploaded `hermes-codex-pool` plugin to evolve. Native credential-pool ownership must not be used for broker leases.

Its current implementation imports internal Hermes modules such as `agent.credential_pool` / `hermes_cli.auth`, prompts for both access and refresh tokens, and creates native `PooledCredential` rows containing the refresh token. That is exactly the ownership model this rewrite removes.

The static audit found that a plugin-only implementation is **not safe** in this snapshot:

- `pre_llm_call` and request middleware are fail-open;
- `llm_execution.next_call` is deliberately single-use, so it cannot perform replacement-account replay;
- initialization still resolves Hermes's native Codex OAuth pool unless an explicit key is supplied;
- the built-in HTTP 401 branch invokes Hermes's own Codex credential refresh.

Therefore, `docs/integrations/hermes-agent-patch.md` specifies a small fail-closed core patch against exact audited source paths. No Hermes package is added here.

## External Hermes core patch

The external implementer must:

1. add `agent/codex_broker.py`, a synchronous HTTPS client and in-memory per-turn lease manager;
2. change all `openai-codex` branches in `hermes_cli/runtime_provider.py` to use a non-secret initialization sentinel in broker mode rather than native credentials;
3. attach the manager in `agent/agent_init.py` and disable the native Codex credential pool;
4. obtain one lease per `(session_id, turn_id)` in `agent/conversation_loop.py`, apply it by atomically rebuilding the Codex client, and reuse it for inner tool-loop requests;
5. use the existing `on_first_delta` callback as the no-replay boundary;
6. replace a pre-output 401/403/quota failure at most once through the existing outer retry loop;
7. wait on the broker's exact pool-reset timestamp and remain interruptible;
8. discard access-token state at every turn exit and preserve only the non-secret preferred account ID;
9. skip `_try_refresh_codex_client_credentials()` and every native Codex pool read/write while broker mode is active.

The patch document pins the source archive hash, lists concrete methods and source locations, defines tests, and provides a twelve-step live acceptance procedure. Ship only after those tests pass against the live Hermes revision.

## Existing Hermes plugin files to retire

Do not carry the following behavior into the new adapter:

- `codex_pool.py` credential import/persistence;
- `PooledCredential(... refresh_token=...)` creation;
- direct `write_credential_pool` mutation;
- `pre_credential_select` pool management;
- local per-account usage retrieval;
- account add/rename/remove commands.

The old repository can be archived after the new architecture is proven.

---

# 10. Exact user-turn and failure flows

## Normal turn

```text
User -> Pi/Hermes
Client -> Broker /route
Broker -> checks client key
Broker -> checks current account/usage state
Broker -> prefers prior account if still healthy
Broker -> CredentialAuthority ensures valid access token
Broker -> returns access token + account id
Client -> Codex directly
Codex -> streamed reply
Client -> User
```

## One account runs out of usage

```text
Client -> Codex
Codex -> quota/usage error
Client -> Broker /route(failed_account_id=A, failure_kind=quota)
Broker -> refresh/record usage for A
Broker -> excludes A
Broker -> picks B
Broker -> returns B access token
Client -> safely continues/retries according to its host's semantics
```

## Every account is exhausted

```text
Client -> Broker /route(... failure ...)
Broker -> no usable account
Broker -> earliest known reset + 10s padding
Broker -> 429 POOL_EXHAUSTED + next_retry_at + Retry-After
Client -> waits
Client -> Broker /route at next_retry_at
Broker -> refreshed account is usable
Client -> automatically resumes
```

## Access token expires / 401

```text
Client -> Broker /route(failed_account_id=A, failure_kind=auth)
Broker -> CredentialAuthority acquires A mutation lock
Broker -> re-reads ACTIVE credential
Broker -> broker-owned Codex runtime refreshes/checkpoints it once
Broker -> returns fresh access token
```

No Pi/Hermes refresh-token operation exists.

## Broker is down

Because the requirement says every new user turn must ask the broker for the current best account:

- an already-running turn may finish with its in-memory access token;
- a **new** user turn fails closed with a clear `broker unavailable` error and can retry when the broker returns;
- do not persist a long-lived fallback access token and silently bypass routing, because that undermines the single-authority behavior and client-key revocation.

Codex Broker remains a single point of coordination, intentionally. The failure mode is controlled availability loss rather than credential corruption.

---

# 11. UI changes

## Dashboard

Keep Orbit only. Add at most one small broker-oriented status area; do not redesign the entire UI in this rewrite.

Useful high-level fields:

- broker ready/unavailable;
- total usable accounts;
- next pool reset if all are exhausted;
- client key count/last-used activity link.

Existing account cards continue showing authentication and usage state. Remove activation state, next-activation controls, and ambiguity acknowledgment.

## Account detail

Keep existing usage/auth detail. Remove activation controls and password prompts. Rename re-login wording to `Sign in again`.

The auth.json download remains available to the logged-in administrator but is visually identified as a **manual external export**, not something Pi/Hermes should consume.

## Settings

Add **Client access keys** next to existing runtime/webhook configuration.

Creation UX:

1. enter name;
2. click Create;
3. one-time secret appears in a copyable field;
4. warning says it cannot be displayed again;
5. subsequent list shows only prefix/metadata.

Revoked key must fail `/api/v1/route` immediately.

---

# 12. Documentation cleanup and rename

## `README.md` — REWRITE

New opening:

> **Codex Broker** is a central Codex authentication, quota, and account-routing service. It keeps the only mutable copy of each account's OAuth credential, tracks Codex usage windows, and routes trusted local clients to an available account.

README sections:

1. What it solves — refresh-token ownership conflict.
2. Architecture diagram.
3. Install/upgrade from Windowkeeper without relogin.
4. HTTPS LAN setup.
5. Create client access key.
6. Install Pi adapter.
7. Hermes adapter only if compatibility-tested/shipped.
8. Removal of legacy automatic activation.
9. Backup/recovery.
10. Security limitations, especially issued access-token revocation.

## Other root docs — MODIFY/PRUNE

Rewrite branding/current behavior in:

- `PRODUCT.md`
- `OPERATIONS.md`
- `SECURITY.md`
- `.env.example`
- Compose examples
- release gates

Obsolete prototype/implementation plans should not remain as competing “current” designs. Either delete them or move only historically useful credential-integrity research under `docs/archive/windowkeeper/`.

Keep research about refresh-token forking/checkpointing because it explains why the architecture exists. Delete old five-dashboard-prototype documentation once Orbit is canonical.

Do not rewrite historical migration SQL merely to change branding.

---

# 13. Test plan / release gates

## Migration tests — highest priority

Create a fixture representing an actual pre-rewrite Windowkeeper installation:

- migrations 001-006 applied;
- vault sentinel created with old literal;
- one or more encrypted `ACTIVE` credentials;
- optional `EXPORT` credential;
- usage snapshots/accounts/settings.

Test that Codex Broker:

1. applies migrations 007-009;
2. removes activation tables/state without touching account or credential rows;
3. verifies the old sentinel;
4. decrypts all old ACTIVE credentials;
5. lists accounts;
6. serves an access-token lease;
7. performs a managed checkpoint;
8. does not request re-login.

Migration 009 is destructive only to removed activation history. Rollback to software expecting that schema requires restoring its automatic pre-v9 backup.

## Credential concurrency tests

- 50 concurrent `/route` calls for one account with a comfortably valid access token => no mutation/refresh.
- 50 concurrent `/route` calls when token is near expiry => exactly **one** refresh/checkpoint lineage mutation; every caller receives the newest token.
- concurrent usage poll + client refresh => serialized mutation; never two refresh-token writers.
- stale refresh/checkpoint may not overwrite a newer ACTIVE bundle.

## Routing tests

- every new-turn call runs router logic;
- preferred healthy account remains selected;
- failed account is skipped;
- next stable account is selected;
- disabled/deleted/no-ACTIVE/needs-login accounts are skipped;
- known 100% short or weekly window is unavailable;
- stale-but-not-known-exhausted telemetry does not unnecessarily take the whole broker down;
- all exhausted returns exact earliest reset + 10 seconds;
- `Retry-After` agrees with JSON timestamp;
- unknown reset fails explicitly rather than fabricating a time.

## Web/session tests

- login creates persistent cookies with `Max-Age`;
- idle lifetime = 31 days, absolute = 90 days;
- idle touch cannot extend beyond absolute limit;
- browser-admin mutation endpoints need session + CSRF, **not password**;
- logout revokes session;
- admin password change revokes sessions;
- only Orbit dashboard exists; variant query no longer changes layout.

## Client-key tests

- key secret is shown once;
- only hash/prefix stored;
- valid key routes;
- revoked key immediately receives 401;
- deleted/revoked state behaves consistently;
- one client cannot use browser session endpoints merely by possessing a client key;
- client key never appears in logbook/export.

## TLS tests

- loopback development can start according to explicit dev policy;
- non-loopback start without TLS fails;
- Pi/Hermes test client rejects untrusted cert;
- trusted local CA succeeds;
- `Secure` cookie is enabled in normal LAN deployment.

## Pi adapter tests

- one broker request per **user prompt**, not per tool-loop iteration;
- broker can return same account on successive turns;
- fresh decision still occurs every turn;
- pre-output quota failover retries with new lease;
- post-output failure surfaces clearly and is never replayed automatically;
- pool-exhausted waits until broker timestamp and auto-resumes;
- revoked broker key fails clearly;
- no refresh token exists in adapter files/state/logs.

## Hermes compatibility gate

Live test against a pinned current Hermes revision/version:

- initializes without a broker-external refresh token;
- broker called once per user turn;
- middleware changes actual Codex auth header;
- multi-tool turn keeps one turn lease unless failure occurs;
- quota failure reroutes and resumes safely;
- all-exhausted wait/resume works;
- no private `agent.*` credential mutation imports.

If any gate fails, do not ship the Hermes adapter.

---

# 14. Implementation sequence

Implementation status at the current branch:

- phases 0-6 are implemented by the incremental commits after `d4d336b`;
- the Pi package exists at `packages/pi-extension/` and passes its Node tests and type check;
- the Hermes work is intentionally specification-only at `docs/integrations/hermes-agent-patch.md` pending live-checkout validation;
- migrations 007-009 add client routing and remove legacy activation state while preserving migrations 001-006 for direct upgrades;
- the synthetic pre-rewrite migration fixture proves sentinel/credential compatibility and managed checkpointing without relogin;
- a production data-volume copy drill and the live Hermes gate remain release operations because those external checkouts/data are not in this repository.

## Phase 0 — freeze behavior + Hermes spike

1. Add migration fixture/backups and record current baseline tests.
2. Complete the Hermes static source audit and write the external live-spike procedure.
3. Do not create `adapters/hermes/`; live proof and any Hermes core patch happen in the external checkout.

## Phase 1 — rename safely

1. Rename Python package/repository/README/visible strings to Codex Broker.
2. Preserve crypto sentinels, volume, DB, lock, cookies, and credential schema compatibility.
3. Ensure old data boots before adding new broker behavior.

## Phase 2 — simplify administrator UI

1. Set 31-day idle / 90-day absolute sessions.
2. Add persistent cookie expiry.
3. Delete browser step-up reauthentication code and fields.
4. Delete four dashboard variants and switcher; keep Orbit.
5. Run web/security tests.

## Phase 3 — central broker core

1. Add migrations 007-008.
2. Add `client_auth.py` + Settings key UI.
3. Add `credential_authority.py` over existing ACTIVE/checkpoint machinery.
4. Add `router.py` with stable preferred/next-available selection.
5. Add `/api/v1/route` and health.
6. Add exact reset timestamp + padding behavior.
7. Add direct HTTPS LAN configuration and startup guard.
8. Run concurrency/migration/security tests before touching client adapters.

## Phase 4 — Pi adapter

1. Create the small Pi package in `packages/pi-extension/` against 0.84.4.
2. Add no local vault, refresh, account pool, or persistent access token.
3. Add broker HTTPS client and per-user-turn in-memory lease.
4. Implement bounded pre-output failover and fail-closed post-output behavior.
5. Test against the broker with a trusted local CA.

## Phase 5 — Hermes patch specification

1. Write `docs/integrations/hermes-agent-patch.md` against the audited source paths and public contracts.
2. Specify no native credential pool or refresh-token persistence.
3. Specify per-turn broker lease, request-header injection, failure reporting, and bounded retry/wait behavior.
4. Include live compatibility commands and pass/fail gates for the external implementer.
5. Do not create `adapters/hermes/` in this repository.

## Phase 6 — cleanup/release

1. Remove automatic activation routes, services, scheduler, UI, tests, and Codex model-turn adapter methods.
2. Apply migration 009 to drop activation tables and `account_state.activation_state` without rewriting historical migrations.
3. Rewrite docs.
4. Archive/delete obsolete Windowkeeper and old plugin planning docs.
5. Verify no old product-name strings remain except intentional storage/crypto compatibility identifiers.
6. Run full Python and Pi tests/type checks.
7. Test upgrade from a copy of the real existing data volume (external release drill; synthetic 001-006 fixture is automated here).
8. Cut the first Codex Broker release only after zero-relogin migration is proven (release not performed by this implementation task).

---

# 15. Devil's-advocate risks

| Risk | Decision |
| --- | --- |
| Broker becomes a single point of failure | Accept. New turns fail cleanly instead of risking credential corruption. Existing in-flight turn can finish. HA is YAGNI. |
| Client-key revocation cannot revoke an already-issued OpenAI access token | Accept/document for v1. A full proxy would solve it but adds major data-plane complexity. |
| Cached usage can be a few minutes stale | Accept. Background polling stays; force refresh on reported quota failure. No fake reservation system. |
| Multiple simultaneous clients can exhaust one account together | Accept/reactively reroute. Requests are concurrent; refresh-token mutations remain serialized. |
| Switching accounts harms prompt caching | Prefer prior healthy account on each fresh turn rather than round-robin. |
| Removing browser re-password prompts weakens protection against a stolen admin session | Explicit user choice. Retain TLS, long random session tokens, CSRF, Secure/HttpOnly cookies, expiry, logout. |
| 90-day absolute session is longer than conventional high-security admin interfaces | Explicit convenience choice for a private home-LAN management service. Keep a 31-day inactivity cutoff. |
| Renaming crypto strings could strand every credential | Never rename legacy KDF/sentinel strings. Test migration before release. |
| Renaming Docker volume can make data appear lost | Preserve physical `windowkeeper-data` volume name. |
| Hermes plugin API may still be insufficient for retry/resume | Compatibility gate. Omit plugin instead of shipping a brittle private-internals hack. |
| A full project rewrite could accidentally discard Windowkeeper's strong checkpoint/recovery code | Refactor around existing vault/services rather than rewriting the credential engine from scratch. |

---

# 16. Source-quality / research notes

Primary sources were preferred over blogs/issues wherever possible.

### OpenAI Codex App Server

Official documentation: <https://developers.openai.com/codex/app-server/>

Relevant current behavior:

- `chatgptAuthTokens` is intended for host applications that own ChatGPT authentication lifecycle;
- host supplies `accessToken` + `chatgptAccountId`;
- App Server can request a new host-managed access token after 401 and retry;
- `account/rateLimits/read` reports usage windows and `resetsAt` timestamps.

**CRAAP:** current documentation, directly relevant, first-party authority, implementation-level examples, informational purpose.

### OAuth refresh-token rotation

IETF RFC 9700: <https://www.rfc-editor.org/rfc/rfc9700.html>

It describes refresh-token rotation as issuing a new token on refresh and invalidating the prior token to detect replay. This is the core reason multiple independent refresh-token owners are structurally unsafe.

**CRAAP:** current Best Current Practice, directly relevant, standards authority, normative technical source, noncommercial purpose.

### Bearer-token transport

IETF RFC 6750: <https://www.rfc-editor.org/rfc/rfc6750.html>  
OWASP TLS Cheat Sheet: <https://cheatsheetseries.owasp.org/cheatsheets/Transport_Layer_Security_Cheat_Sheet.html>

Bearer tokens require confidentiality/integrity in transport; OWASP recommends TLS for all authenticated/sensitive pages and API communication.

**CRAAP:** standards + recognized security guidance, directly relevant, authoritative, technically verifiable, security guidance purpose.

### Pi extension API

Current upstream extension documentation: <https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md>

Relevant behavior:

- `before_agent_start` runs after the user's prompt and before the agent loop;
- `agent_start` / `agent_end` are once per user prompt;
- inner turns repeat during tool use;
- extensions can register/override providers.

This supports one fresh broker lease per user turn while preserving the same lease through the inner tool loop.

**CRAAP:** current upstream source documentation, directly relevant, project authority, corroborated by source, engineering documentation purpose.

### Hermes plugin/middleware APIs

Official/current sources:

- <https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks/>
- <https://github.com/NousResearch/hermes-agent/blob/main/docs/middleware/README.md>
- <https://hermes-agent.nousresearch.com/docs/user-guide/features/credential-pools/>

Relevant behavior:

- `pre_llm_call` runs once per turn;
- `llm_request` can replace provider request kwargs;
- `llm_execution` wraps provider execution;
- middleware receives turn/session/provider context;
- `fill_first` is the default credential-pool strategy;
- Hermes warns that credential rotation resets prompt cache.

These make a broker plugin plausible, but implementation remains compatibility-gated because initialization and safe error/resume behavior must be demonstrated, not assumed.

**CRAAP:** current official/project docs and source, directly relevant, upstream authority, testable against code, engineering documentation purpose.

### Uvicorn HTTPS

Uvicorn settings/deployment documentation supports binding to a LAN host and supplying certificate/key files directly. This is sufficient for the deliberately small home-LAN deployment and avoids another reverse-proxy service.

---

# 17. Definition of done

The rewrite is done only when all of these statements are true:

- Existing Windowkeeper accounts appear in Codex Broker after upgrade with **no account relogins**.
- Only Codex Broker ever persists or rotates a refresh token.
- Pi has no refresh tokens and asks the broker on every user turn.
- Hermes has no refresh tokens if its public-plugin compatibility gate passes; otherwise no Hermes package is shipped yet.
- Concurrent requests cannot fork a refresh-token lineage.
- Quota exhaustion reroutes to another account automatically.
- Total pool exhaustion returns an exact padded retry timestamp and clients wait/resume automatically.
- Browser login normally survives restarts and ordinary use for at least a month.
- Logged-in administrator actions no longer ask for the password again.
- Orbit is the only dashboard layout.
- Client access keys can be created, viewed once, independently revoked, and never stored/logged in plaintext by the broker.
- Broker/client traffic on the LAN is HTTPS and non-loopback plaintext startup is rejected.
- The product is consistently named **Codex Broker**, except intentional legacy on-disk cryptographic/storage identifiers.
