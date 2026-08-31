# Codex refresh-token forking research

## Current conclusion

Codex `0.145.0` supports a proactive refresh through `account/read` with `refreshToken: true`, persists successful token changes to `auth.json`, and treats reused, expired, revoked, or otherwise invalid refresh credentials as permanent authentication failures.

OpenAI does not provide a stable contract that repeated use of one refresh token creates independently renewable lineages. A historical grace-period observation is not a cloning API.

Primary pinned sources:

- <https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/app-server-protocol/src/protocol/v2/account.rs>
- <https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/core/src/auth.rs>
- <https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/core/src/auth/storage.rs>

## Windowkeeper boundary

Windowkeeper lets Codex own normal managed refresh timing and treats `auth.json` as opaque mutable state. Every authenticated runtime is quiesced and checkpointed before deletion, including when its requested RPC fails.

For the required download feature, enrollment may attempt one controlled second refresh to create an `EXPORT` snapshot after the new `ACTIVE` credential is already durable. Windowkeeper never uses or refreshes that export and never rolls ACTIVE back when export issuance fails.

## Limitations

- The export may stop renewing after ACTIVE rotates.
- One export must not be copied into multiple independently refreshing services.
- Multiple independent named exports require another OAuth approval or a coordinated credential-lease protocol; static cloning is not safe.
- Live-account release evidence can observe current behavior but cannot turn it into an upstream guarantee.
