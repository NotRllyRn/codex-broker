# Product

<!-- impeccable:product-schema 1 -->

## Platform

Web service with a browser administration UI and authenticated machine API.

## Users

Codex Broker serves a self-hosting operator who administers multiple ChatGPT/Codex identities they own or are authorized to manage. Pi, Hermes, T3 Code, and the local ChatGPT macOS adapter consume broker-issued access-only leases. Responses-compatible apps can also use the built-in proxy with the same client keys.

## Product purpose

Prevent independent Codex clients from racing or invalidating rotating OAuth refresh tokens. Codex Broker is the single mutable credential authority, tracks authoritative usage windows, and routes each new user turn to an eligible account.

## Operating context

One hardened Docker-first Go process runs on a trusted Linux host. Same-network clients connect by local IP over verified TLS and authenticate with hashed, revocable broker client keys. Clients either call Codex directly with leased access tokens or send Responses requests through the broker on its existing listener. The optional macOS loopback adapter is a client-side Responses bridge because the ChatGPT app cannot consume leases directly.

## Capabilities and constraints

- Multiple isolated ChatGPT/Codex accounts with labels.
- One mutable encrypted `ACTIVE` lineage and at most one immutable `EXPORT` snapshot per account.
- Opaque credential checkpointing after every authenticated broker runtime, including failures and cancellation.
- Device-code and managed browser login; manual token import is retired.
- Authoritative short/weekly usage polling and stable preferred-account routing.
- Exact pool-reset wait responses; no weighted routing, prediction, or reservation.
- Opaque HTTP Responses and compaction proxy with bounded pre-output failover, cancellation, and memory-only request counters; no inference history, billing, or additional API surfaces.
- Machine leases contain access token, upstream account ID, public broker account ID, and expiry—never refresh tokens.
- SQLite with one owning process; AES-256-GCM vault envelopes; temporary plaintext runtime directories.
- One Orbit dashboard, persistent administrator sessions, CSRF, incidents, webhooks, and sanitized logs.
- Legacy activation controls/history are removed; fixed minimal ephemeral pulses stagger idle weekly windows across the eligible account pool.
- WCAG 2.2 AA target.

## Product principles

1. One owner for every mutable credential lineage.
2. Durable checkpoint before plaintext cleanup.
3. Fresh routing decision for every user turn.
4. Fail closed on credential, broker, or TLS uncertainty.
5. Keep lease and Responses APIs small; share transport helpers without abstracting account routing.
6. Preserve on-disk compatibility identifiers during rename and migration.
