# Product

<!-- impeccable:product-schema 1 -->

## Platform

Web service with a browser administration UI and authenticated machine API.

## Users

Codex Broker serves a self-hosting operator who administers multiple ChatGPT/Codex identities they own or are authorized to manage. Pi, Hermes, and future trusted LAN clients consume broker-issued access-only leases.

## Product purpose

Prevent independent Codex clients from racing or invalidating rotating OAuth refresh tokens. Codex Broker is the single mutable credential authority, tracks authoritative usage windows, and routes each new user turn to an eligible account.

## Operating context

One hardened Docker-first Python process runs on a trusted Linux host. Same-network clients connect by local IP over verified TLS and authenticate with hashed, revocable broker client keys. Clients call Codex directly with leased access tokens; the broker remains a control plane.

## Capabilities and constraints

- Multiple isolated ChatGPT/Codex accounts with labels.
- One mutable encrypted `ACTIVE` lineage and at most one immutable `EXPORT` snapshot per account.
- Opaque credential checkpointing after every authenticated broker runtime, including failures and cancellation.
- Device-code and managed browser login; manual token import is retired.
- Authoritative short/weekly usage polling and stable preferred-account routing.
- Exact pool-reset wait responses; no weighted routing, prediction, reservation, or inference proxy.
- Machine leases contain access token, upstream account ID, public broker account ID, and expiry—never refresh tokens.
- SQLite with one owning process; AES-256-GCM vault envelopes; temporary plaintext runtime directories.
- One Orbit dashboard, persistent administrator sessions, CSRF, incidents, webhooks, and sanitized logs.
- Legacy activation controls/history are removed; a fixed minimal ephemeral pulse keeps idle short/weekly windows active.
- WCAG 2.2 AA target.

## Product principles

1. One owner for every mutable credential lineage.
2. Durable checkpoint before plaintext cleanup.
3. Fresh routing decision for every user turn.
4. Fail closed on credential, broker, or TLS uncertainty.
5. Prefer minimal control-plane APIs over a data-plane proxy.
6. Preserve on-disk compatibility identifiers during rename and migration.
