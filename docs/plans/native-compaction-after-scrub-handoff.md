# Native Compaction After Scrub Handoff

## Goal

Let a Codex thread continue through repeated bounded model steps after the
broker has scrubbed an oversized normal Responses request. The broker does not
compact or persist client history; it signals Codex to run its native
compaction by changing usage only in upstream's genuine successful terminal
event.

## Design

The authenticated request body is admitted up to 128 MiB. Requests at or above
50 MiB continue through the existing single-large-request gate and must scrub
to at most 40 MiB before forwarding; the upstream limit remains 64 MiB.

When scrubbing changes a request, the gateway classifies the pre-scrub body:
`/v1/responses/compact` and any `/v1/responses` input containing a
`compaction_trigger` item are compaction. A request-scoped arming decision is true only for a changed normal
`/v1/responses` request. That decision is calculated once and reused across
authentication retries and account failover.

The existing SSE notice transformer receives the arming option. It first
captures telemetry from the unmodified terminal event. When the event is
`response.completed`, it changes only client-facing usage:

- `input_tokens` becomes the fixed 1,000,000 context-full sentinel;
- `output_tokens` retains the valid upstream count, or becomes zero when usage
  was absent;
- `total_tokens` becomes their sum;
- all unrelated response and usage fields remain intact.

No event is added or emitted early. Failed and incomplete events are unchanged,
and compaction requests are never armed. A single aggregate counter records
when a completed event is actually rewritten.

## Verification

Fast tests cover terminal-event rewriting, synthesized usage, truthful
telemetry, semantic compaction detection, retry/failover behavior, and the
unchanged below-threshold path. An HTTP test allocates an 81 MiB request to
prove that the old 80 MiB boundary is recoverable while the existing
content-length test proves that requests above 128 MiB are rejected before
routing. The existing gate test continues to prove that only transformation,
not upstream inference, is serialized.

The fixed sentinel is intentionally model-agnostic for this first version. A
model window registry and broker-owned compaction remain out of scope.
