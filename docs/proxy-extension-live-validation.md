# Proxy extension live validation

Validated on 2026-11-07 using an isolated Broker instance, one freshly enrolled
ChatGPT account, managed Codex CLI 0.165.0, and `gpt-6.1-sol`. Production data and
credentials were not used. Prompts contained only synthetic test content.
Automatic window-pulse model requests were disabled.

## Results

Twenty completed model responses recorded **804 input tokens, 239 output tokens,
and 1,043 total tokens**. Database totals, dashboard API rolling/lifetime totals,
and the browser's formatted display agreed. Cached input was zero in these tests.
One deliberately canceled request had no terminal usage event and therefore no
persisted terminal usage row; its possible partial upstream consumption is not
included in those totals.

| Check | Observed result |
| --- | --- |
| Ordinary HTTP Responses SSE | Completed responses, expected answer and usage prefix. |
| Usage extension toggle | Turning it off removed the prefix; operational token accounting continued. |
| Usage settings | Applying the generic account-label setting changed the next real response. |
| Settings validation | Invalid threshold relationship returned 400; stale revision returned 409 without changing settings. |
| Browser settings | Apply affected proxy output; Cancel discarded the draft; Reset restored defaults; Disable/Enable took effect. |
| Request-size cleanup | With a 2 MiB trigger and 1 MiB target, a 3,146,064-byte fixture became 16,743 bytes with its latest message intact. Disabling cleanup retained the full body. These measurements used local interception, not model inference. |
| Real cleanup and native handoff | A separate oversized whitespace fixture was reduced before real inference. The client received the existing 1,000,000-input-token compaction signal; telemetry retained the actual 224 input + 8 output = 232 tokens. |
| Current Codex compaction | A `compaction_trigger` sent to `/v1/responses` returned a compaction item. Replaying its unchanged opaque content retained the example context. |
| Request transformation | An intentionally invalid incoming model was rewritten before dispatch; persisted request metadata contained the actual upstream model. |
| Response transformation | Client-visible total tokens were deliberately rewritten to 999999; persisted telemetry still matched the raw upstream total of 34. |
| Whole-response replacement | Real SSE was buffered and replaced with a 202 JSON response; actual upstream usage remained recorded. |
| Local responses | Synthetic ordinary and compact responses bypassed inference and created no fabricated upstream usage rows. |
| Extension exceptions/timeouts | Intentional exceptions rolled back the failing extension's candidate. A hung worker was killed; the original request still completed. A later request restarted the worker. |
| Extension storage/settings | Normal Node filesystem access worked. Settings and files survived worker/Broker restarts. |
| Concurrent requests | Two simultaneous real requests returned their distinct markers and separate telemetry records. |
| Cancellation | Closing the client after its first SSE event released the active request. |
| Authentication/limits | Missing authentication returned 401; oversized declared ingress returned 413; unsupported Responses GET/WebSocket transport returned 426. |
| Persistence | Enrolled credentials and telemetry survived restarts without another sign-in. |

## Defect found and fixed

The initial Node bridge charged body transfer/parsing against the 100 ms handler
budget. A harmless hook timed out while receiving a 3 MiB request, so the extension
was skipped before local interception. An invalid-model guard prevented that test
from accidentally running a large inference request.

The bridge now separates bounded phases: input/output transport and parsing get
5-second budgets, ordinary handler execution retains 100 ms, initialization gets
1 second, and output JSON serialization gets 1 second because getters can execute
extension code. The redundant clone of the newly decoded event was also removed;
each invocation already receives a separate JSON copy through IPC.

An 8 MiB request/response regression verifies that large transfers do not consume
the execution budget. Existing throw, loop, serialization, cancellation, rollback,
ordering, settings, thread-state, and crash-recovery tests still pass. The live
3 MiB cleanup test and transformation/error tests were repeated successfully on
the fixed binary.

## Limits of this validation

- The legacy `/v1/responses/compact` route returned an upstream 404. This is not a
  passing endpoint check. Codex 0.165.0's current remote V2 compaction uses a
  `compaction_trigger` on the normal Responses stream, which passed. The legacy
  route was not changed or silently remapped.
- One account cannot validate real multi-account failover or quota exhaustion.
  Those remain covered by deterministic fake-upstream tests.
- Nonzero cache hits, long stream-idle timeouts, sustained production load,
  realtime voice/WebSockets, and image/audio generation were not exercised live.
- Telemetry records observed terminal Responses usage; it does not estimate
  missing usage after cancellation or fabricate accounting for local replies.

The temporary custom extension was removed, both built-ins were re-enabled, and
the original defaults were restored (usage label/retry status on; cleanup trigger
50 MiB and target 40 MiB). Intentional failure diagnostics remain in the isolated
test logs. The test server and its encrypted account remain available for further
operator-authorized testing.

Regression validation after the fix: full Go suite, `go vet`, race checks for the
extension runtime/gateway and affected HTTP/relay extension tests, and live browser
checks of settings and telemetry. No production deployment or merge was performed.
