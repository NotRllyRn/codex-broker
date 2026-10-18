# Responses Payload Scrubber Implementation Plan

Status: implemented

Scope: Codex Broker `POST /v1/responses` proxy path

Implementation state: complete

## 1. Executive summary

Codex Broker currently reads at most 64 MiB and rejects a larger request before
forwarding it. Long-lived stateless Responses conversations can cross that byte
limit even when their model-token usage remains reasonable, especially when old
screenshots, inline files, or verbose tool results are replayed on every turn.

Add a deterministic, stateless payload scrubber at the existing proxy boundary.
Requests smaller than 56 MiB keep the current byte-for-byte fast path. At 56 MiB
or more, the broker removes replaceable historical bulk until the serialized
request is no larger than 50 MiB. The scrubber never changes the current turn,
never edits opaque reasoning or compaction values, and never separates a tool
call from its result. If it cannot reach the target safely, it rejects the
request once with a descriptive `413` and does not contact or retry upstream.

The feature stays local to `internal/responsesproxy` and the HTTP Responses
handler. It does not change account selection, pool spacing, credential
handling, streaming, or failover behavior.

## 2. Product requirements document

### 2.1 Problem

The proxy currently applies a single 64 MiB read limit. Once replayable history
crosses that limit, every later turn fails before upstream inference begins.
Token-oriented compaction is not a sufficient byte limit because encoded images
and files can occupy many megabytes without a proportional token cost.

The broker is the last component that sees the fully serialized request before
the hard boundary, so it can enforce a byte budget without requiring immediate
changes in every client.

### 2.2 Goal

Keep otherwise valid Responses conversations usable by removing old,
replaceable bulk before the request reaches the 64 MiB boundary, while
preserving the protocol and semantic state required for the current turn.

### 2.3 Success criteria

- A request smaller than 56 MiB follows the existing path without scrubber JSON
  parsing, reserialization, or byte changes.
- A request at or above 56 MiB is reduced to at most 50 MiB before routing.
- The newest user message and every item after it are left untouched.
- System and developer messages are retained.
- Matching tool calls and results are retained or removed together.
- `reasoning`, `compaction`, and their encrypted content are never edited.
- All replacements conform to the schema of their enclosing Responses item.
- A request that cannot be reduced safely is rejected before account routing or
  upstream I/O.
- The same scrubbed byte slice is reused for every authorized failover attempt.
- No prompt, attachment, filename, path, MIME value, hash, item identifier, or
  tool content is logged or persisted.
- Metrics contain only aggregate counts and byte totals.
- At most one large request is scrubbed concurrently.

### 2.4 Non-goals

- Initiating, emulating, or coordinating conversation compaction.
- Persisting scrubbed history on behalf of a client.
- Uploading inline data to an attachment or Files API.
- Summarizing content with a model.
- Reconstructing information removed in an earlier request.
- Translating Chat Completions payloads.
- Making speculative transformations for unknown future item types.
- Guaranteeing recovery when the protected current turn alone exceeds the
  target.
- Changing the 64 MiB bounds used for upstream response frames.

### 2.5 User-visible behavior

The successful response remains an ordinary Responses response. The model may
see short markers where historical media or output was removed, for example:

```text
[Historical image omitted by Codex Broker: 4,391,252 encoded bytes]
```

If safe reduction is impossible, return the existing OpenAI-shaped error form:

```json
{
  "error": {
    "message": "Request remains too large after historical content cleanup.",
    "type": "invalid_request_error",
    "code": "request_too_large_after_scrub"
  }
}
```

Do not include body excerpts, item identifiers, filenames, or content types in
the error.

### 2.6 Constraints and assumptions

- Input order is chronological, as expected by the Responses API.
- The newest `role: "user"` message begins the current turn.
- All items from that message to the end must remain intact. Official OpenAI
  guidance recommends carrying forward all reasoning, function calls, and
  function-call outputs associated with the active turn.
- Inline images and files are legal Responses content and may appear in message
  or function-output content arrays. Computer output uses a distinct
  `computer_screenshot` object and therefore needs a distinct replacement.
- Unknown structures are opaque. They may be retained or removed as part of a
  verified complete historical turn, but must not be edited internally.

Relevant official references:

- [Create a model response](https://developers.openai.com/api/reference/resources/responses/methods/create)
- [Reasoning models](https://developers.openai.com/api/docs/guides/reasoning)
- [File inputs](https://developers.openai.com/api/docs/guides/file-inputs)

## 3. Software functional design specification

### 3.1 Request budgets

Use binary MiB and exact serialized UTF-8 byte counts.

| Budget | Value | Purpose |
| --- | ---: | --- |
| Normal fast-path ceiling | `< 56 MiB` | Forward without scrubber work |
| Scrub trigger | `56 MiB` | Begin historical cleanup |
| Scrub target | `50 MiB` | Leave 14 MiB below the hard boundary |
| Recoverable inbound ceiling | `80 MiB` | Permit bounded rescue of an already oversized thread |
| Existing outbound hard ceiling | `64 MiB` | Never forward at or above this boundary |
| Concurrent large scrubs | `1` | Bound transient JSON memory |

Keep these as package constants for the first implementation. A configuration
surface is unnecessary until operational evidence shows a real need to tune
them.

### 3.2 Component boundaries

Add one pure module and keep orchestration in the existing handler:

```text
internal/responsesproxy/scrub.go
internal/responsesproxy/scrub_test.go
internal/httpserver/proxy.go
internal/httpserver/proxy_stats.go
internal/httpserver/proxy_test.go
```

The pure module owns JSON classification and rewriting. It must not import the
HTTP server, router, store, logger, or credential packages.

Suggested minimal interface:

```go
type ScrubPolicy struct {
	TriggerBytes int
	TargetBytes  int
}

type ScrubResult struct {
	Body          []byte
	BeforeBytes   int
	AfterBytes    int
	RemovedBytes  int
	Changed       bool
}

func Scrub(body []byte, policy ScrubPolicy) (ScrubResult, error)
```

Tests use small policies rather than allocating tens of megabytes for every
unit case. Boundary integration tests cover the real constants.

### 3.3 Handler flow

For exactly `POST /v1/responses`:

1. Authenticate the broker client as today.
2. Use `Content-Length` for admission:
   - known length below 56 MiB: retain the normal concurrent path;
   - known length at or above 56 MiB: acquire the single large-request gate;
   - unknown length: acquire the gate before reading.
3. Read through `http.MaxBytesReader` with the 80 MiB inbound ceiling.
4. Run existing broker-notice removal.
5. Call `Scrub` once.
6. If cleanup fails or remains above the target, return `413` before calling
   the router.
7. Route normally and pass the resulting byte slice to `Forward`.
8. Reuse that exact slice for all auth retries and account failovers.

Other proxy endpoints keep the existing 64 MiB behavior. In particular, this
proposal does not expand multipart, image-generation, transcription, thread
goal, or unsupported standalone-compaction requests.

### 3.4 Parsing strategy

Use the standard library and avoid a new JSON dependency.

1. Decode the request envelope as `map[string]json.RawMessage`.
2. Decode only `input` as `[]json.RawMessage`.
3. Classify each item with a small header containing only `type`, `role`, and
   `call_id`.
4. Fully decode an item only when a known rewrite handler selects it.
5. Preserve untouched items as `json.RawMessage`.
6. Marshal only after a cleanup phase changed something.

This intentionally favors a small, auditable implementation over a streaming
JSON editor. The 80 MiB input cap and one-scrub gate provide the memory bound.

### 3.5 Protected region

Find the last top-level message whose role is `user`. Its index through the end
of `input` is the protected current turn.

Within the earlier historical prefix, also protect:

- all system and developer messages;
- the latest compaction item and every item after it;
- any item participating in a `call_id` relationship that crosses into the
  protected region;
- malformed or unclassifiable items unless their entire containing turn is
  safely evicted.

If no user boundary can be established, do not perform turn eviction. Known
historical media handlers may operate only when they can prove the item is
outside an active tool continuation; otherwise fail closed.

### 3.6 Call-pair index

Build a map from each non-empty `call_id` to every input index containing it.
Before deleting any candidate set:

- every indexed occurrence of each affected `call_id` must be inside the set;
- a call known to require a result must not be left without one;
- a result must not be left without its call;
- a relationship crossing the protected boundary disqualifies the candidate.

Rewriting only a result's content does not change `call_id` and therefore does
not disturb pairing.

### 3.7 Cleanup phases

Run phases in the following order. After each phase, serialize and measure. Stop
as soon as the body is at most 50 MiB.

#### Phase A: discard a superseded prefix

If a latest historical `compaction` item exists, remove only items preceding
it. Preserve the compaction item byte content and everything after it. This uses
an already-existing canonical context boundary; it does not initiate
compaction.

#### Phase B: replace historical inline media

Walk eligible items from oldest to newest.

| Enclosing structure | Original content | Replacement |
| --- | --- | --- |
| user/system/developer message content | `input_image` data URL | `input_text` omission marker |
| user/system/developer message content | `input_file.file_data` | `input_text` omission marker |
| function-call output array | `input_image` | `input_text` omission marker |
| function-call output array | `input_file` | `input_text` omission marker |
| `computer_call_output.output` | inline `computer_screenshot` | fixed valid one-pixel PNG data URL |

Only inline `data:` or Base64 bodies are candidates. Preserve `file_id` and
ordinary `https:` references because they do not materially inflate the
request. Do not Base64-decode payloads; count their encoded bytes directly.

Do not add non-schema metadata fields. Do not rewrite assistant output content,
image-generation results, or unknown media-like fields in the first version.

#### Phase C: truncate historical tool output text

Eligible types are explicit, known result items such as
`function_call_output` and `custom_tool_call_output`. Do not truncate tool-call
arguments.

For a string output larger than 256 KiB:

- keep up to 8 KiB from the beginning;
- keep up to 8 KiB from the end;
- cut only on valid UTF-8 boundaries;
- insert a fixed marker containing only the omitted byte count.

Apply the same rule to known text parts inside a result content array. Preserve
the containing item, status, ID, type, and `call_id`.

#### Phase D: evict complete historical turns

Partition the unprotected prefix into turns beginning with a user message and
ending immediately before the next user message. Walk oldest to newest.

A turn is evictable only when:

- it contains no system or developer message;
- it does not contain the latest compaction item;
- every `call_id` relationship it touches is completely inside the turn;
- it does not overlap the protected current turn;
- deleting it leaves structurally valid JSON input.

Delete the whole turn as raw items, including its reasoning records. This does
not inspect, decrypt, truncate, or reserialize reasoning content.

### 3.8 Failure behavior

Return an internal typed error for:

- invalid large-request JSON;
- missing or non-array `input` in a request requiring cleanup;
- no safe current-turn boundary when additional eviction is required;
- inability to reach the target without protected mutations;
- a scrubbed body unexpectedly reaching the outbound hard ceiling.

Map malformed JSON to `400 invalid_request`. Map safe-reduction failure to
`413 request_too_large_after_scrub`. Neither case reaches account routing.

Do not attempt cleanup in response to an upstream `413`; pass that upstream
response through once. This avoids a second inference attempt with changed
context.

### 3.9 Determinism and retry safety

- The same input and policy must produce the same output bytes.
- Cleanup order is input order, never map iteration order.
- The scrubber runs at most once per incoming request.
- Account retry/failover uses the already-scrubbed body.
- A result below 56 MiB takes the fast path if presented again, making cleanup
  naturally idempotent.
- No upstream call begins until the final byte count is accepted.

### 3.10 Memory and concurrency

Add a one-slot large-request gate to `Server`. Initialize it in `New` and lazily
initialize it in test fixtures that construct `Server` directly.

The gate is held from before the large/unknown body is read until JSON cleanup
finishes. Waiting observes `r.Context().Done()`.

Expected worst-case transient memory consists of the bounded input byte slice,
raw-message copies, the changed item, and the final body. Only one request may
incur that cost at a time. Normal requests remain concurrent and skip the JSON
scrubber.

### 3.11 Observability and privacy

Extend in-memory proxy statistics with numeric aggregates only:

```text
scrubbed_requests
scrubbed_input_bytes
scrubbed_output_bytes
scrubbed_removed_bytes
scrub_rejected_requests
```

Do not add per-item counters, labels, filenames, hashes, MIME values, IDs, or
content excerpts. Do not persist these counters. Do not add a dashboard change
in the first implementation; the existing authenticated proxy-status response
is sufficient for verification.

Any structured operational log, if needed, is limited to the result and the
three byte totals. Logging is not required for correctness and should be
omitted unless existing operations conventions require it.

### 3.12 Security properties

- Authentication occurs before allocating the expanded 80 MiB allowance.
- A single authenticated client cannot start concurrent scrub parses.
- Request contents remain memory-only.
- Placeholder construction never includes attacker-controlled filenames,
  paths, MIME values, URLs, or content excerpts.
- Unknown JSON is retained or atomically removed, not interpreted loosely.
- Size checks use `len([]byte)`, not rune counts or token estimates.
- The outbound body is measured after all existing and new transformations.

## 4. Implementation plan

### Phase 1: pure scrubber

1. Add budget constants, `ScrubPolicy`, `ScrubResult`, and typed errors.
2. Implement raw envelope parsing and item classification.
3. Implement current-turn protection and the `call_id` index.
4. Implement the four cleanup phases as small private functions.
5. Add exact serialization and target enforcement.
6. Unit-test each phase without involving HTTP or routing.

Keep each handler narrow: one item type, one transformation, one reason to
change. Prefer table-driven dispatch over a large recursive generic walker.

### Phase 2: proxy integration

1. Add the large-request gate to `Server`.
2. Give `/v1/responses` the 80 MiB authenticated inbound allowance.
3. Run notice cleanup followed by `Scrub` before routing.
4. Map typed errors to `400` or `413`.
5. Assert the accepted body is below the outbound boundary.
6. Confirm retries receive the identical scrubbed byte slice.

### Phase 3: counters and documentation

1. Add aggregate in-memory scrub counters.
2. Expose them through the existing authenticated proxy status snapshot.
3. Correct the stale request-limit statement in `SECURITY.md` while documenting
   inbound rescue, scrub trigger, target, and hard outbound boundary.
4. Document the possibility of historical omission markers in the proxy
   integration section.

### Phase 4: verification

Run:

```text
go test ./internal/responsesproxy ./internal/httpserver
go test ./...
go test -race ./internal/responsesproxy ./internal/httpserver
```

Add a focused benchmark for a synthetic 56–80 MiB request. The benchmark is an
engineering guardrail, not a release-blocking absolute performance target.

## 5. Test specification

### 5.1 Fast path

- A body one byte below the trigger is returned as the original slice.
- Whitespace and unknown fields remain byte-for-byte unchanged.
- Existing notice cleanup behavior remains unchanged.

### 5.2 Boundary behavior

- Exactly 56 MiB enters cleanup.
- Successful cleanup reaches 50 MiB or less.
- Exactly 80 MiB is accepted for attempted rescue.
- More than 80 MiB returns `413` without parsing or routing.
- No accepted body reaches upstream at or above 64 MiB.

### 5.3 Media replacements

- Old user image data is replaced with valid `input_text`.
- Old inline file data is replaced with valid `input_text`.
- Function-output array media is replaced without changing `call_id`.
- Computer output retains `computer_screenshot` with the fixed valid PNG.
- Remote URLs and file IDs remain unchanged.
- Media in the newest user turn remains byte-for-byte unchanged.
- Assistant and unknown content variants remain unchanged.

### 5.4 Tool-output truncation

- Head and tail are retained at UTF-8 boundaries.
- The omission count is correct in bytes.
- Tool-call arguments are unchanged.
- Tool and result IDs, types, statuses, and `call_id` values are unchanged.
- Short outputs remain unchanged.

### 5.5 Pair and turn safety

- A complete call/result pair may be evicted with its old turn.
- A pair crossing a candidate boundary prevents that turn's eviction.
- Parallel calls and results remain paired.
- An orphan-shaped malformed history fails closed rather than creating another
  orphan.
- The newest user item and all following reasoning, calls, and results remain
  unchanged.

### 5.6 Opaque state

- `reasoning.encrypted_content` values are identical after media-only cleanup.
- Compaction encrypted content is identical.
- The latest compaction item is retained.
- Prefix removal before an existing compaction item does not decode or modify
  the compaction item.

### 5.7 Errors and retries

- Large malformed JSON returns `400` without upstream I/O.
- An oversized protected current turn returns the scrub-specific `413`.
- No account is selected for a scrub rejection.
- Auth and quota failover receive exactly the same scrubbed bytes.
- An upstream `413` is not retried or transformed reactively.

### 5.8 Concurrency and properties

- Only one large scrub runs concurrently.
- Cancellation while waiting for the gate releases promptly.
- Race tests pass.
- Fuzzing asserts: no panic; valid successful output JSON; output is at most the
  target; protected raw values remain equal; every retained known result has a
  retained matching call; and a second scrub makes no further change.

## 6. Acceptance checklist

- [ ] Fast-path requests are byte-for-byte compatible.
- [ ] The scrub target is enforced using serialized bytes.
- [ ] Current-turn protection matches the latest-user boundary.
- [ ] Tool pairing is proven before item deletion.
- [ ] Encrypted reasoning and compaction values are opaque.
- [ ] Replacements pass documented Responses schemas.
- [ ] Unknown item types fail closed.
- [ ] Large-request memory concurrency is bounded.
- [ ] Retry/failover reuses one scrubbed body.
- [ ] Metrics contain only counts and byte totals.
- [ ] No inference content is logged or persisted.
- [ ] Targeted, full, race, fuzz, and boundary tests pass.
- [ ] Security and integration documentation reflects actual limits.

## 7. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| A future Responses variant is misclassified | Rewrite only explicit known shapes; treat everything else as opaque |
| Tool history becomes invalid | Index `call_id`; delete only closed pairs inside complete turns |
| Current reasoning continuity is damaged | Protect the latest user message and every following item |
| JSON parsing causes excessive memory | 80 MiB cap, raw messages, and one concurrent scrub |
| Cleanup changes every request unnecessarily | Zero-parse fast path below 56 MiB |
| A scrubbed request is replayed differently during failover | Scrub once before routing and reuse the same byte slice |
| Placeholders leak sensitive metadata | Include only fixed text and numeric byte counts |
| Cleanup is insufficient | Evict complete old turns, then fail once with a deterministic `413` |

## 8. YAGNI and modularity decisions

- No database schema, persisted repair state, attachment store, upload API,
  model summarizer, configurable policy language, or client handshake.
- No generic recursive mutation engine. Known item handlers are easier to audit
  and safer when the upstream schema evolves.
- One pure scrubber entry point, one HTTP integration point, and one small set
  of numeric counters.
- Standard-library JSON and synchronization only.
- Configuration is deferred until measured operational evidence requires it.

## 9. Estimated effort

- Pure scrubber and unit tests: 2–3 engineering days.
- HTTP integration, limits, gate, and counters: 1–2 engineering days.
- Boundary, race, fuzz, documentation, and production validation: 1–2
  engineering days.

Expected total: 4–7 engineering days.
