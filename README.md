# Codex Windowkeeper

Windowkeeper is a single-instance supervisor for independently authenticated ChatGPT/Codex accounts. It reads authoritative short and weekly usage windows from the managed Codex app-server, waits while either limit is exhausted, schedules one evidence-backed activation when usage returns, and makes genuinely ambiguous submissions visible instead of retrying blindly.

## What ships

- Isolated runtime and encrypted managed credential lineage per account.
- One-approval enrollment that creates a managed credential and one externally owned `auth.json` export snapshot.
- Device-code sign-in (recommended) and managed browser OAuth.
- Opaque `auth.json` checkpointing after every authenticated Codex runtime, including failed operations.
- Immediate first-window activation, one in-flight activation per account, automatic short/weekly limit recovery, and reported-reset scheduling with a duration fallback when an idle reset moves forward on every poll.
- Activation discovers account-available models and pins the cheapest officially priced text model at its lowest effort and standard service tier.
- Five switchable dashboard compositions—Orbit, Ledger, Rail, Timeline, and Focus—in light and dark themes.
- Durable operations, incidents, SSE updates, sanitized JSONL logs, and numbered, account-specific generic/Slack/Discord webhooks with causes and recovery steps.
- SQLite, AES-256-GCM, opaque administrator sessions, CSRF, recent-password confirmation, and managed Codex availability checks.

Windowkeeper does **not** pool quota, route work between accounts, expose a public API, or call undocumented usage endpoints.

## Local development

Requires Python 3.12+, `uv`, and a separately installed Codex executable.

```bash
uv sync --all-extras
uv run ruff check src tests
uv run mypy src tests
uv run pytest
```

Create a private repository-local `.env` file—no shell exports required:

```bash
cp .env.example .env
chmod 600 .env
uv run windowkeeper vault generate-key  # paste the result into WINDOWKEEPER_VAULT_KEY
# set WINDOWKEEPER_ADMIN_PASSWORD in .env, then:
uv run windowkeeper serve
```

Windowkeeper loads `.env` automatically. The file is ignored by Git and excluded from Docker build contexts; never commit it.

A service is ready once the vault and administrator password are configured and the managed Codex executable starts successfully.

## Hardened Docker Compose

1. Run `cp .env.example .env && chmod 600 .env`.
2. Fill in the administrator password and generated vault key.
3. Run `docker compose up --build -d`. Compose reads `.env` automatically.

```bash
cp .env.example .env
chmod 600 .env
uv run windowkeeper vault generate-key  # paste into .env
docker compose up --build -d
```

The image installs and manages Codex automatically. Its entrypoint uses only `CHOWN`, `SETGID`, and `SETUID` to initialize mounted-volume ownership, immediately drops to UID/GID 10001, and runs with a read-only root filesystem, no-new-privileges, bounded tmpfs runtime trees, and a persistent `/data` volume. `/health/live` checks the process; `/health/ready` also requires the vault, administrator password, and working Codex executable.

### OAuth deployment modes

- `manual` (default): device code works from Docker/NAS/SSH. Browser sign-in displays a one-time authorization URL, then securely accepts the exact localhost callback URL in the authenticated UI. OAuth query values are never persisted or logged.
- `host-loopback`: Linux-only automatic browser callback mode. Start with `docker compose -f compose.host-network.yaml up --build`; keep the service bound to loopback or behind a trusted reverse proxy.
- `disabled`: browser login is unavailable; device code remains available.

Only one browser attempt may own the callback-port set. Device-code attempts remain isolated and obey the configured authentication concurrency bound.

## Operations

See [OPERATIONS.md](OPERATIONS.md) for initialization, OAuth modes, backup/restore, reverse-proxy configuration, incident response, upgrades, and rollback. Release operators must also complete [RELEASE_GATES.md](RELEASE_GATES.md).

```bash
windowkeeper --version
windowkeeper version --json
windowkeeper health --json
windowkeeper status --json
windowkeeper doctor
windowkeeper backup --output /secure/backups/windowkeeper.sqlite
windowkeeper restore --input /secure/backups/windowkeeper.sqlite --confirm RESTORE
windowkeeper vault rotate --old-key-file /secure/old.key --new-key-file /secure/new.key
windowkeeper password-set
windowkeeper vault verify --key-file /secure/current.key
```

Vault rotation is offline, all-or-nothing, and re-encrypts managed credentials, downloadable auth bundles, and webhook URLs/signing secrets before writing the new key file. Replace the configured key file atomically only after the command succeeds.

The dashboard provides password-reauthenticated download of the enrollment `auth.json` snapshot, account enable/disable, one-approval reauthentication, typed-confirmation deletion, manual refresh/activation, operation history, incident state, webhook management, log filtering, and sanitized JSONL download. Normal operations advance only the managed credential and never replace the externally owned export. Activation records the selected model, reasoning effort, standard service tier, and pricing verification date in its durable operation result.

## Security boundary

The vault key must not live in SQLite or the persistent data directory. Runtime credential files exist only under the runtime tmpfs and are removed after their credential state is safely checkpointed. A failed checkpoint quarantines the runtime instead of deleting potentially newer credentials. URL query strings, callback values, device codes, tokens, authorization headers, and known token-shaped strings are redacted before logs, SSE, API responses, incidents, or webhooks.

Windowkeeper trusts its managed Codex child process with plaintext credentials while that isolated process is running. It does not protect against a compromised host, root user, or malicious Codex binary.

## UI evaluation

Use the **View** selector in the header to switch among all five mockups without losing filters. Use the adjacent theme control for system, light, or dark mode. Both choices are stored locally in the browser; the server view-model and functionality remain identical.
