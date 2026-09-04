# Hermes Agent patch: Codex Broker credentials

## Target

Implement this change in the live Hermes checkout, not in Codex Broker.

- Audited snapshot: `hermes-agent-2026.8.31`
- Source archive SHA-256: `216dda97a3fa29637268d225e62229b92118c27a2e80d639d0e15aa12ef0b2eb`
- Broker contract: `POST /api/v1/route` in Codex Broker `plan.md`

## Implementation status

This specification is implemented in [`NotRllyRn/hermes-agent-codex-broker`](https://github.com/NotRllyRn/hermes-agent-codex-broker). The production pin is branch `codex-broker/v0.21.0`, tag `codex-broker-v0.21.0`, commit `da7102a9e0`, based on upstream production revision `b0ab2e163a` reported as Hermes Agent v0.21.0. The same commit is recorded by the `integrations/hermes-agent` submodule.

The live v0.21.0 checkout had decomposed several paths since the archived audit, so the implementation was adapted to its current `agent/agent_init.py`, conversation phase modules, client lifecycle, and runtime-provider seams rather than copied by line number. See [`hermes-agent.md`](hermes-agent.md) for installation and future-release maintenance.

## Static audit result

A plugin-only implementation is not safe in this snapshot:

1. `llm_request` middleware can add `extra_headers`, and the OpenAI SDK should let those headers override client defaults.
2. `pre_llm_call`, request middleware, and execution middleware are fail-open. Exceptions are logged and Hermes continues to the provider.
3. `llm_execution.next_call` is deliberately single-use, so middleware cannot retry with a second lease.
4. The built-in Codex 401 branch calls `_try_refresh_codex_client_credentials(force=True)`, which would return credential ownership to Hermes.
5. Provider initialization resolves Hermes's native `credential_pool.openai-codex` unless an explicit key is supplied.

Therefore, add one small fail-closed core integration. Do not create or mutate Hermes `PooledCredential` rows.

## Configuration

Add only these settings, read from the process environment:

```text
HERMES_CODEX_BROKER_URL=https://192.168.1.20:8787
HERMES_CODEX_BROKER_CLIENT_KEY=cbk_...
HERMES_CODEX_BROKER_CA_CERT=/path/to/ca.crt  # optional if CA is in OS trust
```

Broker mode is enabled only when URL and client key are both present. Reject an `http://` URL. Never support disabled certificate verification. Do not accept a refresh token, account list, local routing policy, or persisted access-token cache.

## New file: `agent/codex_broker.py`

Implement a synchronous, thread-safe `CodexBrokerLeaseManager` because Hermes's main conversation loop is synchronous.

Minimal state:

```python
@dataclass(frozen=True)
class Lease:
    account_id: str              # broker public account ID
    access_token: str
    chatgpt_account_id: str
    expires_at: str

class CodexBrokerLeaseManager:
    _lease_by_turn: dict[tuple[str, str], Lease]
    _preferred_by_session: dict[str, str]  # non-secret affinity only
    _failed_turns: set[tuple[str, str]]    # bounds failover to one
    _lock: threading.RLock
```

Required methods:

- `from_environment() -> CodexBrokerLeaseManager | None`
- `lease_for_turn(session_id, turn_id, *, interrupted) -> Lease`
- `replace_failed_lease(session_id, turn_id, failure_kind, *, interrupted) -> Lease | None`
- `discard_turn(session_id, turn_id) -> None`
- `apply_to_agent(agent, lease) -> None`

HTTP behavior:

- use Hermes's existing `httpx` dependency;
- verify the server with system trust or `HERMES_CODEX_BROKER_CA_CERT`;
- authenticate with `Authorization: Bearer cbk_...`;
- set a finite connect/read timeout;
- validate every response field and never include the response body in an exception or log;
- on `status=wait`, sleep until the broker's `next_retry_at`/`retry_after_seconds`, checking interruption at least once per second, then call `/route` again;
- fail closed on broker errors: no request may use the placeholder or a previous turn's lease;
- erase turn leases at turn completion; retain only the preferred broker account ID for session affinity.

`apply_to_agent()` must update the active Codex client atomically:

1. Build required Codex identity headers with `agent.codex_headers.codex_cloudflare_headers()`.
2. Set `ChatGPT-Account-ID` from the broker response explicitly.
3. Set `agent.api_key` and a copied `agent._client_kwargs["api_key"]` to the leased access token.
4. Set copied `agent._client_kwargs["default_headers"]` to the required headers.
5. Call `agent._replace_primary_openai_client(reason="codex_broker_lease")` and fail if it returns false.

Do not log the lease object, token, Authorization header, raw broker response, or client key.

## Modify `hermes_cli/runtime_provider.py`

In `_resolve_explicit_provider()`'s `provider == "openai-codex"` branch (around line 1750 in the audited snapshot):

- when broker mode is configured, return the official Codex base URL, `api_mode="codex_responses"`, and an in-memory sentinel API key such as `broker-managed`;
- do not call `resolve_codex_runtime_credentials()`;
- mark the returned source as `codex-broker`;
- never persist the sentinel as OAuth state.

This sentinel exists only so `AIAgent` can initialize before the first user-turn lease. The fail-closed conversation-loop hook below must replace it before provider execution.

Apply the same rule to other `openai-codex` runtime-resolution branches around lines 518 and 2197. Centralize the predicate/helper rather than duplicating environment parsing.

## Modify `agent/agent_init.py`

Near the provider/client initialization around lines 1310-1545:

- attach `agent._codex_broker = CodexBrokerLeaseManager.from_environment()`;
- broker mode is valid only for `provider == "openai-codex"` and `api_mode == "codex_responses"`;
- force `agent._credential_pool = None` in broker mode;
- do not call `resolve_provider_client("openai-codex", ...)` or load native Codex credentials in broker mode;
- allow initial client construction with the non-secret sentinel, but do not execute a request until a lease has been applied.

Subagents and cron/gateway sessions receive the same manager behavior. Do not silently disable broker mode on non-CLI execution paths.

## Modify `agent/conversation_loop.py`

### Per-user-turn lease

The audited loop has stable `session_id` and `turn_id`, and one outer user turn can contain many API/tool iterations. Immediately before the first provider execution for each `(session_id, turn_id)`:

```python
if agent.provider == "openai-codex" and agent._codex_broker is not None:
    lease = agent._codex_broker.lease_for_turn(
        agent.session_id or "", turn_id, interrupted=lambda: agent._interrupt_requested
    )
    agent._codex_broker.apply_to_agent(agent, lease)
```

Place this before `pre_api_request` and `run_llm_execution_middleware`, after request identity is known. `lease_for_turn()` must return the same in-memory lease for inner tool-loop calls but perform a new broker call for the next user turn.

### Track safe retry boundary

The current `_stop_spinner()` callback is passed as `on_first_delta`. Extend it to set a local `attempt_output_started = True`; reset that flag before every physical provider attempt. This is the replay boundary. Never broker-reroute an attempt after the first model delta.

### Bounded failure replacement

After `classify_api_error()` (around line 4880), before the built-in Codex OAuth-refresh branch around line 5052:

- map HTTP 401/403 to `auth`;
- map account quota/usage exhaustion and terminal HTTP 429 to `quota`;
- ignore generic transport and server failures;
- if broker mode is active, no output started, and this turn has not failed over, call `replace_failed_lease()`;
- when it returns a lease, apply it and `continue` the existing outer retry loop without incrementing the normal retry budget;
- when all accounts are exhausted, the manager waits for the broker timestamp and then returns a lease;
- when broker access fails, abort the turn clearly instead of falling through to native Codex refresh;
- after one replacement failure, use normal terminal error handling; never request a third account in the same user turn.

In broker mode, skip `_try_refresh_codex_client_credentials(force=True)` unconditionally. Codex Broker is the only refresh-token owner.

### Cleanup

Call `discard_turn()` from the existing turn-finalization path on success, error, and interruption. Keep only the non-secret preferred account ID.

## Modify `agent/codex_headers.py`

Add a small overload/helper that accepts an explicit ChatGPT account ID. Keep JWT extraction for native Hermes auth, but broker mode must use the broker field and must not infer routing identity independently.

Required precedence:

1. Hermes `User-Agent` and `originator` remain mandatory.
2. Broker-provided `ChatGPT-Account-ID` replaces any existing account header case-insensitively.
3. User-configured headers cannot replace Authorization or account identity in broker mode.

## Disable conflicting native behavior

When broker mode is active:

- do not load, select, rotate, cool down, refresh, or write `credential_pool.openai-codex`;
- do not seed from `providers.openai-codex.tokens` or `~/.codex/auth.json`;
- do not run local Codex account usage probes for routing decisions;
- do not show advice to run `codex` or `hermes auth add openai-codex` after broker-auth failures;
- show `Codex Broker unavailable`, `waiting for broker pool reset`, or `Codex Broker client key rejected` as appropriate.

Leave all native behavior unchanged when broker environment variables are absent.

## Tests to add in the Hermes checkout

Add focused tests rather than copying the entire broker implementation:

- `tests/agent/test_codex_broker.py`
  - rejects HTTP and untrusted TLS;
  - trusted local CA succeeds;
  - validates lease/wait payloads;
  - never serializes or persists access tokens;
  - exact wait timestamp is honored and interruption cancels wait;
  - preferred account is reused on a new turn;
  - replacement includes `failed_account_id` and `failure_kind`;
  - only one replacement is allowed per turn.
- `tests/run_agent/test_codex_broker_runtime.py`
  - initializes with no Hermes Codex OAuth credential or pool entry;
  - one `/route` call per user turn, not per tool-loop request;
  - actual Responses request carries leased Authorization and account ID;
  - 401 before output rebuilds the client and retries once;
  - terminal quota 429 before output changes account and retries once;
  - any first delta prevents automatic replay;
  - pool wait resumes after broker reset;
  - broker outage fails closed;
  - native `_try_refresh_codex_client_credentials` is never called;
  - turn cleanup removes access-token state.
- Extend `tests/agent/test_codex_cloudflare_headers.py` for explicit broker account identity and case-insensitive override.
- Extend credential-pool tests to prove broker mode performs zero pool reads/writes while native mode is unchanged.

Use a local HTTPS test server and generated test CA. Do not weaken TLS in tests with `verify=False`.

## Live acceptance procedure

1. Back up Hermes auth/config state.
2. Remove/disable Hermes's local `openai-codex` credential pool for the test profile.
3. Create a dedicated broker client key named for the Hermes host.
4. Configure the three environment variables and select an `openai-codex` model.
5. Run a prompt that performs at least two tool calls; verify one broker request for the user turn.
6. Submit a second prompt; verify a fresh broker request with the previous account as preference.
7. Force account A to return 401 before output; verify exactly one broker failure report and account B request.
8. Force a quota response before output; verify account B is selected.
9. Force a failure after a streamed delta; verify no replay occurs.
10. Exhaust every account; verify Hermes waits until the broker-provided timestamp and resumes.
11. Revoke the Hermes broker key; verify the next turn fails before any OpenAI request.
12. Search Hermes state, logs, config, and credential pools for the leased token; it must not exist after turn cleanup.

## Acceptance gate

Ship the Hermes integration only when every automated and live test above passes. If the live Hermes revision lacks `_replace_primary_openai_client`, `on_first_delta`, or the stable retry loop described here, re-audit that revision and adapt the seam; do not monkey-patch private objects from a plugin.
