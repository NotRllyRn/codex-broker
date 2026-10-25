# Streamer modes (issue #3)

## Design

Keep two independent, opt-in controls. Dashboard streamer mode belongs to this
browser and persists in a cookie scoped to the application's root path. Render
sensitive dashboard labels generically on the server, including account names,
labels, client keys, identities, and devices. Mask live profile/proxy labels too.
Reload on toggle so navigation and refreshes never flash the original labels.
Apply browser privacy only to the accounts dashboard. Settings and other pages
keep their normal names and controls; the toggle appears only on the dashboard.
This is presentation privacy; it does not change stored data or API permissions.

Client-key streamer mode is a SQLite boolean defaulting to false for existing and
new keys. An authenticated, CSRF-protected settings action changes it. Carry the
flag through primary and relay authentication into the shared gateway. Replace
both account label and fallback account ID in the injected response notice with
“Usage stats”, retaining quota/reset/retry information and internal telemetry.
Relay changes take effect after snapshot synchronization, like access scopes.

## Implementation and validation

1. Add migration, key persistence/settings, and gateway/relay propagation.
2. Add dashboard toggle and generic rendering across templates and live updates.
3. Cover defaults, persistence, authorization, relay propagation, notice privacy,
   template rendering, browser persistence, and dynamic labels with regressions.
4. Run Go race tests/vet/build and JavaScript/TypeScript checks. Inspect the diff
   for missed identity surfaces and unsafe form behavior; fix review findings.
5. Open a PR closing #3, inspect CI, merge after checks pass, and sync main.
