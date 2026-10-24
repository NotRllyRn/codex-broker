# Codex Broker

Codex Broker lets you combine multiple ChatGPT accounts into one 
shared pool, giving your apps access to higher usage limits without 
needing to manage each account separately.

It owns each account's refresh token, tracks usage limits, and
leases short-lived access tokens or proxies Responses requests. The dashboard
also caches per-account Codex profile activity, including lifetime tokens,
peak usage, streaks, longest tasks, the activity calendar, and account token shares.
Its usage panel estimates API costs for recorded proxy traffic using cached
LiteLLM list rates, with daily cost/token charts, cache savings, and cost splits
by token type and speed. Select an account and a 7-, 30-, or 90-day period.
These estimates exclude direct leases and historical profile tokens; they are
API-equivalent values, not subscription charges. Daily aggregates survive
response cleanup, starting with the response history available at upgrade.

**Usage identities** combine usage from multiple devices under a name such as
“Personal usage.” The native sync helper imports every agent supported by pinned
**ccusage 20.0.43**, including locally logged HTTP proxy requests. It uploads
compact daily JSON reports; repeated uploads update totals instead of adding
them again. Identity history survives seven-day proxy response cleanup.

## Collect usage from your devices

1. In **Settings → Usage identities**, create an identity and copy its generated
   `cbi_…` code. Use that code on each device; its name cannot authorize uploads.
2. With Node.js 22 or newer installed, run on each device:

   ```sh
   npx codex-broker@latest install --broker-url https://YOUR-BROKER
   ```

   `pnpm dlx codex-broker@latest install --broker-url https://YOUR-BROKER`
   also works. No manual release download or local build is needed.
3. Enter the identity code at the hidden prompt. Add `--ca /path/to/broker-ca.pem`
   for a private CA, or `--name 'Work laptop'` for a friendly device name.
   Node is needed only for setup; background syncing runs native executables.
4. On the dashboard, choose your identity, optionally a device, and **All time**.

The first sync imports all retained history. Hourly/startup/login runs upload
only changed daily reports, with durable recovery after outages and safe
retries. All supported agents and routes are included; no direct/proxy choices
are needed. Only numerical ccusage reports leave the device, not conversations.
Token and cost totals follow ccusage; individual response counts are unavailable
from aggregate reports. Different devices contribute independently.

See [device sync details](docs/usage-sync.md) for platform selection, private
certificates, custom log directories, scheduling, correction/recovery, updates,
and migration from the earlier Python helper.

If a device was installed with the wrong Broker address, follow the
[connection recovery steps](docs/usage-sync.md#wrong-broker-url-or-failed-first-sync).
Upgrade from `0.91.1` with `npx codex-broker@latest install --broker-url https://CORRECT-BROKER`
to repair the saved URL while preserving device state (fixed in `0.92.1`).

## Supported apps

| App | Integration | First step |
| --- | --- | --- |
| [Pi](https://github.com/badlogic/pi-mono) | Extension included in this repository | [Install the extension](#pi) |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) | Version-pinned maintained fork | [Run the installer](#hermes-agent) |
| [T3 Code](https://github.com/pingdotgg/t3code) | [`codex-broker` fork branch](https://github.com/NotRllyRn/t3code/tree/codex-broker) | [Build and connect the fork](#t3-code) |
| Responses-compatible apps | Built-in HTTP proxy | [Set the base URL and client key](#responses-compatible-apps) |
| ChatGPT for macOS (Codex) | Loopback Go adapter included in this repository | [Install the adapter](#chatgpt-for-macos) |

All integrations use existing revocable `cbk_` client keys. Leased access tokens
stay in memory; proxy clients receive only Responses output. Apps never receive
broker refresh tokens or a complete broker-managed `auth.json`.

## Client key account access

In **Settings → Client access keys**, choose label rules when creating a key or
open **Edit account access** on an active key. No rules means all accounts,
including unlabeled accounts; existing keys keep this access after upgrading.
An **Allow** rule requires a match with at least one allowed label. **Block**
excludes matching accounts and takes precedence over Allow. With only Block
rules, all other accounts remain accessible. Routing never falls back outside
these permissions when the permitted accounts are exhausted.

Manage labels on each account page. Adding or removing account labels changes
which scoped keys can use that account. See [scope design](docs/key-scopes.md).

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

## Optional high availability relay

Run one Relay Edge on a different host to keep new Codex requests working
during primary restarts and short outages. On the primary checkout:

```bash
scripts/bootstrap-relay.sh 192.168.1.21 https://192.168.1.20:8787
docker compose up -d --build broker
```

The broker URL must be the primary's direct address, not the HA frontend.

Copy only `deployment/relay/` to a checkout on the relay host. Do **not** copy
the primary `.env`, database, vault key, administrator password, enrollment
key, or CA private key. On the relay host:

```bash
docker compose -f compose.relay.yaml up --build -d
curl --cacert deployment/relay/ca.crt https://192.168.1.21:8787/health/ready
```

Point an external load balancer at the primary as preferred and the relay as
backup. The relay periodically receives all in-rotation short-lived leases,
active client-key hashes, quota metadata, and one expiring emergency-reserve
permit. Its disk cache and telemetry outbox are authenticated and encrypted
with the dedicated relay key. Terminal token/timing telemetry collected during
an outage is reconciled into the primary database after recovery; inference
content is never cached. See [Relay Edge deployment](docs/relay-edge.md).

## NixOS

Import `codex-broker.nixosModules.default` from this repository's flake, then
configure the service with a Codex package pinned to the supported version:

```nix
services.codex-broker = {
  enable = true;
  codex = myPinnedCodex; # Must provide codex-cli 0.164.0.
  vaultKeyFile = "/run/secrets/broker-vault";
  adminPasswordFile = "/run/secrets/admin-credential";
};
```

The listener defaults to `127.0.0.1:8787`. For LAN access, set `host`,
`tls.certFile`, and `tls.keyFile`; set `openFirewall = true` if needed.
Generate the vault key with `nix run . -- vault generate-key`. Provision secret
files outside the Nix store; systemd loads them as credentials. Custom users
and groups must already exist; the module creates state directories with mode
`0700`. The optional `publicEnrollment` service uses a separate dynamic user
and receives only its enrollment key and TLS credentials.

Build the broker with `nix build .`; validate the module with `nix flake check`.

## Connect an app

### Responses-compatible apps

For an app that supports a custom OpenAI Responses base URL, set:

```text
Base URL: https://192.168.1.20:8787/v1
API key:  cbk_… (create a separate named key for each app in Settings)
```

Include the configured root path before `/v1`, if any. Trust the broker's CA on
that client. The same listener supports Codex Responses and compaction, model
discovery, image generation and edits, dictation transcription, and thread-goal
reads and writes. Requests use broker-selected managed accounts; multipart
transcription bodies and query strings are preserved. Production checks on
2026-10-08 confirmed that ChatGPT Codex requires `stream:true` and currently
returns `404` for standalone compaction. This is a narrow Codex gateway, not
the full OpenAI API surface.

For example, using a key held in `BROKER_CLIENT_KEY`:

```bash
curl --cacert deployment/certs/ca.crt https://192.168.1.20:8787/v1/responses \
  -H "Authorization: Bearer $BROKER_CLIENT_KEY" \
  -H 'Content-Type: application/json' \
  -H 'X-Codex-Broker-Thread-ID: thread-123' \
  -H 'X-Codex-Broker-Turn-ID: turn-456' \
  -d '{"model":"YOUR_CODEX_MODEL","input":"Hello","store":false,"stream":true}'
```

The optional broker-only thread and turn headers associate all model and tool
iterations with their originating conversation and user turn. Keep each value
stable for its scope. The broker records terminal token usage and timing but
strips both headers before contacting OpenAI.

The broker preserves Codex Responses events and prefixes each streamed final
answer with the selected account and cached remaining quota:

```text
🔨 Account · 5h 70% 4h36m · 7d 95% 3d17h
```

Transparent account retries and failovers add `· ↻ retry` or `· ↻ failover`.
The injected line is removed from replayed input before the next upstream turn.
Responses and standalone compaction requests at or above 56 MiB are reduced to
at most 50 MiB before routing by replacing old inline media, truncating old tool
output, and finally removing complete old turns. The newest user turn, opaque
reasoning and compaction state, and tool-call/result pairing are protected;
requests that cannot be reduced safely receive `413` without an upstream attempt.
Compaction, models, images, transcription, thread goals, and non-streaming
Responses pass through without a notice. Your app must send requests supported
by ChatGPT Codex; the gateway does not translate Chat Completions or emulate
unsupported features. WebSocket Responses handshakes receive `426` so Codex
immediately falls back to HTTP streaming.

Routing prefers accounts already in rotation. When none has usable capacity,
it activates an eligible held account immediately, bypassing the staggered
schedule. Use **Pulse** on a dashboard account card or account page to send a
small request and start that account's rotation manually. Pulse history records
the result; depleted, disabled, or busy accounts cannot start a new pulse.

The dashboard shows active requests, recent client keys, request totals,
account failovers, and per-model rolling-seven-day and lifetime proxy usage.
The pool usage rings show equally weighted usage percentages per account.
The 5-hour ring counts quota-blocked or weekly-full accounts as 100% used,
even if their short-window reading is zero or missing; disabled and
unauthenticated accounts are excluded. The weekly ring uses all valid weekly
readings. Other missing or invalid readings are omitted, and cached readings
may lag upstream usage. A shared availability count shows enabled, verified
accounts with measured short-window capacity that are neither quota-blocked
nor weekly-full. Proxy usage keeps secondary metrics under **More metrics**.
Account-level quota estimates divide measured percentage-point consumption by
active proxy hours, using existing usage polls. Rolling estimates cover intervals
ending in the last seven days; lifetime estimates retain their totals after
response details expire. Overlapping responses count once per account.
One model selector controls tokens/hour and short/weekly quota/hour in both
periods. Model quota estimates use only intervals with one known model;
mixed/unknown intervals remain in All models. Model quota lifetime starts when
attribution is enabled; existing account/token totals are preserved.
Usage limits are polled hourly by default; profile statistics are refreshed at
most once every six hours. Known short and weekly resets also schedule a usage
refresh after a two-minute grace period. Resets due together share one refresh.
Responses use the token counts returned in their
streams and do not trigger usage polls. Quota rejections temporarily exhaust
an account for every client, regardless of its cached percentages, until the
first known short/weekly reset or a successful fresh usage poll.
Detailed response telemetry is retained for seven days; compact lifetime
counters remain. When the pool has a known retry time, requests
return `429` with `Retry-After` immediately. No prompt or response content is
retained.
The lease API remains available for Pi, Hermes, and T3 Code.

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
- [Managed Codex upgrades](docs/managed-codex-upgrades.md) — automated release detection, upgrade PRs, and GitHub Actions setup
- [ChatGPT macOS adapter](docs/integrations/chatgpt-macos.md) — local installation and Codex provider configuration
- [Public enrollment](docs/public-enrollment.md) — isolated device-code enrollment
- [Relay Edge deployment](docs/relay-edge.md) — active/passive high availability
- [API contract](plan.md) — authenticated machine endpoints and routing responses
- [Security policy](SECURITY.md) — vulnerability reporting and security boundary

Codex Broker cannot protect credentials on a compromised broker host or prevent
a trusted client from copying an already-leased access token.
