# macOS loopback adapter plan

Status: implemented; authentication was superseded by
[`macos-native-auth.md`](macos-native-auth.md); macOS live acceptance remains a
release gate

## Outcome

Add a supported `codex-broker-adapter` Go program for local Codex chats in the
ChatGPT macOS app. Codex sends Responses API requests to a loopback endpoint;
the adapter obtains an access-only lease from Codex Broker and forwards the
request to the ChatGPT Codex backend.

```text
ChatGPT app (Codex) -> 127.0.0.1 adapter -> Codex Broker /route
                              |
                              +------------> ChatGPT Codex /responses
```

The adapter is a built-in provider base-URL override, not a traffic interceptor.
It does not modify the signed ChatGPT app, export `auth.json`, or receive a
refresh token.

## Boundaries

- Listen only on an IP loopback address.
- Accept only `POST /v1/responses`, `POST /v1/responses/compact`, and a small
  health endpoint that verifies broker connectivity.
- Load the `cbk_` broker client key from macOS Keychain at startup. Require but
  discard the built-in provider's native bearer on Responses requests.
- Keep the broker client key, leased access token, request body, and response
  body out of logs and durable storage.
- Require HTTPS for Codex Broker, allow an explicit broker CA certificate, and
  never disable certificate verification.
- Use the fixed ChatGPT Codex Responses upstream. Do not add a general-purpose
  forward proxy or configurable upstream in the user-facing CLI.
- Replace `Authorization` and `ChatGPT-Account-ID`; never forward caller-owned
  identity headers upstream.
- Retry account authentication and quota failures only before response headers
  reach the app. Never replay a partially streamed response.
- Preserve native behavior when the adapter provider is not selected.

## Components

### `internal/loopback`

One handler owns the complete request lifecycle:

1. Validate loopback method, path, bearer token, and bounded request body.
2. Request a lease with a process session ID, request turn ID, and the last
   successful public account ID as a non-secret preference.
3. Clone the request to `https://chatgpt.com/backend-api/codex/responses`.
4. Replace upstream authorization and account identity with the lease.
5. Stream a successful response without buffering.
6. On a pre-output `401`/`403`, ask the broker to refresh or replace the failed
   account and retry once for the same account.
7. On a pre-output `429`, report quota failure and traverse distinct accounts.
8. Honor broker pool waits until reset or request cancellation.
9. Fail closed on broker, TLS, validation, or routing uncertainty.

The broker response is bounded to 64 KiB. The incoming Responses request is
bounded to 32 MiB. Upstream error bodies are bounded before classification.

### `cmd/codex-broker-adapter`

Expose only the settings needed by a local service:

- `--listen` (default `127.0.0.1:8789`, loopback addresses only)
- `--broker-url` (required HTTPS origin)
- `--broker-ca` (optional PEM file)
- `--keychain-account` (macOS account owning the Keychain item)

The command handles `SIGINT`/`SIGTERM` and shuts down cleanly.

### macOS installer

`scripts/install-macos-adapter.sh` builds the binary, stores a supplied broker
client key in macOS Keychain, writes a user LaunchAgent, and starts it. It does
not edit `~/.codex/config.toml`; the integration guide provides the exact
provider block so existing Codex configuration is not overwritten.

## Verification

- Unit-test configuration validation and loopback-only binding.
- Use TLS test servers for the broker and upstream.
- Verify request bytes and streaming response bytes are preserved.
- Verify leased authorization and account headers replace local credentials.
- Verify quota and authentication failover payloads and cycle bounds.
- Verify pool waits are interruptible.
- Verify invalid/revoked client keys fail before any upstream request.
- Run `gofmt`, `go vet`, `go test -race ./...`, both Go builds, and the existing
  Pi TypeScript checks.

## Deliberate omissions

- No GUI, menu-bar item, auto-updater, or privileged daemon.
- No local credential database or token cache.
- No generic OpenAI API surface, model catalog proxy, WebSocket proxy, or cloud
  ChatGPT integration.
- No mid-stream replay or generated continuation prompt.
- No automatic edits to user Codex configuration.
