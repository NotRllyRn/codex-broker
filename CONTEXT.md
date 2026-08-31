# Windowkeeper glossary

## Account

A locally named representation of one authenticated ChatGPT identity and its usage history, scheduling, and activation state.

## Managed credential bundle

The single mutable credential lineage Windowkeeper uses for an account. Codex owns normal OAuth refresh behavior; Windowkeeper treats `auth.json` as opaque state and checkpoints it after every authenticated runtime.

## Downloadable credential bundle

An externally owned `auth.json` snapshot issued during enrollment when the account has no export. Windowkeeper never uses, refreshes, or replaces an existing export during reauthentication or normal operations. Its independent renewability is not guaranteed.

## Enrollment

Creation of an account from one ChatGPT approval. Windowkeeper durably captures the source, advances the managed lineage, and attempts one separate export snapshot without risking rollback of the managed credential.

## Reauthentication

Replacement and verification of the managed credential from a new ChatGPT approval. An existing export remains unchanged.

## Credential checkpoint

The quiesce, opaque capture, encryption, and atomic promotion performed before an authenticated runtime is deleted. Failures quarantine runtime evidence rather than discard potentially newer credentials.
