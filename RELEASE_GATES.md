# Codex Broker release gates

A release is blocked unless every applicable gate passes.

## Automated

```bash
uv sync --all-extras
uv run ruff check src tests
uv run pyright src tests
uv run pytest
npm --prefix packages/pi-extension test
npm --prefix packages/pi-extension run check
```

Required coverage includes:

- migrations through 009, foreign keys, idempotency, and credential-generation preservation;
- upgrade from a pre-rename database without account relogin;
- vault mismatch, verification, rotation, backup, and restore;
- managed credential checkpointing after success, RPC failure, cancellation, and restart;
- concurrent near-expiry leases producing one credential mutation;
- stable preferred routing, failed-account exclusion, known exhaustion, exact reset padding, and unknown-reset failure;
- client-key one-time display, hashing, authentication, revocation, and log redaction;
- persistent administrator sessions, CSRF, logout/password-change revocation, and Orbit-only UI;
- non-loopback TLS startup guard and trusted/untrusted CA behavior;
- Pi one-lease-per-user-turn behavior, bounded pre-output failover, wait/resume, and no secret persistence;
- absence of activation endpoints, scheduler code, activation tables after migration 009, and broker-owned model turns.

## Compatibility

Record the pinned Codex package version, `codex --version`, executable SHA-256, App Server initialization/login/rate-limit response shapes, callback ports, and credential-file allowlist. Test upgrades independently from broker behavior changes.

Historical compatibility identifiers must remain stable: `windowkeeper.db`, lock/volume names, vault sentinel and HKDF/AAD strings, cookie name, and existing persisted rows. New CLI, webhook, log, and problem-schema values use Codex Broker branding.

## Live security and deployment

- Start native or emulated `linux/amd64` and `linux/arm64` images.
- Verify readiness, device login, browser callback validation, routing lease, checkpoint, backup/restore, and vulnerability scan.
- Capture LAN traffic and confirm credentials/client keys are not readable.
- Confirm a wrong CA, hostname/IP SAN mismatch, expired certificate, and revoked client key fail closed.
- Search logs, Pi state, and browser responses for access token, refresh token, `auth.json`, callback values, and client-key leakage.

## Hermes gate

Hermes source is maintained in the dedicated fork and pinned here as a submodule; it is not packaged into Codex Broker. For every Hermes release, rebase `codex-broker-next`, create a new immutable version branch/tag, run the automated and live acceptance steps in `docs/integrations/hermes-agent-patch.md`, then update the submodule and installer pins together. The installer must refuse unknown revisions, validate TLS/client authentication, and roll back when gateway startup fails.

## Final

Test against a copy of a real existing data volume. Release only after account listing, export decryption, route leasing, and a managed credential checkpoint succeed without relogin. Do not push or publish from the implementation session.
