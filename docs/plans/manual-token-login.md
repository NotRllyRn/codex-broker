# Manual token login

## Status

Retired by [managed credential checkpointing](managed-credential-checkpointing.md).

New enrollment and reauthentication accept only ChatGPT device-code or browser OAuth. Windowkeeper no longer manufactures Codex's private `auth.json` token structure or accepts raw access/refresh tokens through the UI or service API.

Historical `MANUAL_TOKENS` enum and database values remain readable for migration compatibility. Existing encrypted ACTIVE credentials created by older versions continue through the same opaque managed-checkpoint lifecycle; no migration parses or rewrites their token fields.
