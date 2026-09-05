# Codex Broker

Codex Broker is a central Codex authentication, quota, and account-routing service. It owns the only mutable OAuth credential for each account, tracks short and weekly usage windows, and leases access tokens to trusted LAN clients without exposing refresh tokens.

## What ships

- Encrypted, isolated credential lineage per ChatGPT/Codex account.
- Device-code and managed browser sign-in.
- Opaque `auth.json` checkpointing after every broker-owned authenticated runtime.
- Stable account routing with preferred-account affinity and exact pool-reset responses.
- Hashed, revocable client keys for the machine API.
- A small Pi extension under [`packages/pi-extension`](packages/pi-extension/README.md).
- A version-pinned Hermes fork submodule under [`integrations/hermes-agent`](integrations/hermes-agent), with its implementation specification retained under [`docs/integrations/hermes-agent-patch.md`](docs/integrations/hermes-agent-patch.md).
- One Orbit dashboard, persistent administrator sessions, CSRF protection, incidents, webhooks, and sanitized logs.
- Direct TLS for same-network deployment.

Codex Broker is a control plane, not an inference proxy. Clients call Codex directly with short-lived leased access tokens. Revoking a broker key prevents future leases but cannot revoke an already-issued upstream token before its expiry.

The former automatic activation/model-turn scheduler is intentionally removed. Broker-owned model turns spend quota, complicate credential mutation, and are not required for routing.

## Local development

Requires Python 3.12+, `uv`, and a compatible `codex` executable.

```bash
uv sync --all-extras
uv run ruff check src tests
uv run pyright src tests
uv run pytest
```

The historical `WINDOWKEEPER_*` configuration prefix and storage names remain compatibility identifiers so existing installations upgrade without relogin.

```bash
cp .env.example .env
chmod 600 .env
uv run codex-broker vault generate-key
# Set WINDOWKEEPER_VAULT_KEY and WINDOWKEEPER_ADMIN_PASSWORD in .env.
uv run codex-broker serve
```

Loopback HTTP is allowed for development. Non-loopback binding requires a TLS certificate and private key.

## Secure LAN deployment

Generate a local CA, server certificate, administrator password, vault key, and `.env`, then start Compose:

```bash
scripts/bootstrap.sh 192.168.1.20
docker compose up --build -d
```

Install `deployment/certs/ca.crt` in every client host's trust store, or configure the Pi/Hermes CA-file variable. Never disable certificate verification. The service communicates over local IP addresses, but TLS still prevents passive credential capture and detects man-in-the-middle endpoints.

Create a client key and copy the secret once:

```bash
codex-broker client-key create "Pi desktop"
```

Machine endpoints:

- `POST /api/v1/route` — select an eligible account and return an access-only lease, or an exact wait response.
- `GET /api/v1/health` — authenticated broker readiness.

See [`plan.md`](plan.md) for the API contract and [`OPERATIONS.md`](OPERATIONS.md) for backup, recovery, upgrades, and incident response.

## Pi

```bash
pi install ./packages/pi-extension
```

Run `/broker-status` in Pi to configure the broker URL, API token, and CA certificate path. The extension requests one in-memory lease per user turn and never stores refresh tokens or complete `auth.json` payloads.

## Hermes

Hermes requires a small core integration because its plugin hooks fail open at the credential boundary. The tested fork is pinned as a Git submodule and can be installed on a compatible Git-based Hermes installation with [`scripts/install-hermes-integration.sh`](scripts/install-hermes-integration.sh). See [`docs/integrations/hermes-agent.md`](docs/integrations/hermes-agent.md) for installation, exact version pins, and the rebase workflow.

## Operations

```bash
codex-broker --version
codex-broker health --json
codex-broker status --json
codex-broker doctor
codex-broker backup --output /secure/backups/windowkeeper.sqlite
codex-broker restore --input /secure/backups/windowkeeper.sqlite --confirm RESTORE
codex-broker vault rotate --old-key-file /secure/old.key --new-key-file /secure/new.key
codex-broker password-set
```

The administrator can enroll, reauthenticate, enable, disable, refresh, and delete accounts; download the immutable manual export; manage client keys and webhooks; inspect operations/incidents; and export sanitized logs. Password prompts are limited to sign-in and password change. Other browser mutations require the administrator session and CSRF token.

## Security boundary

The vault key must stay outside SQLite and the persistent data directory. Runtime plaintext exists only in isolated temporary directories and is removed after safe checkpointing. Failed checkpoints quarantine the runtime rather than deleting potentially newer credentials. Tokens, authorization headers, callback values, device codes, and URL query strings are redacted.

Codex Broker does not protect against a compromised host, root user, malicious Codex binary, or a trusted client that exfiltrates its leased access token.
