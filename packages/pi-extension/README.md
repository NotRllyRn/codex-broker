# Codex Broker Pi extension

Routes `openai-codex` turns through Codex Broker. It overrides Pi's Codex stream with the built-in `openai-codex-responses` implementation using the in-memory leased access token, waits when the pool is unavailable, and keeps replacing exhausted accounts until the request succeeds.

## Install

```sh
pi install ./packages/pi-extension
```

Run `/broker-status` and use its TUI menu to set the HTTPS server address, API token, and CA certificate path. The extension verifies each change and stores it with mode `0600` in `~/.pi/agent/codex-broker.json` (or `$PI_CODING_AGENT_DIR/codex-broker.json`). Existing `CODEX_BROKER_*` variables remain fallback defaults for compatibility, but saved settings take precedence.

Create the API key with `codex-broker client-key create "Pi desktop"`. Copy it immediately; the broker stores only its hash. Never commit the settings file.

On startup, the status bar shows `broker: connecting`, then `broker: ready` or `broker: unavailable` after an authenticated health check. After routing, it and `/broker-status` show the selected account label, remaining 5-hour and weekly usage, and each known reset countdown. Authentication/quota failures reroute across the available pool and wait for the exact reset when all accounts are exhausted. Transport failures revalidate the preferred account, replace it when no usage remains, and automatically retry or continue without replaying already-streamed text. Context-window errors remain Pi-managed compaction errors rather than broker failovers. The extension never stores refresh tokens or full `auth.json` payloads.
