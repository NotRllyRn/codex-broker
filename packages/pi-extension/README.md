# Codex Broker Pi extension

Routes `openai-codex` turns through Codex Broker. It overrides Pi's Codex stream with the built-in `openai-codex-responses` implementation using the in-memory leased access token, waits when the pool is unavailable, and keeps replacing exhausted accounts until the request succeeds.

## Install

```sh
pi install ./packages/pi-extension
```

Set these variables in the environment that launches Pi:

```sh
export CODEX_BROKER_URL=https://192.168.1.20:8787
export CODEX_BROKER_CLIENT_KEY=cbk_...
# Only needed when the broker CA is not in the OS trust store:
export CODEX_BROKER_CA_CERT=/path/to/ca.crt
```

Create the API key with `codex-broker client-key create "Pi desktop"`. Copy it immediately; the broker stores only its hash. Trust the broker's CA and keep the API key outside Pi settings and source control.

The status bar and `/broker-status` show the selected account label, remaining 5-hour and weekly usage, and each known reset countdown. Pre-output authentication/quota failures reroute across the available pool and wait for the exact reset when all accounts are exhausted; failures after streamed output are surfaced without automatic replay. The extension never stores refresh tokens or full `auth.json` payloads.
