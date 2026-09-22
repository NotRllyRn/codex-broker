# Codex Broker

Codex Broker lets you combine multiple ChatGPT accounts into one 
shared pool, giving your apps access to higher usage limits without 
needing to manage each account separately.

It owns each account's refresh token, tracks usage limits, and
leases short-lived access tokens. The dashboard also caches per-account Codex
profile activity and presents combined token, streak, task, skill, and account
share statistics without requiring a fresh upstream read after restart.

## Supported apps

| App | Integration | First step |
| --- | --- | --- |
| [Pi](https://github.com/badlogic/pi-mono) | Extension included in this repository | [Install the extension](#pi) |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) | Version-pinned maintained fork | [Run the installer](#hermes-agent) |
| [T3 Code](https://github.com/pingdotgg/t3code) | [`codex-broker` fork branch](https://github.com/NotRllyRn/t3code/tree/codex-broker) | [Build and connect the fork](#t3-code) |
| ChatGPT for macOS (Codex) | Loopback Go adapter included in this repository | [Install the adapter](#chatgpt-for-macos) |

All integrations use in-memory access-only leases. They never store broker
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

### ChatGPT for macOS

On the Mac, run the loopback-adapter installer with the broker HTTPS origin and
its CA certificate. The installer securely prompts for a dedicated client key:

```bash
scripts/install-macos-adapter.sh \
  https://192.168.1.20:8787 \
  /path/to/codex-broker-ca.crt
```

Then point the built-in OpenAI provider's `openai_base_url` at the adapter, set
the documented realtime overrides so Voice keeps its native OpenAI routes, and
restart the ChatGPT app. This routes local **Codex** Responses traffic through
the broker while preserving native ChatGPT authentication for Voice and other
account features. See the
[ChatGPT macOS guide](docs/integrations/chatgpt-macos.md) for the exact provider
configuration, Keychain behavior, verification, and removal steps.

## Local development

Requires Go 1.27+, Node.js/npm for the Pi extension tests, and a compatible
`codex` executable.

```bash
cp .env.example .env
chmod 600 .env
go run ./cmd/codex-broker vault generate-key
# Set WINDOWKEEPER_VAULT_KEY and WINDOWKEEPER_ADMIN_PASSWORD in .env.
go run ./cmd/codex-broker serve
```

Loopback HTTP is allowed for development. Non-loopback binding requires TLS.

```bash
gofmt -w cmd internal
go vet ./...
go test -race ./...
npm install --prefix packages/pi-extension
npm test --prefix packages/pi-extension
npm run check --prefix packages/pi-extension
```

## More documentation

- [Operations](OPERATIONS.md) — health, backups, upgrades, and incident response
- [ChatGPT macOS adapter](docs/integrations/chatgpt-macos.md) — local installation and Codex provider configuration
- [Public enrollment](docs/public-enrollment.md) — isolated device-code enrollment
- [API contract](plan.md) — authenticated machine endpoints and routing responses
- [Security policy](SECURITY.md) — vulnerability reporting and security boundary

Codex Broker cannot protect credentials on a compromised broker host or prevent
a trusted client from copying an already-leased access token.
