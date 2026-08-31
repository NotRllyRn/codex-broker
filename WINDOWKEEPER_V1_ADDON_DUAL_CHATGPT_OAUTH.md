# Windowkeeper v1 Add-On Plan: Dual ChatGPT OAuth Sign-In

> **Implementation update:** Windowkeeper now pins and installs its supported Codex release inside the image. Operator-supplied Codex package, version-output, and SHA-256 settings described below are superseded and are not required.

**Document type:** Add-on feature plan  
**Applies to:** `WINDOWKEEPER_V1_DETAILED_IMPLEMENTATION_PLAN.md`  
**Research date:** July 26, 2026  
**Status:** Recommended for v1, subject to the proof-of-concept and release gates in this document  
**Feature:** Two ChatGPT OAuth enrollment methods for each Windowkeeper account

1. **Sign in with browser** — the normal ChatGPT browser-based OAuth flow.
2. **Sign in with a code** — the Codex device-code flow for Docker, SSH, headless, and remote-browser environments.

This document is an additive implementation specification. It does not replace the main Windowkeeper implementation plan. Where this document conflicts with an older authentication-flow or callback-forwarding proposal, this add-on is authoritative for ChatGPT OAuth enrollment.

---

## 1. Executive decision

Windowkeeper v1 should support two user-visible ChatGPT sign-in choices:

| User-visible choice | Internal identifier | Official app-server login type | Recommended use |
| --- | --- | --- | --- |
| **Sign in with browser** | `CHATGPT_BROWSER` | `chatgpt` | A browser can reach the Codex loopback callback, or the operator can use the secure manual callback-forwarding fallback. |
| **Sign in with a code** | `CHATGPT_DEVICE_CODE` | `chatgptDeviceCode` | Default for Docker, remote servers, SSH, NAS devices, and browsers running on another computer. |

Both choices authenticate the same class of ChatGPT/Codex account. They are two enrollment transports, not two long-term credential types. After successful enrollment, both produce an account whose upstream authentication mode is `chatgpt`, whose refreshed credential state is owned by the pinned Codex runtime, and whose credential bundle is checkpointed into Windowkeeper's encrypted vault.

### Primary architecture decision

Windowkeeper must **delegate both OAuth flows to the official, pinned Codex app-server**:

```text
account/login/start { type: "chatgpt" }
account/login/start { type: "chatgptDeviceCode" }
```

Windowkeeper must not independently implement OpenAI's authorization endpoint, OAuth client identifier, PKCE generation, token exchange, refresh-token rotation, account routing, or token serialization.

The official app-server already:

- generates the browser authorization URL;
- creates and validates PKCE and OAuth state;
- hosts the browser callback handler;
- implements the device-code workflow;
- exchanges authorization codes;
- stores the resulting credential state;
- refreshes access tokens;
- emits completion and account-update notifications.

Windowkeeper's responsibility is orchestration, safe presentation of the interaction data, account isolation, lifecycle persistence, verification, encrypted checkpointing, cancellation, and recovery.

### Default choice

The dashboard and CLI should preselect **Sign in with a code** unless a deployment preflight proves that automatic browser callback delivery is available.

This is not because device-code OAuth is a lesser flow. It is the most reliable option for Windowkeeper's Docker-first and LAN-oriented deployment model because browser OAuth redirects to a loopback callback on the machine where Codex is running.

---

## 2. Scope and non-goals

### 2.1 Included

This add-on includes:

- browser OAuth enrollment through the official app-server;
- device-code OAuth enrollment through the official app-server;
- an enrollment-method selector in the dashboard and CLI;
- a shared login-attempt state machine;
- an automatic browser callback path when loopback reachability exists;
- a secure manual callback-forwarding fallback for Docker bridge and remote-browser deployments;
- cancellation, expiration, restart, and retry behavior;
- account verification after OAuth completion;
- encrypted credential checkpointing;
- reauthentication and credential replacement through either method;
- complete redaction and session-binding rules;
- proof-of-concept, automated, and protected real-account tests.

### 2.2 Excluded

This add-on does not introduce:

- an independent OpenAI OAuth client implementation;
- editable OAuth endpoints, scopes, client identifiers, or redirect URIs;
- a generic OAuth framework for arbitrary providers;
- shared credentials between Windowkeeper accounts;
- importing another person's active credentials;
- copying one active `auth.json` into multiple account runtimes;
- API-key login as a substitute for ChatGPT-plan OAuth;
- personal access-token support;
- public callback endpoints on the LAN or internet;
- a permanent callback proxy;
- browser automation or storing ChatGPT passwords;
- persistent storage of authorization URLs, user codes, authorization codes, PKCE values, or OAuth state.

---

## 3. Research findings

## 3.1 Official Codex app-server behavior

The current official Codex app-server documentation exposes both managed ChatGPT login methods through `account/login/start`:

```json
{
  "method": "account/login/start",
  "id": 100,
  "params": {
    "type": "chatgpt"
  }
}
```

The browser response contains a login identifier and authorization URL:

```json
{
  "id": 100,
  "result": {
    "type": "chatgpt",
    "loginId": "<uuid>",
    "authUrl": "https://..."
  }
}
```

The device-code method is started with:

```json
{
  "method": "account/login/start",
  "id": 101,
  "params": {
    "type": "chatgptDeviceCode"
  }
}
```

The returned device interaction includes a login identifier, verification URL, and user code. Completion is delivered through `account/login/completed`; the resulting authenticated account is announced through `account/updated` and can be verified through `account/read`.

Codex owns token persistence and automatic refresh for both managed ChatGPT flows. This makes the app-server the correct boundary for Windowkeeper.

**Primary source:**  
<https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>

## 3.2 Browser callback behavior

The official Codex browser flow uses a local loopback callback server. Current source defines:

```text
Default callback port:  1455
Fallback callback port: 1457
Callback path:          /auth/callback
Callback host:          localhost / loopback
```

The callback server is intentionally local. It is not a normal LAN-facing HTTP endpoint. This creates a reachability mismatch when:

- Codex runs inside a Docker bridge network;
- Codex runs over SSH on a remote host;
- the browser runs on a different computer;
- multiple Codex clients contend for port 1455;
- a development environment silently remaps the port.

The app-server documentation allows `useHostedLoginSuccessPage: true` and `appBrand: "codex"` or `"chatgpt"`, but these options only change the success page after the local callback has been received. They do not eliminate the loopback callback requirement.

**Primary sources:**
<https://github.com/openai/codex/blob/main/codex-rs/login/src/server.rs>
<https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>

## 3.3 Pi Agent findings

Pi's OpenAI/Codex login UX presents two explicit choices:

- browser login;
- device-code login for headless use.

Pi directly implements OAuth, including PKCE, state validation, callback handling, token exchange, refresh, device-code polling, and credential storage. It also contains a useful manual fallback pattern: when a browser cannot deliver the loopback callback, the user can paste the callback URL or authorization result back into the client.

Windowkeeper should adopt Pi's **UX pattern**, not its OAuth ownership model. Windowkeeper already depends on the official Codex runtime, so duplicating OAuth would create two implementations of token refresh and credential serialization.

**Primary sources:**
<https://github.com/earendil-works/pi>
<https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/providers.md>
<https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/custom-provider.md>

## 3.4 OpenCode findings

OpenCode has exposed both:

- `ChatGPT Pro/Plus (browser)`;
- `ChatGPT Pro/Plus (headless)`.

Its source directly implements the browser PKCE flow and device-code polling. The design confirms that both methods can lead to the same stored OAuth credential format.

OpenCode's issue history also demonstrates the maintenance cost of owning this integration directly. Reported failures have included token-exchange errors, callback-port conflicts, and regressions where the OAuth choices disappeared from the authentication menu even while code remained in the binary.

Windowkeeper should use this as evidence for a narrow adapter around the official app-server rather than copying OpenCode's OAuth client.

**Primary source:**  
<https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/plugin/codex.ts>

## 3.5 Hermes findings

Current Hermes code directly implements Codex device-code login and stores its own resulting OAuth session. Hermes documentation also supports importing existing Codex CLI credentials produced in `~/.codex/auth.json`.

The current public Hermes implementation does not provide the same fully integrated direct browser-plus-device selector for Codex that Pi and OpenCode expose. Browser-authenticated Codex credentials are normally produced by the official `codex login` flow and then reused or imported. A direct browser method has appeared as a requested or evolving feature rather than a stable documented Hermes behavior.

This distinction matters: Hermes validates device-code support and credential reuse, but it should not be cited as proof that its current main branch owns both direct flows identically.

Windowkeeper should not copy/import an arbitrary active Codex credential file by default because Windowkeeper's account-isolation model requires one credential lineage to have one owner. The useful Hermes lesson is that browser and device-code sessions ultimately produce compatible Codex credential state.

**Primary sources:**
<https://github.com/NousResearch/hermes-agent/blob/main/hermes_cli/auth.py>
<https://github.com/NousResearch/hermes-agent/blob/main/website/docs/integrations/providers.md>
<https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/codex-app-server-runtime.md>

## 3.6 Comparative conclusion

| Implementation | Browser flow | Device-code flow | Own token exchange/refresh? | Windowkeeper lesson |
| --- | ---: | ---: | ---: | --- |
| Official Codex app-server | Yes | Yes | Official runtime owns it | **Use as production boundary.** |
| Pi Agent | Yes | Yes | Yes | Reuse login-method selector and manual callback fallback UX. |
| OpenCode | Yes | Yes | Yes | Confirms dual-flow UX; reported regressions show integration-maintenance risk. |
| Hermes | Via official Codex credential reuse/import | Yes, directly | Mixed | Confirms device-code and common credential outcome; do not claim identical direct browser support. |

---

## 4. Final feature contract

## 4.1 One account, one authentication lineage

A Windowkeeper account has one encrypted Codex credential bundle. The operator may initially enroll or later reauthenticate that account using either browser OAuth or device-code OAuth.

Changing the login method does not create a second credential set. A successful reauthentication replaces the account's prior credential generation through the existing crash-safe credential replacement process.

```text
Windowkeeper account UUID
  -> one active credential generation
  -> one isolated CODEX_HOME at runtime
  -> one authenticated ChatGPT identity/workspace
  -> one selected enrollment method per login attempt
```

## 4.2 Method is history, not auth mode

Persist these as separate concepts:

```text
upstream_auth_mode = chatgpt
login_method       = CHATGPT_BROWSER | CHATGPT_DEVICE_CODE
```

The first indicates the current Codex authentication class. The second records how the operator completed the most recent login.

Do not branch usage, scheduling, activation, or refresh-token behavior based on the login method after enrollment succeeds.

## 4.3 Login completion is not enrollment completion

An `account/login/completed` success notification means the upstream OAuth operation completed. Windowkeeper must not mark the account ready until all post-login checks succeed:

1. `account/read` reports authenticated ChatGPT state.
2. The account/workspace matches the enrollment restriction, when configured.
3. `account/rateLimits/read` succeeds or returns a classified account-side limitation that is permitted by the account contract.
4. The runtime is quiesced for checkpointing.
5. The refreshed credential file passes format and permission validation.
6. The credential bytes are encrypted and committed.
7. The plaintext runtime is cleaned.
8. The account state and login attempt are committed as complete.

---

## 5. User experience

## 5.1 Dashboard method selector

The Add Account and Reauthenticate dialogs should show:

### Sign in with a code — Recommended

Description:

> Open the displayed OpenAI page on any device, enter the one-time code, and return here. Best for Docker, NAS, SSH, and remote servers.

### Sign in with browser

Description:

> Open the normal ChatGPT sign-in page. Automatic completion requires a browser that can reach the Codex localhost callback. A secure copy-and-paste callback fallback is available when it cannot.

The method selector must not mention API keys or access tokens as equivalent options.

## 5.2 Browser method preflight

Before enabling the final **Continue** button for browser OAuth, show a deployment-specific result:

```text
Automatic callback available
```

or:

```text
Manual callback forwarding will be required
```

or:

```text
Browser OAuth unavailable in this deployment; use code sign-in
```

Preflight is based on configured deployment mode, not unreliable browser probing.

## 5.3 Interaction visibility

OAuth interaction data is visible only:

- to the administrator session that started the attempt;
- through a dedicated no-store interaction endpoint;
- while the attempt is in `WAITING_FOR_USER`;
- until consumed, expired, cancelled, or completed.

It must not be included in general account resources, operation resources, SSE replay events, webhook events, diagnostics archives, or logs.

## 5.4 Multiple tabs

A second tab in the same administrator session may display ordinary operation status, but it must not automatically receive the authorization URL or user code.

The initiating tab receives a one-time interaction capability bound to:

```text
admin_session_hash
login_attempt_id
interaction_nonce_hash
expiration
```

The operator may deliberately reveal the interaction in another tab only by restarting the login or using an explicit in-session **Transfer interaction to this tab** action that rotates the nonce.

---

## 6. Shared login-attempt state machine

Use one durable state machine for both methods.

```text
CREATED
  -> STARTING_RUNTIME
  -> STARTING_LOGIN
  -> WAITING_FOR_USER
  -> OAUTH_COMPLETED
  -> VERIFYING_ACCOUNT
  -> QUIESCING_RUNTIME
  -> CHECKPOINTING_CREDENTIAL
  -> COMPLETED
```

Terminal or exceptional states:

```text
CANCEL_REQUESTED
CANCELLED
EXPIRED
FAILED_RETRYABLE
FAILED_ACTION_REQUIRED
RESTART_REQUIRED
SUPERSEDED
```

## 6.1 State definitions

### `CREATED`

The durable login attempt and parent operation are committed. No Codex process has been started.

### `STARTING_RUNTIME`

Windowkeeper is reconstructing the isolated account runtime and starting the pinned app-server.

### `STARTING_LOGIN`

The app-server has initialized. Windowkeeper is invoking `account/login/start` with the selected method.

### `WAITING_FOR_USER`

The app-server returned an interaction. For browser login, this is the authorization URL. For device login, this is the verification URL and one-time code.

### `OAUTH_COMPLETED`

A matching `account/login/completed` success notification arrived. Credentials may still exist only in the temporary runtime and are not yet a durable Windowkeeper credential.

### `VERIFYING_ACCOUNT`

Windowkeeper is reading account identity, plan metadata, workspace restriction, and initial rate-limit state.

### `QUIESCING_RUNTIME`

The account runtime is prevented from mutating the credential file while checkpointing is prepared.

### `CHECKPOINTING_CREDENTIAL`

The new credential generation is encrypted and committed using the vault's two-generation replacement mechanism.

### `COMPLETED`

The account's active credential generation and authentication state are durable. The login interaction is destroyed.

### `CANCEL_REQUESTED`

The operator requested cancellation. Windowkeeper has stopped accepting callback forwarding and sent `account/login/cancel` for the matching upstream login identifier.

### `CANCELLED`

The attempt is terminal and produced no active credential replacement.

### `EXPIRED`

The upstream or Windowkeeper login deadline expired before completion.

### `FAILED_RETRYABLE`

A local runtime or transport error occurred before credentials were accepted. A fresh attempt may be started.

### `FAILED_ACTION_REQUIRED`

The upstream account, workspace policy, or verification result requires administrator action.

### `RESTART_REQUIRED`

Windowkeeper restarted while the attempt depended on transient authorization interaction. OAuth attempts are not resumed across service restart.

### `SUPERSEDED`

A newer login attempt replaced an older queued or incomplete attempt for the same account before upstream completion.

## 6.2 Concurrency invariants

- At most one nonterminal login attempt per account.
- At most one browser OAuth attempt globally by default.
- Device-code attempts may run for different accounts up to `WINDOWKEEPER_AUTH_CONCURRENCY`.
- Login cannot overlap activation, usage refresh, credential replacement, deletion, or another account-scoped Codex RPC for the same account.
- A browser callback may satisfy only the exact active `login_attempt_id` and upstream `loginId`.
- A completed login cannot activate an account if the attempt was cancelled or superseded before checkpoint commit.

---

## 7. Persistence changes

## 7.1 `login_attempts` table

Add a dedicated durable table rather than encoding login state only in generic operation metadata.

```sql
CREATE TABLE login_attempts (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id) ON DELETE CASCADE,

    method TEXT NOT NULL CHECK (
        method IN ('CHATGPT_BROWSER', 'CHATGPT_DEVICE_CODE')
    ),

    state TEXT NOT NULL CHECK (
        state IN (
            'CREATED',
            'STARTING_RUNTIME',
            'STARTING_LOGIN',
            'WAITING_FOR_USER',
            'OAUTH_COMPLETED',
            'VERIFYING_ACCOUNT',
            'QUIESCING_RUNTIME',
            'CHECKPOINTING_CREDENTIAL',
            'COMPLETED',
            'CANCEL_REQUESTED',
            'CANCELLED',
            'EXPIRED',
            'FAILED_RETRYABLE',
            'FAILED_ACTION_REQUIRED',
            'RESTART_REQUIRED',
            'SUPERSEDED'
        )
    ),

    upstream_login_id TEXT,
    initiating_session_hash BLOB NOT NULL,
    interaction_nonce_hash BLOB,

    callback_port INTEGER,
    callback_mode TEXT CHECK (
        callback_mode IS NULL OR callback_mode IN (
            'AUTOMATIC_LOOPBACK',
            'MANUAL_FORWARD'
        )
    ),

    requested_at_ms INTEGER NOT NULL,
    started_at_ms INTEGER,
    interaction_expires_at_ms INTEGER,
    oauth_completed_at_ms INTEGER,
    completed_at_ms INTEGER,

    expected_workspace_id TEXT,
    observed_email TEXT,
    observed_plan_type TEXT,

    error_code TEXT,
    error_summary TEXT,

    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
```

The following values must never be stored in this table:

- browser authorization URL;
- device verification URL when it contains sensitive query data;
- user code;
- authorization code;
- returned callback URL;
- OAuth state;
- PKCE verifier or challenge;
- access token;
- refresh token;
- raw app-server login payload.

## 7.2 Unique nonterminal attempt

Enforce one active attempt per account using a partial unique index:

```sql
CREATE UNIQUE INDEX uq_login_attempt_active_account
ON login_attempts(account_id)
WHERE state IN (
    'CREATED',
    'STARTING_RUNTIME',
    'STARTING_LOGIN',
    'WAITING_FOR_USER',
    'OAUTH_COMPLETED',
    'VERIFYING_ACCOUNT',
    'QUIESCING_RUNTIME',
    'CHECKPOINTING_CREDENTIAL',
    'CANCEL_REQUESTED'
);
```

## 7.3 Account metadata

Add or expose:

```text
preferred_login_method
last_successful_login_method
last_login_at
last_login_attempt_id
```

`preferred_login_method` is a UX preference. It must not prevent selecting the other method.

## 7.4 Transient interaction store

Maintain an in-memory map owned by the authentication service:

```text
login_attempt_id -> LoginInteraction
```

Conceptual shape:

```python
@dataclass
class LoginInteraction:
    attempt_id: str
    session_hash: bytes
    nonce_hash: bytes
    method: LoginMethod
    expires_at_monotonic: float

    auth_url: str | None
    verification_url: str | None
    user_code: str | None

    expected_callback_scheme: str | None
    expected_callback_host: str | None
    expected_callback_port: int | None
    expected_callback_path: str | None
    expected_state_hash: bytes | None

    consumed: bool = False
```

This store is intentionally lost on restart. Startup converts corresponding nonterminal attempts to `RESTART_REQUIRED` and cleans any temporary runtime.

---

## 8. Device-code login implementation

## 8.1 Start sequence

1. Authenticate the administrator and validate CSRF.
2. Require recent-password reauthentication when replacing an existing credential.
3. Validate that the account has no nonterminal login attempt.
4. Insert the parent `Operation` and `login_attempts` row in one transaction.
5. Submit the account actor command.
6. Reconstruct the isolated runtime without any previous credential for first enrollment, or in a separate candidate generation for reauthentication.
7. Start and initialize the pinned app-server.
8. Invoke:

```json
{
  "method": "account/login/start",
  "id": "<rpc-id>",
  "params": {
    "type": "chatgptDeviceCode"
  }
}
```

1. Validate the response against the generated stable schema.
2. Store only the safe upstream login identifier and expiration metadata durably.
3. Put the verification URL and user code in the transient interaction store.
4. Transition to `WAITING_FOR_USER`.
5. Send a non-sensitive SSE event indicating that interaction is ready.

## 8.2 Dashboard presentation

Display:

- verification URL as a button and copyable text;
- user code with grouping exactly as returned;
- expiration countdown based on an absolute server timestamp;
- status such as `Waiting for authorization`;
- Cancel button.

Do not encode the user code into a QR code unless the official response includes a complete verification URL that already embeds the code. The plain URL and code must always remain visible.

The browser page must use:

```http
Cache-Control: no-store
Pragma: no-cache
Referrer-Policy: no-referrer
Content-Security-Policy: default-src 'self'; ...
```

JavaScript must render the values through `textContent`, not HTML insertion.

## 8.3 Completion

The authentication service listens for:

```json
{
  "method": "account/login/completed",
  "params": {
    "loginId": "<matching-login-id>",
    "success": true,
    "error": null
  }
}
```

Notifications with a different login ID are treated as account-isolation violations and must not mutate this attempt.

After completion, destroy the user code and verification URL before beginning account verification.

## 8.4 Device-flow errors

Classify:

| Condition | Windowkeeper result |
| --- | --- |
| User has not approved yet | Remain `WAITING_FOR_USER`; app-server owns polling. |
| User denies authorization | `FAILED_ACTION_REQUIRED` with a sanitized denial message. |
| Code expires | `EXPIRED`. |
| Device-code authentication disabled by workspace/account policy | `FAILED_ACTION_REQUIRED`; recommend browser OAuth. |
| App-server exits before interaction | `FAILED_RETRYABLE`. |
| App-server exits after approval but before Windowkeeper verification | Recover only if the resulting credential file is complete and the pinned proof-of-concept establishes safe adoption; otherwise `RESTART_REQUIRED`. |
| Administrator cancels | `CANCEL_REQUESTED` -> app-server cancel -> `CANCELLED`. |

Windowkeeper does not implement the device polling interval or `slow_down` behavior. The official app-server owns it.

---

## 9. Browser OAuth implementation

## 9.1 Start request

After the shared start sequence, invoke:

```json
{
  "method": "account/login/start",
  "id": "<rpc-id>",
  "params": {
    "type": "chatgpt",
    "useHostedLoginSuccessPage": true,
    "appBrand": "codex"
  }
}
```

The optional hosted-page fields must be sent only when they exist in the stable generated schema for the pinned Codex version. Otherwise omit them.

Validate the returned object and place `authUrl` only in the transient interaction store.

## 9.2 Authorization URL validation

Before presenting the URL:

- require HTTPS;
- reject username/password URL components;
- reject fragments;
- impose a maximum encoded length, initially 16 KiB;
- parse `redirect_uri` from the query;
- require the callback scheme to be `http`;
- require the callback host to be `localhost` or `127.0.0.1` as allowed by the pinned release;
- require the callback path to be `/auth/callback`;
- require the callback port to be in the release-tested allowlist, initially `{1455, 1457}`;
- extract and hash the OAuth `state` value for later manual-forward validation;
- retain the complete authorization URL only in memory.

If any value differs from the pinned compatibility manifest, fail closed with `CODEX_BROWSER_AUTH_CONTRACT_CHANGED` rather than guessing.

## 9.3 Callback delivery modes

Browser OAuth has two supported delivery modes.

### Mode A: automatic loopback callback

Use this when the Codex app-server shares the network namespace containing the browser-visible loopback listener.

The supported v1 deployment is an optional Linux host-network Compose profile:

```yaml
services:
  windowkeeper:
    network_mode: host
    # No ports: mapping when host networking is active.
```

In this profile:

- the Windowkeeper service still binds the dashboard according to its configured address and port;
- the child app-server's `127.0.0.1:1455` or `127.0.0.1:1457` listener exists on the Docker host loopback;
- a browser running on the Docker host reaches the callback directly;
- a browser on another machine may use SSH local port forwarding to the Docker host.

Example:

```bash
ssh -N \
  -L 1455:127.0.0.1:1455 \
  -L 1457:127.0.0.1:1457 \
  operator@windowkeeper-host
```

Then the remote browser's `localhost` callback traverses the SSH tunnel to the host-network callback server.

Host-network mode is optional because it weakens Docker network isolation compared with the default bridge profile. It must be explicitly selected and documented.

### Mode B: secure manual callback forwarding

Use this in the normal Docker bridge deployment when the browser cannot reach the app-server's container-local loopback listener.

Sequence:

1. Windowkeeper displays the app-server authorization URL.
2. The operator completes ChatGPT sign-in in the browser.
3. The browser attempts to open a URL shaped like:

```text
http://localhost:1455/auth/callback?code=...&state=...
```

1. If the browser cannot connect, it leaves the callback URL in the address bar.
2. Windowkeeper displays a secure text field labeled **Paste the full callback URL**.
3. The operator copies the complete URL from the browser and submits it to Windowkeeper.
4. Windowkeeper validates the submitted URL against the active attempt.
5. Windowkeeper makes a one-time internal loopback request to the app-server callback listener.
6. The app-server validates state, exchanges the authorization code, stores credentials, and emits the normal completion notification.
7. Windowkeeper destroys the submitted URL immediately.

This is a controlled version of the established remote/headless workaround where the failed callback request is replayed on the machine where Codex is listening. Pi also uses a manual return-value fallback for browser OAuth.

## 9.4 Manual callback validation

The callback-forward endpoint must accept only an authenticated POST body. Never accept callback data in the Windowkeeper page URL or query string.

Validation steps:

1. Require the active login attempt to be `CHATGPT_BROWSER` and `WAITING_FOR_USER`.
2. Require the requesting administrator session to match the initiating session.
3. Require a valid interaction nonce.
4. Limit the body to 16 KiB.
5. Parse the value as one absolute URL.
6. Reject URL credentials and fragments.
7. Require exact scheme, normalized host, port, and path from the expected redirect URI.
8. Require `code` to be present once.
9. Require `state` to be present once.
10. Compare SHA-256 of the submitted state against the expected state hash using constant-time comparison.
11. Reject duplicate query keys for `code`, `state`, or `error`.
12. Reject a callback already submitted for this attempt.
13. Mark the transient callback capability consumed before making the internal request.

The internal request must use a dedicated local client:

```text
Destination:     exact expected loopback callback URL only
Redirects:       disabled
Environment proxy: disabled
.netrc:          disabled
Cookies:         none
Connect timeout: 2 seconds
Read timeout:    5 seconds
Maximum response body: 16 KiB
Logging:         method/status only; never URL or headers
```

A non-2xx or non-3xx callback response does not automatically reveal the upstream error to the operator because it may contain sensitive detail. Store only an allowlisted error code and sanitized summary.

The matching `account/login/completed` notification remains canonical. The callback-forward HTTP response by itself is not proof of login completion.

## 9.5 Why an in-container same-port bridge is not the default

Do not implement a userspace process that tries to bind `0.0.0.0:1455` in the same network namespace while Codex is already bound to `127.0.0.1:1455`. On normal Linux socket semantics, the wildcard bind conflicts with the existing specific-address bind unless unsafe or nonportable socket behavior is introduced.

Also, a Docker published port normally targets the container's network interface, not a service listening only on the container loopback interface.

Therefore the reliable v1 choices are:

- host-network mode for automatic loopback delivery;
- SSH forwarding when the browser is remote;
- secure manual callback forwarding in ordinary bridge mode;
- device-code login as the recommended default.

## 9.6 Browser cancellation and port conflicts

Only one browser OAuth attempt should be active globally because the official callback server uses a small fixed port set.

Before starting browser OAuth:

- acquire the global browser-login lease;
- verify no unrelated process in the active network namespace owns both tested ports;
- start the app-server and let it choose its supported port;
- derive the actual port from the returned authorization URL;
- persist only the safe port number.

If both tested ports are unavailable, fail with:

```text
BROWSER_CALLBACK_PORT_UNAVAILABLE
```

and recommend code sign-in.

Cancellation must:

1. stop accepting manual callback data;
2. call `account/login/cancel` with the matching login ID;
3. wait for completion/cancellation within a bounded deadline;
4. terminate the account runtime if necessary;
5. destroy all transient interaction values;
6. release the global browser-login lease.

---

## 10. API changes

All routes remain private under `/api/internal/v1`.

## 10.1 Start browser login

```http
POST /api/internal/v1/accounts/{account_id}/login/browser
```

Request:

```json
{
  "expected_workspace_id": null,
  "callback_preference": "auto"
}
```

Allowed callback preferences:

```text
auto
manual
```

`auto` means automatic loopback where supported, otherwise manual fallback. It must not silently expose a LAN callback listener.

Response:

```http
202 Accepted
Location: /api/internal/v1/operations/{operation_id}
```

```json
{
  "api_version": "windowkeeper.dev/internal/v1",
  "kind": "LoginAttemptAccepted",
  "data": {
    "operation_id": "...",
    "login_attempt_id": "...",
    "method": "CHATGPT_BROWSER"
  }
}
```

## 10.2 Start device-code login

```http
POST /api/internal/v1/accounts/{account_id}/login/device-code
```

Request:

```json
{
  "expected_workspace_id": null
}
```

Response is the same accepted-operation pattern.

## 10.3 Retrieve transient interaction

```http
GET /api/internal/v1/login-attempts/{attempt_id}/interaction
```

Requirements:

- authenticated initiating session;
- valid interaction nonce supplied in a protected header or session-bound server state;
- no-store response;
- attempt must be `WAITING_FOR_USER`.

Browser response example:

```json
{
  "api_version": "windowkeeper.dev/internal/v1",
  "kind": "BrowserLoginInteraction",
  "data": {
    "authorization_url": "https://...",
    "callback_mode": "MANUAL_FORWARD",
    "expires_at": "2026-07-26T12:34:56Z"
  }
}
```

Device response example:

```json
{
  "api_version": "windowkeeper.dev/internal/v1",
  "kind": "DeviceCodeLoginInteraction",
  "data": {
    "verification_url": "https://...",
    "user_code": "ABCD-EFGH",
    "expires_at": "2026-07-26T12:34:56Z"
  }
}
```

Never include these payloads in ordinary operation polling.

## 10.4 Forward browser callback

```http
POST /api/internal/v1/login-attempts/{attempt_id}/browser-callback
```

Request:

```json
{
  "callback_url": "http://localhost:1455/auth/callback?code=...&state=..."
}
```

Response:

```http
202 Accepted
```

```json
{
  "api_version": "windowkeeper.dev/internal/v1",
  "kind": "BrowserCallbackAccepted",
  "data": {
    "login_attempt_id": "...",
    "status": "forwarding"
  }
}
```

The returned body must never echo the submitted URL.

## 10.5 Cancel login

```http
POST /api/internal/v1/login-attempts/{attempt_id}/cancel
```

Use `202 Accepted` and the existing durable-operation model.

## 10.6 Problem codes

Add stable RFC 9457 problem codes:

```text
LOGIN_ALREADY_ACTIVE
LOGIN_INTERACTION_NOT_READY
LOGIN_INTERACTION_EXPIRED
LOGIN_INTERACTION_SESSION_MISMATCH
LOGIN_INTERACTION_ALREADY_CONSUMED
LOGIN_METHOD_UNAVAILABLE
DEVICE_CODE_DISABLED
BROWSER_CALLBACK_PORT_UNAVAILABLE
BROWSER_CALLBACK_REACHABILITY_REQUIRED
BROWSER_CALLBACK_INVALID
BROWSER_CALLBACK_STATE_MISMATCH
BROWSER_CALLBACK_FORWARD_FAILED
CODEX_BROWSER_AUTH_CONTRACT_CHANGED
CODEX_DEVICE_AUTH_CONTRACT_CHANGED
LOGIN_ACCOUNT_VERIFICATION_FAILED
LOGIN_WORKSPACE_MISMATCH
LOGIN_CREDENTIAL_CHECKPOINT_FAILED
LOGIN_RESTART_REQUIRED
```

Problem details must not include raw upstream URLs, codes, callback query strings, or token data.

---

## 11. SSE changes

SSE publishes only non-sensitive lifecycle information:

```json
{
  "event": "login_attempt.updated.v1",
  "data": {
    "attempt_id": "...",
    "account_id": "...",
    "method": "CHATGPT_DEVICE_CODE",
    "state": "WAITING_FOR_USER",
    "interaction_ready": true,
    "expires_at": "...",
    "error_code": null
  }
}
```

Do not publish:

- authorization URL;
- verification URL;
- user code;
- callback URL;
- OAuth state;
- authorization code;
- upstream raw error;
- credential path or bytes.

SSE replay after reconnect may say that interaction is ready, but the client must retrieve the sensitive interaction from the dedicated no-store endpoint.

---

## 12. CLI changes

## 12.1 Commands

```bash
windowkeeper account add --login-method device-code
windowkeeper account add --login-method browser
windowkeeper account reauthenticate <account> --login-method device-code
windowkeeper account reauthenticate <account> --login-method browser
windowkeeper login status <attempt-id>
windowkeeper login cancel <attempt-id>
```

Interactive mode should offer:

```text
Choose ChatGPT sign-in method:
  1. Sign in with a code (recommended)
  2. Sign in with browser
```

## 12.2 Device-code CLI behavior

Interactive human output may display the URL and user code directly to the attached TTY.

Do not display the code when:

- stdout is redirected;
- no TTY is present;
- `--json` is selected;
- the user has not explicitly selected an interactive authentication command.

For noninteractive scripts, return the operation/login attempt identifiers and require the authenticated dashboard to complete the interaction. This avoids leaking one-time codes into CI logs.

## 12.3 Browser CLI behavior

The CLI should:

1. print the authorization URL only to the TTY;
2. attempt to open it only when explicitly allowed and a browser environment is detected;
3. state whether automatic loopback or manual forwarding is expected;
4. for manual mode, prompt through hidden/non-echoing input where practical;
5. never print the pasted callback URL;
6. clear the input buffer as soon as practical;
7. never include OAuth interaction values in `--json` output.

## 12.4 Docker operation

The normal operational path remains:

```bash
docker compose exec windowkeeper windowkeeper account add --login-method device-code
```

Browser automatic-loopback mode requires the host-network deployment profile or an established port-forwarding environment. In the default bridge profile, the CLI must clearly enter manual callback-forward mode rather than pretending the browser callback is directly reachable.

---

## 13. Security model

## 13.1 Sensitive-value classification

Treat these as secrets or secret-adjacent values:

```text
authorization URL
verification URL when code-bearing
user code
authorization code
callback URL
OAuth state
PKCE verifier and challenge
access token
refresh token
ID token
account-routing claims
raw auth.json
```

The authorization URL and state do not have the same durability as a refresh token, but they can authorize or correlate a live attempt and therefore receive the same no-log/no-persist treatment.

## 13.2 Logging

Add field names to the recursive redaction denylist:

```text
authUrl
auth_url
verificationUrl
verification_url
userCode
user_code
callbackUrl
callback_url
code
state
code_verifier
code_challenge
access_token
refresh_token
id_token
```

Do not log raw app-server login responses even at TRACE.

Allowlisted login log events may contain only:

```text
attempt_id
account_id
method
state
upstream_method
callback_mode
callback_port
elapsed_ms
error_code
```

## 13.3 Browser security headers

Pages and endpoints presenting interaction data require:

```http
Cache-Control: no-store, max-age=0
Pragma: no-cache
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
Cross-Origin-Opener-Policy: same-origin
Content-Security-Policy: default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'
```

Do not load analytics, third-party scripts, remote fonts, QR libraries, or image resources on the login-interaction page.

## 13.4 CSRF and origin checks

All login start, callback-forward, cancel, and transfer actions require:

- administrator session;
- synchronizer CSRF token;
- same-origin Origin/Referer validation;
- POST semantics;
- recent reauthentication when replacing credentials.

## 13.5 Interaction session binding

Hash and store the initiating server-side session identifier. Compare it on every interaction retrieval or callback-forward action.

Session logout or expiration immediately invalidates the transient interaction and triggers cancellation of the upstream login.

## 13.6 No generic SSRF primitive

The manual callback forwarder must not accept an arbitrary destination. The destination is constructed from the validated expected callback tuple retained for the active attempt.

The submitted URL supplies only the query values after it passes complete validation. The outbound request is always local loopback and cannot be redirected.

## 13.7 Credential checkpointing

After either OAuth flow:

1. stop account operations;
2. ensure the app-server has flushed credentials;
3. validate runtime directory ownership and mode;
4. open the credential file without following symlinks;
5. require a regular file with expected maximum size;
6. read exact bytes;
7. encrypt into a candidate credential generation;
8. commit candidate and account metadata atomically;
9. promote through the existing crash-safe key/generation process;
10. clean plaintext runtime;
11. report completion only after commit.

Do not parse tokens to implement refresh or infer account identity unless the pinned compatibility adapter explicitly requires a documented field. Use `account/read` for identity evidence.

---

## 14. Failure and recovery behavior

## 14.1 Service restart

OAuth interaction attempts are not restart-resumable because their authorization URL/code and in-memory validation data are intentionally not durable.

At startup:

1. find nonterminal login attempts;
2. mark them `RESTART_REQUIRED`;
3. terminate or clean any leftover runtime;
4. retain the existing active credential generation, if this was reauthentication;
5. do not adopt a partially written candidate credential unless the credential-checkpoint protocol proves it was fully committed;
6. require a fresh login attempt.

## 14.2 Restart after OAuth completion

There are three cases:

### Before credential file exists

Mark `RESTART_REQUIRED`; no replacement occurs.

### Credential file exists but candidate encryption was not committed

Preserve the old active credential generation. Remove the incomplete candidate/runtime and require reauthentication.

### Candidate credential committed and promotion transaction completed

The account is authenticated even if the final UI operation status was not committed. Startup reconciliation verifies the active generation and changes the login attempt to `COMPLETED` with a recovery marker.

## 14.3 Account mismatch

If `account/read` returns a different expected workspace or other enforceable identity restriction:

- do not overwrite the current active credential;
- log out and destroy the candidate runtime;
- mark `LOGIN_WORKSPACE_MISMATCH`;
- open an action-required incident when reauthentication was required for an enabled account.

Email address alone is display evidence and is not a strong stable identity key.

## 14.4 Rate-limit read failure after login

A successful OAuth login followed by a transient `account/rateLimits/read` failure may still produce a valid credential, but Windowkeeper must distinguish:

- authentication verified;
- usage integration not yet verified.

For first enrollment, keep the account disabled in `STARTING` or `ACTION_REQUIRED` until a bounded verification retry succeeds or the operator explicitly accepts a documented unsupported-plan condition.

For reauthentication, the replacement credential may be promoted only when `account/read` proves the intended account. Usage verification can retry through the normal usage subsystem if the failure is transport-only.

## 14.5 Device-code policy rejection

When device code is disabled by account/workspace policy:

- present browser OAuth as the alternative;
- do not repeatedly retry device-code login;
- persist a sanitized capability result with an expiration, for example 24 hours;
- allow an explicit operator retry after policy changes.

## 14.6 Browser callback conflict

If the returned callback port is occupied by another client or the callback is delivered to the wrong active listener:

- state validation in the official app-server should reject the callback;
- Windowkeeper shows `BROWSER_CALLBACK_STATE_MISMATCH` without raw state values;
- cancel the attempt;
- recommend closing competing clients or using code sign-in.

---

## 15. Configuration

Add:

```dotenv
# Preferred method shown first in the UI.
WINDOWKEEPER_DEFAULT_CHATGPT_LOGIN_METHOD=device-code

# Browser OAuth deployment capability.
# Values: disabled, manual, host-loopback
WINDOWKEEPER_BROWSER_OAUTH_MODE=manual

# Tested callback ports for the pinned Codex release.
WINDOWKEEPER_BROWSER_OAUTH_CALLBACK_PORTS=1455,1457

# Global browser attempt limit; v1 must remain 1.
WINDOWKEEPER_BROWSER_OAUTH_CONCURRENCY=1

# Overall user-interaction deadline unless the app-server returns a shorter one.
WINDOWKEEPER_LOGIN_TIMEOUT=15m

# Maximum submitted callback URL size.
WINDOWKEEPER_BROWSER_CALLBACK_MAX_BYTES=16384
```

Validation:

- Reject `host-loopback` unless the deployment explicitly confirms host networking or an equivalent tested loopback-forwarding environment.
- Reject callback ports outside the release compatibility manifest unless a new Codex version has passed its proof-of-concept.
- Do not permit `0.0.0.0` callback listeners.
- Do not expose a setting for OAuth client ID, issuer, scopes, token endpoint, or redirect path.

---

## 16. Application service interfaces

Conceptual interfaces:

```python
class LoginMethod(StrEnum):
    CHATGPT_BROWSER = "CHATGPT_BROWSER"
    CHATGPT_DEVICE_CODE = "CHATGPT_DEVICE_CODE"


class AuthenticationService(Protocol):
    async def start_login(
        self,
        *,
        account_id: UUID,
        method: LoginMethod,
        initiating_session: SessionIdentity,
        expected_workspace_id: str | None,
        callback_preference: str | None,
    ) -> LoginAttemptAccepted: ...

    async def get_interaction(
        self,
        *,
        attempt_id: UUID,
        session: SessionIdentity,
    ) -> LoginInteractionView: ...

    async def forward_browser_callback(
        self,
        *,
        attempt_id: UUID,
        session: SessionIdentity,
        callback_url: SecretStr,
    ) -> None: ...

    async def cancel_login(
        self,
        *,
        attempt_id: UUID,
        session: SessionIdentity,
    ) -> OperationRef: ...
```

The app-server adapter exposes a method-neutral result:

```python
@dataclass(frozen=True)
class UpstreamLoginInteraction:
    login_id: str
    method: LoginMethod
    auth_url: SecretStr | None
    verification_url: SecretStr | None
    user_code: SecretStr | None
    expires_at: datetime | None
```

Use secret wrapper types whose `repr`, string formatting, serialization, and logging return redacted placeholders.

---

## 17. Proof-of-concept plan

Complete this before implementing the full UI.

## 17.1 Official app-server contract proof

Against the exact proposed pinned Codex release:

1. Generate stable JSON Schema.
2. Verify `chatgpt` exists in `account/login/start`.
3. Verify `chatgptDeviceCode` exists.
4. Verify browser response includes `loginId` and `authUrl`.
5. Verify device response includes the documented interaction fields.
6. Verify `account/login/cancel` behavior for each method.
7. Verify completion and account-update notification ordering.
8. Verify `account/read` after both methods.
9. Verify the same `account/rateLimits/read` path after both methods.
10. Verify credential refresh modifies the expected credential bundle.

## 17.2 Browser callback proof

Test:

- automatic callback in Linux host-network mode on port 1455;
- fallback to 1457 with 1455 occupied;
- browser on another computer through SSH port forwarding;
- normal Docker bridge mode with secure manual callback forwarding;
- state mismatch rejection;
- expired authorization code;
- repeated callback submission;
- callback submission after cancellation;
- simultaneous browser-login rejection;
- competing local Codex client on port 1455.

## 17.3 Device-code proof

Test:

- normal approval;
- user denial;
- expiration;
- cancellation;
- workspace policy disables device code;
- browser on a different device;
- two different accounts authenticating concurrently;
- wrong login notification routed to an account actor.

## 17.4 Credential equivalence proof

Using one disposable account in separate clean runs:

1. Login through browser OAuth.
2. Record only safe structural metadata about the generated credential file.
3. Validate account and usage reads.
4. Destroy the test credential generation.
5. Login through device-code OAuth.
6. Validate the same account and usage operations.
7. Confirm both are surfaced as upstream `chatgpt` authentication.
8. Confirm Windowkeeper does not require method-specific runtime behavior after checkpoint.

Do not compare or retain raw token strings in the test report.

---

## 18. Automated test plan

## 18.1 Unit tests

- login method parsing and defaults;
- authorization URL validation;
- redirect URI allowlist validation;
- callback URL parsing;
- duplicate query-key rejection;
- state hash constant-time comparison;
- transient interaction expiry;
- session binding;
- one-time interaction consumption;
- login state-transition legality;
- error-code sanitization;
- secret wrapper serialization and `repr`;
- callback destination construction cannot escape loopback;
- startup conversion to `RESTART_REQUIRED`.

## 18.2 Fake app-server tests

The schema-validated fake app-server should script:

```text
browser interaction -> success
browser interaction -> cancel
browser interaction -> callback state failure
browser completion -> account verification success
browser completion -> workspace mismatch
device interaction -> success
device interaction -> denial
device interaction -> expiration
notification for wrong login ID
credential file absent after completion
credential file malformed
credential file changes during checkpoint
app-server EOF at every phase
```

The fake credential writer must allow crash failpoints before and after fsync/rename boundaries.

## 18.3 Database tests

- partial unique index rejects two active attempts for one account;
- completed history allows a later new attempt;
- browser global lease is respected;
- operation and login-attempt creation is atomic;
- credential promotion and login completion remain consistent across crash;
- restart leaves old credential active if replacement was incomplete.

## 18.4 HTTP tests

- CSRF required for start, cancel, and callback forward;
- Origin/Referer validation;
- interaction endpoint session mismatch;
- no-store headers;
- sensitive values absent from operation and account endpoints;
- callback input never echoed;
- callback request body size limit;
- generic URLs rejected;
- redirect following disabled;
- SSE contains only `interaction_ready`.

## 18.5 Browser smoke tests

One Chromium suite should cover:

1. Add-account method selector.
2. Device-code interaction page.
3. Copy code and open verification URL controls.
4. Expiration and cancel behavior.
5. Browser interaction page.
6. Manual callback URL input.
7. Invalid callback error without leaking the URL.
8. Successful transition to account verification.
9. Reauthentication confirmation.
10. Page refresh does not expose the interaction to another session.

Do not automate real ChatGPT credentials in ordinary CI.

## 18.6 Redaction tests

Inject canary values into every sensitive field and verify absence from:

- stdout/stderr;
- JSONL files;
- log SSE;
- state SSE;
- database text columns;
- operation JSON;
- incident payloads;
- webhook payloads;
- browser HTML;
- exception traces;
- downloaded diagnostics.

The release must fail on any canary match.

---

## 19. Release acceptance criteria

The dual OAuth add-on is release-ready only when:

1. The pinned official app-server exposes and passes both managed login flows.
2. Browser login succeeds through at least one automatic-loopback environment.
3. Browser login succeeds through the manual callback-forward fallback in Docker bridge mode.
4. Device-code login succeeds with a browser on a different device.
5. Both methods produce a usable encrypted credential bundle and the same downstream account behavior.
6. Neither method requires Windowkeeper to know or persist OpenAI OAuth client secrets, PKCE verifiers, or refresh-token protocol details.
7. Cancellation is reliable for both methods.
8. Restart never promotes an incomplete replacement credential.
9. Workspace mismatch never overwrites an existing valid credential.
10. No sensitive interaction value appears in any persistent or streamed diagnostic surface.
11. Only one browser login can own the callback port set at a time.
12. Device-code attempts remain isolated across at least three simultaneous accounts under the configured auth semaphore.
13. Browser callbacks with incorrect state, port, path, session, or attempt ID are rejected.
14. A Codex version/schema mismatch disables login without damaging stored credentials.
15. Documentation clearly recommends device-code login for Docker, NAS, SSH, and remote use.

---

## 20. Rollout plan

### Phase A — Adapter and fake integration

- Add stable app-server request/response models.
- Add `LoginMethod` and `login_attempts` schema.
- Implement state machine and transient interaction store.
- Implement both adapter calls and cancellation.
- Build fake app-server scenarios.

### Phase B — Device-code path

- Implement dashboard and CLI interaction.
- Complete real-account proof.
- Make device-code the default method.

### Phase C — Browser automatic callback

- Add host-network deployment profile.
- Validate ports 1455 and 1457.
- Document SSH forwarding.
- Complete protected real-account tests.

### Phase D — Browser manual callback fallback

- Implement strict callback parser and one-time forwarder.
- Add interaction-session binding and redaction tests.
- Complete bridge-network real-account proof.

### Phase E — Reauthentication and hardening

- Integrate crash-safe credential replacement.
- Add workspace restrictions.
- Complete restart/failpoint matrix.
- Add release documentation and troubleshooting.

---

## 21. Compatibility policy

OAuth is one of the highest-risk upstream compatibility areas. For every Codex upgrade:

1. Regenerate the stable protocol schema.
2. Diff login request variants and response shapes.
3. Confirm both login method identifiers still exist.
4. Confirm cancellation and completion notification fields.
5. Inspect browser callback source for port, host, path, and success-page changes.
6. Run browser and device protected integration tests.
7. Confirm credential storage and refresh mutations.
8. Update the release compatibility manifest.
9. Do not deploy the upgrade when any auth contract is unverified.

Windowkeeper must not add a raw-OAuth fallback if the app-server contract changes. The correct response is to hold the Codex upgrade or update the narrow adapter after testing.

---

## 22. Documentation text for operators

### Sign in with a code

Use this method for normal Windowkeeper deployments. Windowkeeper shows an OpenAI verification page and one-time code. Open the page on any device, enter the code, and return to Windowkeeper. The code expires and is never stored in Windowkeeper's database or logs.

### Sign in with browser

This opens the standard ChatGPT sign-in page. Codex completes the flow through a temporary localhost callback.

- When Windowkeeper uses the host-network browser-auth profile and the browser is on the Docker host, completion is automatic.
- For a remote browser, forward localhost ports 1455 and 1457 over SSH.
- In normal Docker bridge mode, the browser may show a connection error after successful authorization. Copy the complete localhost callback URL from the address bar and paste it into Windowkeeper's secure callback field. Windowkeeper forwards it once to the official Codex callback handler and immediately discards it.

Never send the callback URL, authorization URL, or one-time code to another person. Do not paste them into issue reports, logs, or chat messages.

---

## 23. Sources

### Official OpenAI/Codex

- Codex app-server authentication endpoints and managed login modes:  
  <https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>
- Codex browser callback server, default/fallback ports, redirect handling, PKCE, and token persistence:  
  <https://github.com/openai/codex/blob/main/codex-rs/login/src/server.rs>
- Remote/headless OAuth discussion and callback replay/port-forwarding examples:  
  <https://github.com/openai/codex/discussions/4650>
- Remote callback listener issue and confirmed local-curl behavior:  
  <https://github.com/openai/codex/issues/4265>

### Pi Agent

- Repository:
  <https://github.com/earendil-works/pi>
- Provider and OAuth documentation:
  <https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/providers.md>
  <https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/custom-provider.md>

### OpenCode

- Codex OAuth plugin source containing browser and headless methods:  
  <https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/plugin/codex.ts>
- Authentication-method regression report:  
  <https://github.com/anomalyco/opencode/issues/27905>
- Browser token-exchange failure report:  
  <https://github.com/anomalyco/opencode/issues/16281>

### Hermes Agent

- Authentication implementation:  
  <https://github.com/NousResearch/hermes-agent/blob/main/hermes_cli/auth.py>
- Provider documentation:  
  <https://github.com/NousResearch/hermes-agent/blob/main/website/docs/integrations/providers.md>
- Codex runtime credential prerequisites:  
  <https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/codex-app-server-runtime.md>

---

## 24. Final recommendation

Add both choices to Windowkeeper v1, but implement them as two orchestrated entry points into the official Codex app-server rather than two Windowkeeper-owned OAuth clients.

Use **Sign in with a code** as the normal default. It matches Windowkeeper's Docker-first, remote-friendly deployment and avoids localhost callback ambiguity.

Keep **Sign in with browser** as a fully supported alternative for operators whose workspace disables device code or who prefer the standard ChatGPT login experience. Support automatic loopback completion in an explicit host-network/port-forwarded environment and a tightly validated, one-time manual callback-forward fallback in ordinary Docker bridge mode.

This design provides the requested dual sign-in experience while preserving the main plan's strongest requirements: official upstream integration, account isolation, encrypted credential persistence, minimal secret exposure, version-pinned compatibility, and fail-closed behavior.
