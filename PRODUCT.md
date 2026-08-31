# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Windowkeeper serves one self-hosting operator who administers multiple ChatGPT/Codex identities they own or are explicitly authorized to manage. They typically use it from a trusted LAN, NAS, Docker host, or remote SSH environment and need reliable unattended operation plus clear evidence when intervention is required.

## Product Purpose

Windowkeeper monitors each account's Codex usage windows and intentionally submits one fixed minimal activation after short and weekly limits permit it. Success means exhausted accounts recover automatically after reset, every account remains isolated, activation is never blindly duplicated, and the operator can understand and repair every unhealthy state through the dashboard or CLI.

## Positioning

Windowkeeper is an evidence-driven, fail-closed activation supervisor. Unlike account rotators or quota pools, it preserves one managed credential lineage per account and prefers skipping an ambiguous window over risking a duplicate accepted turn.

## Operating Context

The service runs as one hardened Docker-first Python process on Linux amd64 or arm64. Operators enroll an account with one ChatGPT authorization, optionally download its externally owned enrollment `auth.json` snapshot, inspect short and weekly usage, manage schedules and incidents, run local administrative CLI commands, and receive durable webhook notifications.

## Capabilities and Constraints

- Multiple independently authenticated accounts with overlapping labels.
- The Codex app-server installed and managed by Windowkeeper is the only upstream integration seam.
- Device-code login is recommended; managed browser login is also supported. New manual token import is retired.
- SQLite is the durable store; one process owns one data directory.
- Each account has one mutable ACTIVE lineage and at most one immutable, externally owned EXPORT snapshot.
- Every authenticated Codex runtime checkpoints opaque `auth.json` before deletion, including after RPC failure.
- Credentials use AES-256-GCM envelopes and exist in plaintext only in private runtime directories or the authenticated export response.
- One accepted activation at most for each account and window key; exhausted windows wait automatically, while genuinely ambiguous submissions block replay.
- Activation uses the account's cheapest available model with verified standard-tier rates, its lowest advertised reasoning effort, and no premium speed tier.
- FastAPI, Jinja, native JavaScript, native SSE, Click, and no Node build or client router.
- Dashboard view models remain independent from layout experiments.
- WCAG 2.2 AA target.
- Protected live-account gates remain release-time decisions and must not be fabricated.

## Evidence on Hand

The implementation baseline is `WINDOWKEEPER_V1_DETAILED_IMPLEMENTATION_PLAN.md`. The dual-login add-on is `WINDOWKEEPER_V1_ADDON_DUAL_CHATGPT_OAUTH.md`. No customer claims, benchmarks, brand assets, or production credentials are present and future work must not invent them.

## Product Principles

1. Evidence over inferred health.
2. Durable truth before external effects.
3. Fail closed when acceptance is ambiguous.
4. Keep every credential lineage and runtime isolated.
5. Expose operational complexity through small, stable service and view-model interfaces.

## Accessibility & Inclusion

The dashboard targets WCAG 2.2 AA with semantic landmarks and tables, keyboard-operable controls, visible focus, non-color state labels, reduced-motion support, mobile-specific renderers, and exact accessible timestamps.
