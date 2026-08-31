# One-login managed credential and export snapshot

## Status

Superseded by [managed credential checkpointing](managed-credential-checkpointing.md).

## Outcome

One OAuth enrollment can issue:

1. one mutable `ACTIVE` credential owned by Windowkeeper;
2. at most one immutable `EXPORT` snapshot owned by an external service.

The fork is attempted only when an account has no export. Windowkeeper persists the managed result before attempting export issuance, so an export failure cannot roll back or destroy the valid managed credential.

## Normal operation

Usage refresh, activation, and reconciliation use only `ACTIVE`. Codex owns normal refresh timing. After every authenticated runtime, Windowkeeper quiesces Codex, captures opaque `auth.json`, atomically advances `ACTIVE` when it changed, and only then removes plaintext.

An existing `EXPORT` is never materialized, refreshed, replaced, or used by Windowkeeper. It is not guaranteed to remain independently renewable after `ACTIVE` rotates. Operators must not copy one export into multiple independently refreshing services.

## Failure invariants

- A changed managed credential is checkpointed even when the requested RPC fails.
- Old `ACTIVE` becomes `RETIRED` only in the transaction that inserts its replacement.
- Export failure leaves the managed account usable and reports `EXPORT_FORK_FAILED`.
- Checkpoint failure prevents semantic success and quarantines the runtime evidence.
- Download responses remain non-cacheable and require session, CSRF, and administrator reauthentication.
- Protected live-account evidence remains required because upstream does not guarantee independent refresh-token forks.
