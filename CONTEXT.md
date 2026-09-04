# Codex Broker glossary

## Account

A locally named authenticated ChatGPT identity with an isolated managed credential and usage history.

## Managed credential bundle

The only mutable credential lineage for an account. Codex Broker treats `auth.json` as opaque state and checkpoints it after every broker-owned authenticated runtime. Pi, Hermes, and other clients never receive its refresh token.

## Downloadable credential bundle

An immutable, externally owned `auth.json` snapshot created during enrollment when no export exists. Codex Broker never uses, refreshes, or replaces it during normal operation. Its independent renewability is not guaranteed.

## Credential checkpoint

Quiesce the Codex runtime, capture `auth.json`, encrypt it, and atomically promote it before deleting plaintext. Failure quarantines runtime evidence.

## Lease

An in-memory access token, ChatGPT account ID, account public ID, and expiry returned to an authenticated machine client. A lease never includes a refresh token.

## Pool wait

A route response stating that every eligible account is exhausted and giving the exact earliest reset time plus configured padding.

## Preferred account

A non-secret account public ID sent by a client to preserve prompt-cache affinity. The broker still runs eligibility checks for every new user turn.
