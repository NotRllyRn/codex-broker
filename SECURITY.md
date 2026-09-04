# Security policy

Report suspected credential exposure, OAuth callback bypass, vault/checkpoint failure, client-key bypass, TLS downgrade, or cross-account routing privately to the maintainers.

Retain only sanitized operation IDs, timestamps, and error codes. Never attach `auth.json`, access or refresh tokens, callback URLs, device codes, client keys, vault keys, administrator passwords, SQLite files, runtime directories, or unsanitized logs.

## Boundary

- Codex Broker is the sole owner of mutable refresh-token lineages.
- Machine clients receive short-lived access tokens and ChatGPT account IDs only.
- Browser mutations require an authenticated administrator session and CSRF token.
- Client keys are random, stored only as hashes, and accepted only by machine endpoints.
- Non-loopback service binding requires TLS. Clients must use system trust or the configured local CA; certificate verification must never be disabled.
- Webhooks require HTTPS and are redacted before storage.
- Runtime plaintext is isolated and removed only after safe checkpointing. Failed checkpoints preserve quarantined evidence.
- Downloadable exports are immutable external snapshots. Treat them as passwords and never give one rotating credential to multiple writers.

Revoking a broker client key blocks future leases but cannot claw back an access token already issued by OpenAI. That token can remain usable until expiry because Codex Broker intentionally is not an inference proxy.

A compromised host, root user, malicious Codex binary, or trusted client that exfiltrates a lease is outside the boundary.
