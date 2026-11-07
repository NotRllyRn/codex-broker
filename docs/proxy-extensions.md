# Proxy extensions

## Implementation contract

The gateway remains responsible for authentication, account selection, credential
injection, failover, hard size limits, upstream validation and real usage accounting.
The built-in request-size-compaction extension owns history cleanup and the native
compaction handoff. Built-in source and its Settings catalog live in `extensions/`.
Extensions run only after authentication. Default behavior must match the existing
proxy, including replay cleanup, native compaction, non-SSE pass-through and 426
for WebSocket upgrades. Disabling account usage removes only its answer prefix.

Pi 0.84.4's `core/extensions/{types,loader,runner}.ts` supplies the design:
a default factory registers typed `on` handlers; transformations chain in
registration order; handled input short-circuits; context is separate from the
payload. Pi uses jiti and an async Node runtime. Broker instead embeds Goja and
esbuild's TypeScript transform: no Node daemon, IPC, CGO or extra deployment
executable. Only synchronous, self-contained modules are supported. This is a
proxy API, not compatibility with Pi's UI, tools or session-manager APIs.

Implementation sequence: embedded runtime and typed contract; gateway seams;
usage presentation and settings; deterministic protocol and concurrency tests;
full Go/JS checks and binary/Nix build checks. Existing tests are the behavior
baseline. Fake upstream tests must prove transformations and local handling,
composition, failure/cancellation, real telemetry and unchanged default output.

Billion-context (bebc239f2767e1a35c7664755f3ea8cb51faa641) motivates request
reconciliation and compact endpoint interception. No compression, private tool
loop, fabricated encrypted compaction payload or history dropping is included.

## Authoring and operation

The built-in `account-usage` extension is enabled by default. It is a native
adapter around the existing formatter and streaming reconciliation, avoiding a
VM on ordinary requests. Settings → Proxy extensions controls it and all loaded
TypeScript extensions. Disabling it removes the prefix only: existing notices
are still removed from replayed history, raw upstream usage is still recorded,
and the separate request-size-compaction extension keeps its current setting.
Streamer mode still masks the usage label.

The built-in `request-size-compaction` extension is also enabled by default. Its
source, cleanup algorithm and native-compaction handoff are in
`extensions/requestsize/`; the proxy invokes it through the native extension
preparation/presentation pipeline. Its global toggle disables both automatic
history trimming and the follow-up compaction signal. It retains the existing
50 MiB trigger, 40 MiB target, protected-content rules, errors and metrics when
enabled. The host always enforces its 128 MiB inbound Responses cap and 64 MiB
forwarding cap. Disabling cleanup may therefore cause large requests to be
rejected until a custom request extension reduces them. Active requests keep
their initial setting, and relays receive it on their normal snapshot sync.

To provision a trusted module, place a self-contained `.ts` file in an
operator-controlled directory and set `WINDOWKEEPER_PROXY_EXTENSIONS_DIR` to its
absolute path. Restart the broker. The filename without `.ts` is its ID (lowercase
letters, digits, `_` and `-`, up to 64 characters); built-in IDs are reserved.
`.d.ts` files are ignored. Files load once in filename order. There is no upload,
installer, discovery outside that directory, hot reload or marketplace. Mount
the directory read-only in Docker, including relay containers. For NixOS, set
`systemd.services.codex-broker.environment.WINDOWKEEPER_PROXY_EXTENSIONS_DIR`.
The binary embeds both the runtime and compiler; Docker and standalone Go/Nix
builds require no extra runtime dependency. Node is needed only for development
checks of the TypeScript declarations and examples.

The default export is a synchronous factory taking `ExtensionAPI`. Use the types
in `packages/proxy-extension/index.d.ts` via `import type`; see the examples next
to it. Type-only imports disappear during compilation. Runtime imports, npm
packages, async factories/handlers, timers, network, filesystem and process APIs
are unsupported. Prebundle dependencies into one file if necessary. esbuild
transpiles but does not type-check; run `npm run check:proxy-extensions` for the
shipped examples, and `tsc --noEmit` for your own module before provisioning it.

Return a partial patch to change an event. Returning nothing means unchanged;
mutating the event without returning a patch has no effect. Headers are objects
of string arrays, using canonical HTTP names. Binary bodies use `bodyBase64`;
text bodies use `body`. Base64 takes precedence; set it to null when replacing a
binary body with text. Request method and endpoint are descriptive, not mutable.
Credentials and hop-by-hop headers are stripped before entering or leaving the
extension API. Forwarding always injects the broker-selected account credential.
Neither account access tokens nor refresh tokens are part of extension context.

## Hook contract

- `request`: authenticated, bounded body received, before notice cleanup and
  scrubbing. Change body/headers or return `{response: {status, headers, body}}`
  to bypass routing and upstream entirely, including `/v1/responses/compact`.
- `prepared`: after notice cleanup and scrubbing; same transformation and local
  response contract. Modified requests still face the upstream request-size cap.
- `route`: notification before each account selection, with next attempt number
  and prior failure kind. Account access rules remain owned by the authority.
- `attempt`: selected account ID and attempt number, immediately before forwarding;
  change body/headers or handle locally. Each attempt starts from the prepared
  request, so attempt-local changes do not accumulate across retries.
- `upstream`: raw status and safe headers as soon as forwarding returns headers,
  including responses that will cause a retry. Observation only: reserve activation,
  redirects and auth/quota classification use the actual upstream response.
- `retry`: failed attempt/account/failure-kind notification after closing that
  response, before the next route. Existing retry/failover decisions are retained.
- `response`: final response status/headers before client headers commit. Change
  status/headers or request `buffer: true`. Also applies to local responses and
  broker errors after extension initialization. A registered `body` hook always
  enables buffering, even if a different handler returns `buffer: false`.
- `upstream_event`: parsed Responses SSE event, after observing real terminal
  telemetry and before usage-prefix/native-compaction presentation. Return
  `{event}` to replace or `{event: null}` to suppress it. Raw terminal boundaries
  still end the upstream stream. Comments and non-JSON frames retain their
  existing treatment and do not generate parsed-event hooks.
- `client_event`: final parsed Responses SSE event after built-in presentation,
  immediately before encoding/writing. Same replace/suppress contract. Also
  available for locally returned successful Responses SSE streams.
- `body`: entire final client-bound body, headers and status, buffered before any
  client bytes commit. Replace all of them, including changing SSE to JSON or
  returning binary content. Real upstream usage is observed while consuming the
  original stream. Non-SSE and other endpoints use this escape hatch too.
- `finish`: best-effort cleanup with `complete`, `local`, `error` or `cancelled`.
  Every registered cleanup handler gets a bounded invocation even if another
  cleanup fails. It cannot change an already-sent response.

Authentication failures, initial body-size/read failures, invalid correlation IDs,
and WebSocket upgrade rejection stay outside the extension boundary. Extension
failures themselves also bypass response hooks to avoid recursion. Non-Responses
wire protocols are opaque; use `body` for transformations. This API does not add
agent-session ownership, tool execution loops, arbitrary outbound requests, or
new transport/endpoint support.

## Composition, identity and limits

Handlers run synchronously in filename order, then registration order. Each patch
is visible to the next handler. The first local `response` short-circuits the
remaining request/prepared/attempt handlers. Stream transformations run before
and after the built-in presentation layer as their hook names specify. Once
streaming starts, headers and earlier bytes cannot be retracted. For whole-response
replacement use `body`; this trades streaming latency and memory for that control.
Extensions must keep delta/done/final representations consistent when rewriting
text and must preserve signed/opaque fields they do not understand.

Factories run once per request in independent VMs; closures are extension-scoped
request state and persist across that request's attempts. Enable/disable is
snapshotted before initialization and does not disrupt active requests. Relays
receive disabled IDs on their next normal snapshot sync; offline relays use their
encrypted cached snapshot. Source files must be provisioned on each relay
separately. Old relay binaries ignore the additive settings field; upgrade those
binaries before expecting central toggles to affect them.

Context contains the broker request ID and authenticated client-key ID. Native
`thread-id` is preferred, then `client_metadata.thread_id`, JSON
`x-codex-turn-metadata.thread_id`, and the explicit broker correlation header.
Turn identity uses client metadata, turn metadata and then the broker header.
Missing identity remains unknown; IP addresses, `session-id` and cache affinity
are never used to infer threads. This is separate from the existing telemetry
correlation contract, which is unchanged. Native identity is client supplied,
not independently verified; authenticated client-key IDs provide the namespace.

`updateThread` atomically reads/modifies JSON state scoped to extension ID,
client-key ID and native thread ID. No identity means an error. Concurrent
requests cannot lose updates within this callback; multiple callbacks across a
request are not a transaction. Returning null deletes the entry. State is
process-local, survives toggles, and is lost on restart; it is not shared between
replicas/relays and is not suitable as a durable session database. There are at
most 1,024 entries, each at most 64 KiB; capacity exhaustion rejects an update
rather than silently evicting another thread's state.

There are at most 32 modules, 1 MiB source per module, 32 handlers per hook/module,
a 256-frame VM call stack and a 100 ms wall-clock budget per factory/handler.
Hook JSON input/output is capped at 128 MiB, buffered/synthetic responses at
64 MiB. Existing request, SSE frame and stream-idle limits still apply. Large
transformations may hit the hook budget before the byte cap. Hooks execute on
the request path, preserving backpressure; cancellation interrupts JavaScript.
Errors fail closed with a generic 502 before headers or abort the connection
after commit. Request-stage failures identify the failing extension/hook in logs;
client errors never include exception details or request bodies. Streaming-hook
failures abort the response. Cleanup has a fresh bounded context after cancellation.

These are trusted operator extensions, not a hostile-code sandbox. Goja shares
process memory; the byte/time limits are not a hard heap quota, and expensive
native VM operations may not be promptly interruptible. There is no per-module
process isolation. No VM, body buffering or additional event serialization is
used when only the native default extension is active.

## Regression checks

Run `go test ./...`, `go vet ./...`, and
`go test -race -timeout=20m ./...`. The longer race timeout accommodates repeated
SQLite migration fixtures on shared development machines. Runtime and gateway
tests cover TypeScript compilation, ordering, local handling, cancellation,
timeouts, state isolation, and failure cleanup. HTTP/relay tests verify settings,
active-request snapshots, replay cleanup, failover, response transformations,
raw telemetry and offline relay configuration. Stream tests exercise split
frames, terminal suppression, native compaction and opaque payload preservation.

Run `npm ci`, `npm run check:proxy-extensions`, `npm run test:proxy`,
`npm run test:usage`, `npm run test:enrollment`, `npm run test:pi` and
`npm run check:pi` for declarations, examples and existing JavaScript integrations.
Packaging checks are `CGO_ENABLED=0 go build ./cmd/codex-broker`,
`nix build --no-link` and `nix flake check --no-build` (the last command evaluates
module definitions; it does not execute the NixOS VM tests). Protocol regressions
use local fake upstreams and isolated databases, not production accounts.


## Storage permissions and telemetry boundary

Custom TypeScript extensions cannot open SQLite, read/write JSON files or create
folders. Their runtime exposes no filesystem, process or network APIs. Closures
provide request-local state; `updateThread` provides bounded, atomic JSON state
in memory (64 KiB per entry, up to 1,024 entries). This is lost on restart and is
not shared across processes. Durable response history for a context-management
extension needs an additional storage API; none is implemented here. Native
built-ins are compiled host code rather than sandboxed TypeScript, but neither
built-in writes its own transcript/history files. `extensions/` contains source,
not per-extension runtime data.

Existing telemetry records request metadata and upstream terminal usage, not full
request/response bodies or transcripts. Request metadata is parsed from the actual
attempt body after all request/prepared/attempt transformations, immediately before
upstream forwarding. Raw response status and header timing are captured when the
upstream returns. Terminal response identity, model, status and token usage are
observed before `upstream_event`, built-in presentation/native-compaction handoff,
`client_event` and buffered-body transformations. Consequently client-visible
rewrites and local synthetic responses do not fabricate upstream usage. There is
no added request token estimator, body capture or second telemetry system.

## Extension-defined settings

Export a named `settings` constant alongside the default factory, using
`SettingsSchema` from the TypeScript declarations. See
`packages/proxy-extension/examples/settings.ts`. Each field declares a unique
alphanumeric/underscore `key` (starting with a letter), `type`, `label`, and
explicit JSON `default`. Schemas are checked when modules load; module-level code
runs during discovery as well as request initialization, so keep it declarative.
A module without settings needs no settings export. Schemas support up to 64
fields and 64 KiB; values are also limited to 64 KiB.

Available controls:

| Types | Value and controls |
| --- | --- |
| `text`, `textarea`, `password`, `email`, `url`, `tel`, `path` | Text; optional `required`, `minLength`, `maxLength`, Go regex `pattern`. Paths are text and grant no filesystem access. |
| `number`, `integer`, `slider` | Numbers; optional `min`, `max`, `step`. Integer requires a whole number; slider defaults to range 0–100. |
| `percentage`, `ratio`, `duration` | Numbers; defaults to ranges 0–100, 0–1, and nonnegative milliseconds respectively; bounds can be overridden. |
| `boolean` | Checkbox with a boolean value. |
| `select`, `radio`, `multiselect` | String or string array; declare `options: [{value, label}]`. Multiselect rejects duplicates. |
| `color` | Six-digit hex color such as `#336699`. |
| `date`, `time`, `datetime` | Local date/time strings (`YYYY-MM-DD`, `HH:mm[:ss]`, `YYYY-MM-DDTHH:mm[:ss]`), without timezone conversion. |
| `string-list`, `key-value`, `json` | JSON editors for string arrays, string-valued objects, or any JSON value. |

All fields support `description`, `placeholder`, `group`, `unit`, and
`visibleWhen: {key, equals}`. Conditions control display only; hidden fields still
retain and validate their values. They cannot reference password fields.
`required` prevents empty text, lists, and maps; boolean false and numeric zero
remain valid. Server validation is authoritative. No arbitrary HTML, JavaScript,
file upload or extension-supplied dashboard component is executed.

In Settings → Proxy extensions, each configurable extension has a collapsed
Settings panel. Opening it loads saved values. Apply validates and saves all
fields atomically and announces success or errors. Cancel closes and discards
unapplied edits. Reset to defaults loads defaults into the draft; Apply is still
required, and other fields can be edited before applying. Revisions prevent two
open editors from silently overwriting each other. Reopen after a conflict.
Settings can be configured while an extension is disabled.

`proxy.getSettings<T>()` returns a fresh JSON clone. The request factory can read
settings immediately, then `settings_loaded` runs after handler registration.
`settings_changed` receives `{settings, previous}` before the next hook when the
host has published a change. Both events are notifications; returned patches are
ignored. Settings stay consistent during a handler. These hooks use the same
synchronous execution limits and failure behavior as other hooks. Changes may
coalesce if multiple saves happen between hooks. Idle sessions do not receive a
background callback. Finish cleanup uses its last observed settings snapshot.
Native built-ins and enable/disable flags retain their request-start snapshot.

Values persist in Broker's SQLite settings table and survive restarts. Password
fields are encrypted using the configured vault, omitted from browser responses,
and shown only as a saved/not-set indicator; nonempty secrets require a vault.
Leave a password blank to preserve it, or select Clear saved value to clear it.
Reset restores its declared default when applied. Do not put real credentials in
source defaults. Ordinary settings are not encrypted. Trusted extension code can
read its own password settings through `getSettings`; protect the extension
source directory accordingly.

Apply publishes settings to active primary sessions at their next hook boundary.
Relays receive values through normal authenticated snapshot sync and encrypted
snapshot storage. Each relay publishes its latest snapshot on the next incoming
request; active sessions see it at their next hook afterward. Provision matching
extension schemas on primary and relays. Updating a schema requires a restart;
remove or migrate incompatible saved values before changing field types/keys.
Settings persistence does not add durable transcript storage or filesystem access
to the TypeScript runtime.

Built-in defaults preserve previous output: account usage shows the account label
and retry status; request size compaction uses a 50 MiB trigger and 40 MiB target.
Its settings enforce target < trigger and keep the host's hard limits unchanged.

Optional real-browser settings regression (uses an isolated temporary broker,
SQLite database, vault and synthetic extension, with no upstream credentials):
install Playwright and its Chromium browser in a temporary directory, then run
`BROKER_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node scripts/extension-settings.browser.mjs`.
It checks every control plus Apply/Cancel/Reset, secret redaction/preservation,
validation feedback, revision conflicts, subpath routing and a narrow viewport.
