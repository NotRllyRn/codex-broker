# Repository guidance

Use generic labels such as "Personal usage", "Work laptop", or "Example user" in UI placeholders, documentation, and fixtures. Never copy real names or other personal details from user-provided examples into the product or repository.

## Managed Codex upgrade validation

When asked to test a managed Codex upgrade PR, follow the authenticated workflow
in `docs/managed-codex-upgrades.md` as well as checking CI and upstream changes.
Start `managed-codex-check` with that PR's exact candidate CLI and version, share
its device verification URL and code with the user, and keep the process running
while they sign in. It automatically continues with forced token renewal,
encrypted checkpoint/restore, quota reads, and a minimal window pulse. Report
actual results and missing checks; unchanged tokens after a refresh request are
inconclusive, not a pass. Request a fresh code if it expires.

This is an operator-assisted check when testing is requested, not an unattended
GitHub Actions login. Use a fresh isolated sign-in for the account the user has
authorized. Never copy production refresh tokens or use a production broker
database/runtime for this workflow. Keep credentials out of chat, logs, and git.
Keep supporting workflow fixes in a separate PR from the automation-owned
version-bump branch.
