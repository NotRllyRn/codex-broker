# ChatGPT macOS adapter

The `codex-broker-adapter` lets local Codex chats in the ChatGPT macOS app use
the accounts managed by Codex Broker. It is a loopback Responses API override,
not a network interceptor:

```text
ChatGPT app -> http://127.0.0.1:8789/v1/responses
            -> Codex Broker /api/v1/route
            -> ChatGPT Codex backend
```

The built-in OpenAI provider and ChatGPT sign-in remain active. The base URL
override affects both Codex Responses and Voice unless realtime destinations
are set separately. The configuration below sends only local Codex Responses
through the adapter and keeps Voice on its native OpenAI connections.

## Install

Requirements:

- macOS and Go 1.27 or newer;
- a running HTTPS Codex Broker;
- its CA certificate when it uses the repository's private CA; and
- a dedicated broker client key created in **Settings** or with
  `codex-broker client-key create "MacBook Codex"`.

From this repository checkout on the Mac, run:

```bash
scripts/install-macos-adapter.sh \
  https://192.168.1.20:8787 \
  /path/to/codex-broker-ca.crt
```

The installer prompts without echoing for the client key. It builds the Go
binary, stores the key in macOS Keychain, installs a user LaunchAgent named
`dev.codex-broker.adapter`, starts it, and verifies authenticated broker health.
Omit the CA argument when the broker certificate is already trusted by macOS.

The adapter listens only on `127.0.0.1:8789`. At startup it reads the client key
from Keychain, exchanges it for in-memory access-only leases, and never stores
an access token, refresh token, request, or response.

To upgrade an existing installation, pull the latest repository and rerun the
same installer command. It reuses the existing Keychain item when available,
replaces the binary and LaunchAgent, and verifies the restarted service.

## Connect the ChatGPT app

Open **ChatGPT → Settings → Configuration → Open config.toml** and set these
user-level values in `~/.codex/config.toml`:

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8789/v1"
experimental_realtime_webrtc_call_base_url = "https://chatgpt.com/backend-api/codex"
experimental_realtime_ws_base_url = "https://api.openai.com/v1"
```

The two realtime keys are experimental Codex settings and may change between
desktop releases; re-run the live Voice gate after updating ChatGPT.

Remove the old `[model_providers.codex_broker]` and
`[model_providers.codex_broker.auth]` tables if present. Keep your existing
`model` setting. Stay signed into the eligible ChatGPT account that provides
the desktop features you use, completely quit the app, reopen it, select
**Codex**, and start a local project chat.

The app sends its native bearer to the loopback endpoint. The adapter requires
a bearer-authenticated request but discards that credential, asks the broker
for an account lease with its Keychain key, and replaces the upstream identity.
Without the two realtime overrides, `openai_base_url` also redirects Voice call
creation to the adapter as `POST /v1/live`, which the adapter intentionally does
not implement. The overrides restore Codex's native split: WebRTC call creation
uses the ChatGPT backend and realtime audio/control uses OpenAI's WebSocket
service. Voice therefore uses the account signed into ChatGPT, while local
Codex Responses and Voice-delegated Codex turns use broker-selected accounts.

## Operation

The adapter reports a `401` or `403` to the broker as an authentication failure
and permits one broker-refreshed retry for that account. It reports `429` as a
quota failure and traverses distinct eligible accounts. Failover happens only
before response headers reach the app; a partially streamed response is never
replayed. Both normal Responses requests and Codex's remote-compaction requests
use the selected broker account.

Check the service and log with:

```bash
launchctl print gui/$(id -u)/dev.codex-broker.adapter
tail -f "$HOME/Library/Application Support/Codex Broker/logs/adapter.log"
```

Each typed Codex request should produce secret-free lines similar to:

```text
request received path=/v1/responses
request routed path=/v1/responses upstream_status=200 attempt=1
```

No request line means the app is not using the loopback base URL. These logs
never include authorization, account identifiers, prompts, or responses.

Rerun the installer to update the binary, broker URL, CA, or Keychain entry.
Revoke the old broker client key when replacing it.

## Remove

```bash
launchctl bootout gui/$(id -u)/dev.codex-broker.adapter
rm "$HOME/Library/LaunchAgents/dev.codex-broker.adapter.plist"
security delete-generic-password -a "$USER" -s dev.codex-broker.adapter
rm -rf "$HOME/Library/Application Support/Codex Broker"
```

Remove `openai_base_url`, `experimental_realtime_webrtc_call_base_url`, and
`experimental_realtime_ws_base_url` before restarting the ChatGPT app to
restore fully native routing.
