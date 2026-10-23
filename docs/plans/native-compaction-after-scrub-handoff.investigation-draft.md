# Native Compaction After Scrub Handoff

## Goal of next session

Implement the smallest broker-only change that lets a single Codex thread run
for an effectively unlimited sequence of bounded model steps, including a long
goal task that remains inside one logical turn.

When a normal Responses request becomes large, the broker must scrub it,
successfully proxy the model response, and then use that response's real
terminal `response.completed` event to tell Codex that its context is full.
Codex can then perform and persist its own native compaction before the next
model request, without returning control to the user and without changes to T3
Code or Codex CLI.

Do not emit an early or additional `response.completed` event. Rewrite only the
usage object in the terminal event already returned by upstream.

## State of play

- The existing stateless request scrubber is implemented in
  `internal/responsesproxy/scrub.go` and orchestrated by
  `internal/gateway/engine.go`. Its current constants are an 80 MiB inbound
  ceiling, 56 MiB trigger, 50 MiB target, and 64 MiB forwarded-request ceiling.
  The original design is documented in
  `docs/plans/responses-payload-scrubber-implementation-plan.md`.
- The scrubber currently changes only the forwarded copy. Codex retains its
  original local history, so later model requests grow again and can hit the
  broker ceiling.
- The deployed failure involved an 86,736,454-byte request, which exceeds the
  current 80 MiB inbound ceiling before scrubbing can run.
- `internal/responsesproxy/notice.go` already parses and re-serializes every SSE
  event in successful `/v1/responses` streams. This is the narrowest place to
  rewrite the terminal usage sent to the client.
- `internal/gateway/engine.go` already retains the `ScrubResult` long enough to
  know whether a request changed, but it currently discards that fact before
  copying the upstream response.
- Codex 0.162.1 was source-verified to check token status after every completed
  model request. If more model work remains and the reported context is full,
  it calls native compaction with `CompactionPhase::MidTurn`, installs the
  replacement history, and continues the same logical turn. If the logical
  turn is complete, it compacts before the next user message. This behavior is
  in Codex's `codex-rs/core/src/session/turn.rs` around `should_roll_over` and
  `run_auto_compact`.
- Remote compaction is not identified only by the URL. Current Codex can send a
  remote-v2 compaction request to `/v1/responses` with an input item whose type
  is `compaction_trigger`. Legacy clients may use `/v1/responses/compact`.
- No implementation or production change for native compaction arming has been
  made yet. This file is the implementation brief.

### Required behavior

Use these first-version byte budgets:

| Budget | Value |
| --- | ---: |
| Scrub trigger | 50 MiB |
| Scrub target | 40 MiB |
| Recoverable inbound ceiling | 128 MiB |
| Maximum forwarded request | 64 MiB, unchanged |
| Concurrent large scrubs | 1, unchanged |

In `internal/responsesproxy/scrub.go`:

1. Change `MaxInboundRequest` to `128 << 20`.
2. Change `ScrubThreshold` to `50 << 20`.
3. Change `ScrubTarget` to `40 << 20`.
4. Keep the single-large-request gate. The raised ceiling is a rescue allowance,
   not permission for concurrent 128 MiB JSON transformations.

In the request path:

1. Preserve a request-scoped Boolean such as `armNativeCompaction`.
2. Set it only when all of the following are true:
   - the endpoint is a normal `/v1/responses` request;
   - `Scrub` actually changed the request;
   - the request is not itself a compaction request;
   - upstream ultimately returns a successful SSE response.
3. Detect compaction semantically. Treat an input item with
   `"type":"compaction_trigger"` as compaction even on `/v1/responses`, and
   also treat `/v1/responses/compact` as compaction.
4. Reuse the same Boolean across account retries/failovers for that logical
   proxy request.

In the response path:

1. Extend the existing `CopyNoticeResponse` / `streamNotice` path with a small
   options value or Boolean. Do not create a second SSE parser.
2. Wait for upstream's genuine `response.completed` event.
3. Capture broker telemetry from the original upstream event before applying
   the client-facing rewrite. Stored usage and account statistics must remain
   truthful.
4. When `armNativeCompaction` is true, rewrite the client-facing terminal
   event's `response.usage.input_tokens` to a context-full sentinel and set
   `response.usage.total_tokens` consistently. Preserve the real output token
   count and all unrelated fields.
5. Use `1_000_000` input tokens as the first-version sentinel unless a focused
   compatibility test shows it is insufficient. This is intentionally above
   the effective context window of the currently supported Codex models and
   avoids adding a model-to-window registry to the broker.
6. Never rewrite `response.failed` or `response.incomplete`.
7. Never arm or rewrite the response to a compaction request. Otherwise the
   client can enter a compaction loop.
8. Do not delay, truncate, or replace reasoning/tool-call events. Only the
   usage object in the already-terminal event changes.

The expected long-turn sequence is:

```text
large normal model request
  -> broker scrubs to <= 40 MiB
  -> upstream streams reasoning/tool call
  -> upstream sends its real response.completed
  -> broker rewrites only client-facing usage to context-full
  -> Codex sees follow-up work and runs native mid-turn compaction
  -> broker accepts/scrubs the compaction request but does not arm again
  -> Codex persists replacement history
  -> Codex continues the same logical turn with a small next request
```

This cannot make one individually protected current step arbitrarily large. A
single newest turn must still be reducible below 64 MiB. The goal is unlimited
repetition of bounded steps, not unlimited size for one atomic step.

### Required tests

Add focused tests alongside the existing scrub and proxy-stream tests:

1. A request above 80 MiB but below 128 MiB is admitted and scrubbed.
2. A request above 128 MiB receives the existing broker `413` without routing.
3. A scrubbed normal request rewrites only the terminal client-facing
   `response.completed` usage.
4. A request below 50 MiB preserves the upstream terminal event byte semantics
   apart from the existing notice normalization.
5. A scrubbed `/v1/responses` request containing `compaction_trigger` does not
   rewrite usage.
6. `/v1/responses/compact` does not rewrite usage.
7. Failed and incomplete terminal events are not rewritten.
8. Broker telemetry persists the real upstream usage, not the sentinel.
9. Auth retry and account failover still emit at most one terminal event and
   retain the correct arming decision.
10. A regression test models multiple model steps in one logical turn: after a
    scrubbed tool-call response completes, the next client action is
    compaction rather than another growing normal request. If this requires a
    pinned Codex integration fixture, keep it separate from fast Go unit tests.

Run at minimum:

```bash
gofmt -w <changed-go-files>
go test ./internal/responsesproxy ./internal/gateway ./internal/httpserver
go test ./...
go vet ./...
```

Update the old scrubber plan's non-goal stating that the broker does not
coordinate compaction; after this change, that statement is no longer true for
scrubbed normal requests.

## Open decisions

- **Should the context-full sentinel remain a constant or become model-aware?**
  Options considered are a fixed `1_000_000` value or maintaining model context
  windows in the broker. Lean: fixed sentinel for YAGNI. The constraint is that
  it must exceed Codex's effective context limit without adding another model
  catalog.
- **Should missing upstream usage be synthesized?** Options are to create a
  schema-valid usage object or refuse to arm and record a metric. Lean: create
  it for a successful `response.completed`, with zero output tokens only when
  upstream omitted them, because the recovery guarantee matters more than an
  uncommon malformed-usage case.
- **Should arming have an aggregate metric?** Options are no new metric or a
  single `native_compaction_armed` counter. Lean: add the counter because it is
  cheap and is the only production evidence that the recovery path activated;
  never log request content.
- **Does the sentinel affect any configured local rollout budget?** Verify with
  the installed Codex version. Token usage and `codex_rollout_budget_units` are
  separate today, but the implementation should test this rather than assume
  it. If it does, prefer the smallest model-aware threshold rather than
  disabling the recovery behavior.

## Skills to use

- `focused-fix` — keep the patch limited to admission constants, compaction
  detection, and the existing SSE transformation path.
- `tdd-guide` — write the terminal-event and compaction-loop regression tests
  before changing stream behavior.
- `code-reviewer` — review SSE correctness, truthful telemetry, failover, and
  accidental repeated compaction.
- `performance-profiler` — confirm the 128 MiB rescue path stays bounded by the
  existing single-large-request gate.

## Artifacts

- `docs/plans/native-compaction-after-scrub-handoff.md`
- `docs/plans/responses-payload-scrubber-implementation-plan.md`
- `internal/responsesproxy/scrub.go`
- `internal/responsesproxy/notice.go`
- `internal/gateway/engine.go`
- `internal/httpserver/proxy_scrub_test.go`
- `internal/httpserver/proxy_test.go`
