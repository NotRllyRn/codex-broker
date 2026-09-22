# macOS native-auth migration plan

Status: implemented; live Voice acceptance requires macOS

## Goal

Keep the ChatGPT desktop app on its built-in `openai` provider so ChatGPT
authentication, workspace entitlements, Voice, and other native service
connections remain owned by the app. Redirect only Codex model traffic with
the documented user-level `openai_base_url` setting:

```text
ChatGPT account services -----------------------------> OpenAI
ChatGPT Voice call creation ---------------------------> ChatGPT backend
ChatGPT Voice realtime sideband -----------------------> OpenAI
Codex Responses -> 127.0.0.1 adapter -> broker lease -> Codex Responses
```

The two realtime overrides are required because `openai_base_url` also becomes
the default Voice call and sideband base URL.

## Changes

1. Load the dedicated `cbk_` client key into the adapter process from macOS
   Keychain at startup. Retain `CODEX_BROKER_CLIENT_KEY` only for development.
2. Require a bearer-authenticated caller for Responses requests, but never use
   or forward the app's native ChatGPT bearer token.
3. Use only the adapter-owned client key for broker health and route calls.
4. Make loopback health independent of caller credentials so the installer can
   verify both Keychain loading and broker connectivity.
5. Replace the custom-provider configuration with:

   ```toml
   model_provider = "openai"
   openai_base_url = "http://127.0.0.1:8789/v1"
   experimental_realtime_webrtc_call_base_url = "https://chatgpt.com/backend-api/codex"
   experimental_realtime_ws_base_url = "https://api.openai.com/v1"
   ```

6. Keep the existing custom-provider configuration functional during migration:
   its `cbk_` bearer is accepted as a caller bearer but ignored in favor of the
   adapter-owned key.

## Security boundary

- The adapter remains IP-loopback-only and exposes only health, Responses, and
  compaction paths.
- Broker and leased credentials remain in memory and are never logged.
- Native ChatGPT authorization is removed before the upstream request.
- The built-in provider cannot attach a separate adapter secret. Requiring its
  bearer prevents accidental unauthenticated use but is not proof of origin;
  malicious same-user software remains outside the security boundary.
- The fixed HTTPS upstream, verified broker TLS, bounded bodies, redirect
  rejection, failover limits, and no-mid-stream-replay behavior remain intact.

## Verification

- Reject a Responses request without a bearer before broker routing.
- Verify a native-looking bearer is never sent to the broker or upstream.
- Verify broker health uses the adapter-owned client key without caller auth.
- Verify quota/auth failover, compaction, streaming, cancellation, and redirect
  rejection remain unchanged.
- Run race tests, vet, both macOS cross-builds, shell checks, and existing Pi
  checks.
- On macOS, verify typed Codex traffic reaches the adapter, Voice uses its
  direct realtime routes, and a voice-directed Codex task uses broker routing.

## Rollback

Restore the prior `model_provider = "codex_broker"` block in
`~/.codex/config.toml`. The upgraded adapter accepts that caller shape, so the
binary and LaunchAgent do not need to be downgraded.
