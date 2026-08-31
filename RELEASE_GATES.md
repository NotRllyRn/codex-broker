# Release gates

Windowkeeper installs Codex as part of its image and checks it automatically at startup. This file separates repository checks from evidence that can only be produced in the release environment.

## Repository checks

Run on every change:

```bash
uv sync --all-extras --locked
uv run ruff check src tests
uv run ruff format --check src tests
uv run mypy src tests
uv run pyright
uv run pytest
uv run pip-audit
uv build
```

The automated suite covers migrations and credential-generation preservation, singleton refusal, vault mismatch and rotation, opaque credential checkpointing after successful and failed RPCs, immutable export snapshots, fresh runtime/config policy, transport EOF handling, manual-token retirement, credential/webhook encryption, numbered provider-native webhook rendering and account context, OAuth callback validation, redaction, CSRF checks, administrator throttling, all five UI layouts, light/dark controls, immediate and rolling-reset scheduling, real Codex terminal-event parsing, paginated lowest-cost model selection, explicit standard-tier activation, activation deduplication, stale-plan cancellation, tool-item rejection, auth-failure/checkpoint incidents, reauthentication recovery, durable backup/restore, log repair/rotation, and restart reconciliation from upstream turn evidence.

CI uses commit-SHA-pinned actions and publishes multi-architecture images with SBOM, provenance, and GitHub build attestation for version tags.

## Release-environment evidence

Do not publish a release until all of the following are recorded for the Codex package installed in the image:

- Exact Codex package version, `codex --version` output, executable SHA-256, initialization schema, login methods, callback ports, credential-file allowlist, usage shape, model catalog shape, official credit-rate manifest, persistent-thread behavior, and no-tool activation profile.
- Device-code enrollment for at least two isolated real accounts and browser OAuth in each enabled deployment mode.
- One-time ACTIVE/EXPORT issuance evidence, including failure after managed rotation, immutable export bundle identity across normal operations, and explicit acknowledgment that independent export renewability is unsupported.
- Correct account attribution for short and weekly usage reads.
- The complete activation crash/fault-injection matrix, including pre-write, partial write, accepted response, notification loss, checkpoint failure, restart reconciliation, and proof that no accepted logical window is duplicated.
- Secret-canary scans across `/data`, runtime cleanup, logs, HTML/JSON, SSE, incidents, and webhook bodies.
- A 25-account soak covering polling, scheduling, cancellation, shutdown, process cleanup, bounded tasks, database integrity, and latency targets.
- Chromium accessibility and responsive smoke tests for all five layouts in light and dark modes.
- Native or emulated `linux/amd64` and `linux/arm64` image startup, readiness, device login, activation safety, backup/restore, and vulnerability scan.
- Registry signature/attestation verification and an approved exception for any remaining non-fixable high or critical image vulnerability.

Operators do not configure Codex versions or digests. Record the observed release evidence with the image; `windowkeeper doctor` and `/health/ready` must remain blocked when the managed executable is missing or cannot start.
