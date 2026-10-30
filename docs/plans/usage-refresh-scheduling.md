# Usage refresh scheduling

- Pace regular reads at one every 36 seconds (100/hour across the pool), selecting the oldest successful read among idle, enabled, verified accounts in rotation with capacity. Every successful read shares the existing timestamp; failures retain it and back off five minutes.
- Persist the regular deadline and coalesced event requests. Refresh 45 seconds after quota errors or either reset, retry failures, and keep routing blocked until fresh evidence. Do not routinely poll idle held accounts.
- Read usage before a scheduled held-account pulse. Reconcile existing weekly windows using their actual start, then reconsider the next held candidate against the global cadence. Refresh other rotation transitions.
- Preserve measured quota deltas and union of actual model activity. Audit irregular sampling, overlapping streams, model changes, delayed reports and resets; add regression coverage without inventing attribution.
- Run targeted tests, full tests and race checks, review the final diff, commit focused changes and push to main.
