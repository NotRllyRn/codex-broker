# Remove token forking

## Problem and scope

Enrollment currently refreshes the same OAuth source twice to create managed
and downloadable credentials. Different returned tokens do not establish
independent refresh lineages. Remove this unsupported branching and credential
downloads; retain one managed credential per account and normal Codex refresh.
This does not recover credentials already revoked upstream.

## Design

- Keep the existing login identity, workspace, duplicate-email, cancellation,
  and public-enrollment checks. After Codex exits, capture and promote the
  verified OAuth credential directly, then complete login and refresh usage.
- Delete candidate issuance, export helpers, download endpoints, metadata, and
  UI. Keep serialized managed refresh and checkpoint/quarantine behavior.
- Migration 024 deletes only legacy `EXPORT` rows and their unique index.
  Preserve `ACTIVE` and `RETIRED` rows, vault identifiers, and old migrations.
  Simplify vault-key rotation to handle active credentials only.
- No new credential abstraction, configuration switch, or relay protocol change.

## Implementation and verification

1. Simplify login and remove export surfaces and unused runtime helpers.
2. Add the migration and update operational/product documentation.
3. Exercise device and browser login, public enrollment, replacement login,
   cancellation, identity rejection, and checkpoint failure using a fake Codex
   app-server that rejects forced refresh. Assert original credentials survive
   unchanged and no export is created. Verify upgrade preserves managed rows,
   removes exports, and reopens idempotently. Check the removed endpoint rejects POST with 405 (the existing GET fallback).
4. Run the automated release gates, including race tests and client checks.
   Proxy fixtures copy one closed, empty migrated database instead of rerunning
   the historical migrations per test; migration coverage stays in store tests.
5. Commit and push the tested change with concise messages.

## Production rollout

1. Preserve each stack's local Compose configuration. Back up the live SQLite
   database consistently, preserve deployment secrets/configuration separately,
   and retain old image IDs for rollback.
2. Build and validate the new binary against a protected copy of production
   data before restarting production. Confirm migration, vault decryption,
   account listing, and unchanged active credential fingerprints.
3. Update and restart only the broker. Verify TLS liveness/readiness, schema,
   account state, representative authenticated requests, public enrollment
   availability, and relay synchronization over an observation period.
4. Only after broker stability, build/update and restart only the relay. Verify
   readiness, synchronization, authenticated forwarding, and stable restarts.
5. Restore the previous image and pre-migration backup if rollback is needed;
   never overwrite newer credential generations with an old snapshot blindly.
