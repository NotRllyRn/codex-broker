# Usage refresh scheduling

- Pace regular reads at one every 36 seconds (100/hour across the pool), selecting the oldest successful read among idle, enabled, verified accounts in rotation with capacity. Every successful read shares the existing timestamp; failures retain it and back off five minutes.
- Persist the regular deadline and coalesced event requests. Refresh 45 seconds after quota errors or either reset, retry failures, and keep routing blocked until fresh evidence. Do not routinely poll idle held accounts.
- Read usage before a scheduled held-account pulse. Reconcile existing weekly windows using their actual start, then reconsider the next held candidate against the global cadence. Refresh other rotation transitions.
- Preserve measured quota deltas and union of actual model activity. Audit irregular sampling, overlapping streams, model changes, delayed reports and resets; add regression coverage without inventing attribution.
- Run targeted tests, full tests and race checks, review the final diff, commit focused changes and push to main.

## Audit notes

- First pass: use the RPC observation time before credential checkpointing; exclude pulse consumption from client-model quota estimates. Preserve unioned activity spans, weighted rates, reset rejection, and conservative mixed-model attribution.
- Second pass: reset requests use the reset-plus-grace deadline so an in-flight manual read can satisfy them. Event retries have an independent persisted deadline so incomplete successful responses cannot spin. Reject stale cached exhaustion until a fresh read; preserve unknown-reset error handling. Recover resets claimed by the old poller without a successful post-reset read, so upgrades cannot strand exhausted accounts.
- Regression coverage: 50/100-account fairness, restart pacing, concurrent claims, successful-read timestamps for every trigger, failed/incomplete retry backoff, rejection coalescing, reset/read races, externally started held windows and cadence, uneven sampling, late overlapping model activity, checkpoint delays, and pulse attribution.
- Full `go test ./...` passes. Targeted race tests cover the final event retry and concurrent claim changes; broader broker, telemetry, and HTTP race checks validate the implementation.
