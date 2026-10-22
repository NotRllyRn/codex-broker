# Codex refresh-token forking research

## Current conclusion

Codex `0.145.0` supports a proactive refresh through `account/read` with `refreshToken: true`, persists successful token changes to `auth.json`, and treats reused, expired, revoked, or otherwise invalid refresh credentials as permanent authentication failures.

OpenAI does not provide a stable contract that repeated use of one refresh token creates independently renewable lineages. A historical grace-period observation is not a cloning API.

Primary pinned sources:

- <https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/app-server-protocol/src/protocol/v2/account.rs>
- <https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/core/src/auth.rs>
- <https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/core/src/auth/storage.rs>

## Codex Broker boundary

Codex Broker lets Codex own normal managed refresh timing and treats `auth.json` as opaque mutable state. Every authenticated runtime is quiesced and checkpointed before deletion, including when its requested RPC fails.

Enrollment now checkpoints the original verified OAuth credential directly.
There is no export issuance or auth-file download. Migration 024 removes legacy
export snapshots while preserving active credentials and retired history.

## Historical risk

The removed implementation refreshed the same original source separately for
managed and export credentials. Different token values did not establish
independent refresh lineages. On 2026-10-22, an affected account's managed and
export access tokens both returned `token_revoked`, and both refresh tokens
returned `refresh_token_invalidated`. Those results establish upstream
invalidation but do not prove its cause.
