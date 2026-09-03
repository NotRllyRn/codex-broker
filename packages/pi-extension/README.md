# Codex Broker Pi extension

Routes `openai-codex` turns through Codex Broker and injects only the leased access token and ChatGPT account ID into provider headers. It waits when the pool is unavailable and performs at most one failover retry for an authentication or quota failure.

## Install

```sh
pi install ./packages/pi-extension
```

Set these variables in the environment that launches Pi:

```sh
export CODEX_BROKER_URL=https://192.168.1.20
export CODEX_BROKER_CLIENT_KEY=cbk_...
export CODEX_BROKER_CA_CERT=/path/to/ca.crt
export CODEX_BROKER_CLIENT_CERT=/path/to/pi-client.crt
export CODEX_BROKER_CLIENT_KEY_FILE=/path/to/pi-client.key
```

Create the API key with `codex-broker client-key create "Pi desktop"`. Copy it immediately; the broker stores only its hash. Install the generated CA certificate and keep the API key and client private key outside Pi settings and source control.

Use `/broker-status` to show the selected broker account. The extension never stores refresh tokens or full `auth.json` payloads.
