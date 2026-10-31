# Usage refresh scheduling

> The regular and reset/event schedules below are superseded by [Usage refresh lanes](usage-refresh-lanes.md).

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

## Additional edge-case pass

| Case | Mitigation / verification |
| --- | --- |
| Missing, ambiguous, or out-of-range quota windows | Reject the observation; retain the last complete baseline, routing evidence, and successful-refresh timestamp. |
| Older read finishes after a newer one | Reject stale observations transactionally before changing samples or current usage. |
| Upstream still reports exhausted usage after reset, or omits its reset | Persist a recovery read with five-minute backoff rather than stranding the account or polling rapidly. |
| Manual refresh races a pulse, credential operation, or another refresh | Recheck eligibility and unfinished operations inside the claim transaction; deduplicate existing refreshes. |
| Refresh waits behind runtime startup or a usage slot | Use the actual quota RPC start for grace checks, so a fresh read is not mistaken for one begun before the reset/error. |
| Quota rejection arrives during pulse preparation | Recheck the shared routing block before starting the turn. |
| Pulse discovers an old weekly window or lacks a reliable weekly reset | Preserve actual start times, do not move cadence backward, and do not pulse without a future weekly reset. |
| Disabled, deleted, unverified, busy, or credential-less accounts | Scheduler eligibility excludes them and workers recheck eligibility after waiting for a slot; retained event requests can resume after eligibility returns. |
| Outage, shutdown, checkpoint interruption, or failed database transaction | Keep requests/deadlines durable, roll back failed claims atomically, and use existing startup operation reconciliation and credential quarantine. No speculative success is recorded. |
| Both windows exhausted with different resets | A reset check may update one window, but routing and regular polling still require capacity in both windows. |
| Empty pool, all held/exhausted, restarts, simultaneous reset windows, repeated errors | Preserve pacing, skip regular reads, coalesce persistent event requests, and wait for fresh evidence. Existing regression coverage remains applicable. |
| Uneven reads, model switches, concurrency, late telemetry, rounding, resets, and broker pulses | Use observed deltas and unioned activity; keep mixed attribution unknown and exclude broker pulse consumption. Existing regression coverage remains applicable. |

Remaining limits: upstream reporting delay has no guaranteed upper bound; recovery reads therefore back off rather than assuming capacity. Traffic outside the broker cannot be reliably attributed to a model. Delays may exceed the grace period while credentials are busy or the service is offline. System clocks must be synchronized; backward clock jumps may delay reads until observations are ordered again, while the persisted scheduler does not replay missed slots after a forward jump. At 100 regular reads/hour, a pool of N eligible accounts takes approximately N/100 hours per round; event reads are additional. Already-started upstream calls can finish after an account is disabled, but routing still excludes it. The broker is designed for one running service per database; no distributed scheduler or speculative quota attribution is added.
