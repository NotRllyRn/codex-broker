# Security policy

Report suspected credential exposure, OAuth callback bypass, vault/checkpoint failure, client-key bypass, TLS downgrade, or cross-account routing privately to the maintainers.

Retain only sanitized operation IDs, timestamps, and error codes. Never attach `auth.json`, access or refresh tokens, callback URLs, device codes, client keys, vault keys, administrator passwords, SQLite files, runtime directories, or unsanitized logs.

## Boundary

- Codex Broker is the sole owner of mutable refresh-token lineages.
- Lease clients receive short-lived access tokens and ChatGPT account IDs only. Responses proxy clients receive inference output; their `cbk_` keys are replaced with leased credentials upstream.
- Proxy inference traverses broker memory and is never logged or persisted. Request bodies are bounded to 32 MiB; upstream redirects, cookies, credential headers, and hop-by-hop headers are rejected or stripped. Request cancellation reaches upstream, and responses are never replayed after delivery begins.
- Browser mutations require an authenticated administrator session and CSRF token.
- Client keys are random, stored only as hashes, and accepted only by machine endpoints.
- The macOS adapter binds only to loopback, reads its broker client key from Keychain at startup, requires a bearer on Responses requests, discards the caller bearer, and keeps leased credentials and traffic in memory. A malicious same-user process is outside this boundary.
- Non-loopback service binding requires TLS. Clients must use system trust or the configured local CA; certificate verification must never be disabled.
- Webhooks require HTTPS and are redacted before storage.
- Runtime plaintext is isolated and removed only after safe checkpointing. Failed checkpoints preserve quarantined evidence.
- Downloadable exports are immutable external snapshots. Treat them as passwords and never give one rotating credential to multiple writers.

Revoking a broker client key blocks new leases and proxy requests. It does not terminate an already authenticated proxy stream or claw back an access token already issued by OpenAI; that token can remain usable until expiry.

A compromised host, root user, malicious Codex binary, or trusted client that exfiltrates a lease is outside the boundary.
