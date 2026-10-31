# Usage refresh lanes

- Regular lane: 40 reads/hour, one every 90 seconds. Recompute the first four eligible accounts by displayed rotation order (weekly window start, then account ID), and walk them round-robin using a persisted account cursor. Exhaustion, quota blocks, and held state exclude accounts. Busy members and members in retry cooldown are skipped without admitting a fifth account.
- Daily lane: count enabled, verified accounts with active credentials, regardless of rotation or quota state. Space reads by 24 hours / count, selecting the oldest complete successful read among accounts currently safe to refresh. Busy accounts and failed attempts retain the existing retry protection.
- Both lanes use the existing refresh operation and successful-read timestamp. Persist independent deadlines in the singleton scheduler row; do not replay missed slots after downtime. No new worker, cache, or account queue is needed.
- Verify pacing, restart/concurrent claims, top-four replacement, daily ordering and status coverage, shared timestamps, and migration compatibility before deploying.
- The former reset/event background loop is replaced by the daily sweep. Manual reads and required credential/preflight operations remain available. Existing durable event requests are satisfied by successful scheduled reads; they do not trigger extra background reads.
