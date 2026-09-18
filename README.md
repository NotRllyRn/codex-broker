# Codex Broker

Codex Broker securely shares a pool of ChatGPT/Codex accounts with trusted apps
on your network. It owns each account's refresh token, tracks usage limits, and
leases short-lived access tokens without proxying model traffic.

## Supported apps

| App | Integration | First step |
| --- | --- | --- |
| [Pi](https://github.com/badlogic/pi-mono) | Extension included in this repository | [Install the extension](#pi) |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) | Version-pinned maintained fork | [Run the installer](#hermes-agent) |
| [T3 Code](https://github.com/pingdotgg/t3code) | [`codex-broker` fork branch](https://github.com/NotRllyRn/t3code/tree/codex-broker) | [Build and connect the fork](#t3-code) |

All integrations request one in-memory lease per turn. They never store broker
refresh tokens or a complete broker-managed `auth.json`.

## Start the broker

For a TLS-protected LAN deployment, install Docker and Compose, then run:

```bash
git clone https://github.com/NotRllyRn/codex-broker.git
cd codex-broker
scripts/bootstrap.sh 192.168.1.20
docker compose up --build -d
```

Replace the example IP with the broker host's LAN IP. Open
`https://192.168.1.20:8787`, sign in with the generated administrator password,
add at least one Codex account, and create a client key in **Settings**.

Install `deployment/certs/ca.crt` on each client host. Keep `.env`, private keys,
and the client-key secret private. Never expose broker port `8787` to the
Internet. See [Operations](OPERATIONS.md) for backups, upgrades, recovery, and
hardening.

## Connect an app

### Pi

From this repository checkout:

```bash
pi install ./packages/pi-extension
```

Run `/broker-status` in Pi and enter the broker HTTPS URL, client key, and CA
certificate path. See the [Pi extension guide](packages/pi-extension/README.md).

### Hermes Agent

Install Hermes normally, then apply the pinned integration:

```bash
curl -fsSL https://raw.githubusercontent.com/NotRllyRn/codex-broker/main/scripts/install-hermes-integration.sh | sudo -E sh
```

In a private administrator chat, run:

```text
/broker-status set <url> <client-key> <ca-path>
```

Delete that message afterward. See the
[Hermes Agent guide](docs/integrations/hermes-agent.md) for supported versions
and safer script inspection.

### T3 Code

Build the supported fork's `codex-broker` branch, then add the broker URL, client
key, and optional private-CA certificate to the Codex provider in
**Settings → Providers**. A local `codex login` is not required.

See the [T3 Code guide](docs/integrations/t3-code.md) for the build commands and
exact environment variables.

## Local development

Requires Python 3.12+, [`uv`](https://docs.astral.sh/uv/), and a compatible
`codex` executable.

```bash
uv sync --all-extras
cp .env.example .env
chmod 600 .env
uv run codex-broker vault generate-key
# Set WINDOWKEEPER_VAULT_KEY and WINDOWKEEPER_ADMIN_PASSWORD in .env.
uv run codex-broker serve
```

Loopback HTTP is allowed for development. Non-loopback binding requires TLS.

```bash
uv run ruff check src tests
uv run pyright src tests
uv run pytest
```

## More documentation

- [Operations](OPERATIONS.md) — health, backups, upgrades, and incident response
- [Public enrollment](docs/public-enrollment.md) — isolated device-code enrollment
- [API contract](plan.md) — authenticated machine endpoints and routing responses
- [Security policy](SECURITY.md) — vulnerability reporting and security boundary

Codex Broker cannot protect credentials on a compromised broker host or prevent
a trusted client from copying an already-leased access token.
