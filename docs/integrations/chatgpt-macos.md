# ChatGPT macOS adapter

The `codex-broker-adapter` lets local Codex chats in the ChatGPT macOS app use
the accounts managed by Codex Broker. It is a loopback Responses API provider,
not a network interceptor:

```text
ChatGPT app -> http://127.0.0.1:8789/v1/responses
            -> Codex Broker /api/v1/route
            -> ChatGPT Codex backend
```

Ordinary ChatGPT chats, cloud tasks, Voice, connectors, and profile features
continue to use the account signed into the ChatGPT app.

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

The adapter listens only on `127.0.0.1:8789`. It receives the client key from
the ChatGPT app, exchanges it for an in-memory access-only lease, and never
stores an access token, refresh token, request, or response.

## Connect the ChatGPT app

Open **ChatGPT → Settings → Configuration → Open config.toml** and add this to
the user-level `~/.codex/config.toml`:

```toml
model_provider = "codex_broker"

[model_providers.codex_broker]
name = "Codex Broker"
base_url = "http://127.0.0.1:8789/v1"
wire_api = "responses"

[model_providers.codex_broker.auth]
command = "/usr/bin/security"
args = ["find-generic-password", "-s", "dev.codex-broker.adapter", "-w"]
refresh_interval_ms = 0
```

Keep your existing `model` setting, if any. Do not set
`requires_openai_auth = true`; the custom provider authenticates with the
Keychain-backed broker client key. Restart the ChatGPT app, select **Codex**,
and start a local project chat.

The app may still require a nominal ChatGPT or API-key sign-in to unlock its
desktop UI. That signed-in identity is not used for local model requests while
`model_provider = "codex_broker"` is active.

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

Rerun the installer to update the binary, broker URL, CA, or Keychain entry.
Revoke the old broker client key when replacing it.

## Remove

```bash
launchctl bootout gui/$(id -u)/dev.codex-broker.adapter
rm "$HOME/Library/LaunchAgents/dev.codex-broker.adapter.plist"
security delete-generic-password -a "$USER" -s dev.codex-broker.adapter
rm -rf "$HOME/Library/Application Support/Codex Broker"
```

Remove the `codex_broker` provider block or select another `model_provider`
before restarting the ChatGPT app.
