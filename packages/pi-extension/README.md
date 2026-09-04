# Codex Broker Pi extension

Routes `openai-codex` turns through Codex Broker. It overrides Pi's Codex stream with the built-in `openai-codex-responses` implementation using the in-memory leased access token, waits when the pool is unavailable, and retries at most once after a pre-output authentication or quota failure.

## Install

```sh
pi install ./packages/pi-extension
```

Set these variables in the environment that launches Pi:

```sh
export CODEX_BROKER_URL=https://192.168.1.20
export CODEX_BROKER_CLIENT_KEY=cbk_...
# Only needed when the broker CA is not in the OS trust store:
export CODEX_BROKER_CA_CERT=/path/to/ca.crt
```

Create the API key with `codex-broker client-key create "Pi desktop"`. Copy it immediately; the broker stores only its hash. Trust the broker's CA and keep the API key outside Pi settings and source control.

Use `/broker-status` to show the selected broker account. The extension never stores refresh tokens or full `auth.json` payloads.
