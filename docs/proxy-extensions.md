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
payload. Like Pi, custom extensions now run on Node with normal filesystem,
network and package access. Broker retains its Go data plane and uses esbuild to
compile TypeScript, with one long-lived Node process per custom extension.
The two built-ins remain native Go. This is a proxy API, not compatibility with
Pi's UI, tools or session-manager APIs.

Billion-context (bebc239f2767e1a35c7664755f3ea8cb51faa641) motivates request
reconciliation and compact endpoint interception. No compression, private tool
loop, fabricated encrypted compaction payload or history dropping is included.

## Authoring and operation

The built-in `account-usage` extension is enabled by default. It is a native
adapter around the existing formatter and streaming reconciliation, avoiding a
Node worker on ordinary requests. Settings → Proxy extensions controls it and all loaded
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

To provision a trusted module, place a `.ts` entry file in an operator-controlled
directory and set `WINDOWKEEPER_PROXY_EXTENSIONS_DIR` to its absolute path. Restart
the broker. The filename without `.ts` is its ID (lowercase letters, digits, `_`
and `-`, up to 64 characters); built-in IDs are reserved. `.d.ts` files are ignored.
Entry files load in filename order. Put local helper modules in subdirectories
so they are not also discovered as extensions. Local imports are bundled at load
time; npm packages remain external and resolve from the entry file's directory
and its ancestor `node_modules` directories. Provision dependencies there before
starting Broker. There is no installer, hot reload, upload or marketplace.

Custom extensions require **Node 22.13 or newer** on Linux/macOS. The worker and
TypeScript compiler are embedded in the Go binary; Node itself is an external
executable. Set `WINDOWKEEPER_EXTENSION_NODE` to an absolute Node executable if
it is not on PATH. Both Docker images include Node; the Nix package supplies it
through its wrapper. For standalone binaries, install Node separately. Native
built-ins work without Node; unavailable Node isolates custom modules as load
errors shown in Settings. The pipe bridge currently uses Unix extra descriptors;
Windows usage-client builds are unaffected, but Windows proxy workers are not
supported.

Mount extension source and dependencies read-only in Docker, including relays;
keep the Broker data directory writable and persistent. For NixOS, set
`systemd.services.codex-broker.environment.WINDOWKEEPER_PROXY_EXTENSIONS_DIR`.
Nix's service filesystem restrictions still apply to extensions. Package native
bindings must match the deployed OS, architecture and Node ABI; Broker does not
install or rebuild them. Docker's primary image does not include npm, so prepare
`node_modules` in a matching build environment.

The default export is a factory taking `ExtensionAPI`. Factories and handlers may
return Promises; synchronous existing extensions continue to work. Use the types
in `packages/proxy-extension/index.d.ts` via `import type`. Normal Node built-ins
such as `node:fs/promises`, `node:path`, `node:http` and `node:sqlite` are available.
TypeScript is compiled to CommonJS; npm packages must be compatible with Node's
`require` support, or be loaded using dynamic `import()` (for example, ESM with
top-level await). Top-level await in the extension entry itself and `import.meta`
are not supported by this compilation mode; use an async factory and ordinary
`__dirname`/`__filename` instead. esbuild transpiles without type-checking; run
`tsc --noEmit` for your extension and `npm run check:proxy-extensions` for examples.

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
failures skip the affected extension and preserve its input. Non-Responses
wire protocols are opaque; use `body` for transformations. This API does not add
agent-session ownership, tool execution loops, or new proxy transport/endpoint
support. Extensions may make their own outbound requests using Node APIs.

## Composition, identity and limits

Handlers are awaited in filename order, then registration order. Each patch
is visible to the next handler. The first local `response` short-circuits the
remaining request/prepared/attempt handlers. Stream transformations run before
and after the built-in presentation layer as their hook names specify. Once
streaming starts, headers and earlier bytes cannot be retracted. For whole-response
replacement use `body`; this trades streaming latency and memory for that control.
Extensions must keep delta/done/final representations consistent when rewriting
text and must preserve signed/opaque fields they do not understand.

Factories run once per request; closures are extension-scoped request state and
persist across that request's attempts. The entry module is evaluated afresh for
each request, while imported npm modules use Node's process-wide cache. Avoid
request data in imported-module globals. A worker serializes invocations across
its requests; separate extensions have separate processes. This preserves the
synchronous atomic `updateThread` API, at the cost of queueing concurrent hooks
for the same extension. A slow hook can delay other requests using it. Enable/disable is
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
held by Go, survives toggles and worker restarts, and is lost on Broker restart; it is not shared between
replicas/relays and is not suitable as a durable session database. There are at
most 1,024 entries, each at most 64 KiB; capacity exhaustion rejects an update
rather than silently evicting another thread's state.

There are at most 32 modules, 1 MiB source per module, 32 handlers per hook/module,
up to 1,024 active request instances per worker, and a 100 ms wall-clock budget
per handler, including async I/O. Output JSON serialization has its own 1-second
budget (getters can execute code). Input parsing and
pipe transfers have separate 5-second budgets; copying large bodies does not
consume the handler budget. Worker startup and queueing are separate.
Module discovery and request factory initialization have a 1-second budget to
allow Node to resolve/import packages; process startup has a separate 5-second limit. Node uses a 512 MiB V8 old-space
limit; this is not a total process-memory quota.
Hook JSON input/output is capped at 128 MiB, buffered/synthetic responses at
64 MiB. Existing request, SSE frame and stream-idle limits still apply. Large
transformations may hit the hook budget before the byte cap. Hooks execute on
the request path, preserving backpressure; cancellation terminates an executing worker. Waiting for a busy worker is
context-cancellable without terminating the other request's invocation.
Extension errors are logged and skipped, preserving the input to the failing
extension for that invocation. Invalid patches are isolated before committing.
Client responses do not contain exception details. Actual upstream errors and
resource limits still follow host error handling. Cleanup has a fresh bounded
context after cancellation. See Failure isolation and extension logs below.

These are **trusted operator extensions, not a sandbox**. Node runs as the
Broker OS user and can access whatever that account and service confinement
permit, including potentially Broker files and inherited environment secrets.
A separate process contains ordinary crashes; it does not restrict filesystem,
network or process authority. The extension context still omits account tokens.
The built-in-only path starts no worker and adds no IPC or body buffering.

A timeout, process exit or cancellation during execution kills that extension's
worker. Other extensions continue. All request closures in the killed worker are
lost: their next hook fails open, and their cleanup cannot run. New requests
restart the worker; Go-owned thread state and filesystem data survive. Ordinary
JavaScript exceptions retain the process and allow best-effort `finish` handlers.
Worker termination cannot undo filesystem writes, database commits, network
requests or child processes started by an extension. Await work inside hooks;
background API calls after the invocation ends are unsupported. Background
`proxy.log` calls are dropped rather than attributed to another request.
`finish` should close extension-owned handles; Broker releases its request
instance afterward and closes workers when the HTTP server shuts down.

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

Use normal Node filesystem/database APIs for durable extension data. Broker does
not provide a storage abstraction. `proxy.getDataDir()` creates and returns the
absolute `<Broker data directory>/extensions/<extension-id>` path (0700 on
creation). IDs, rather than display names, distinguish directories; the same ID
keeps its files across requests, toggles, worker restarts and Broker restarts.
Renaming the ID selects a new directory. Disabling/removing an extension does not
delete its files. Source in repository `extensions/` is separate from this data.

The helper is a convention, **not a filesystem sandbox**. Extensions can create
JSON files, SQLite databases, subdirectories or other storage, and can also access
paths outside it when OS permissions allow. Manage migrations, transactions,
retention, locks and backups in the extension. Replicas and relays have their own
local directories; Broker does not replicate these files. Scope stored session
history by authenticated `clientId` plus native `threadId`, and handle absent
identity explicitly. See `packages/proxy-extension/examples/storage.ts`.

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
execution limits and failure behavior as other hooks. Changes may
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
extension schemas on primary and relays. Updating a schema requires a restart; incompatible saved values skip that
extension until the values are reset/migrated to match its new schema.
Settings stay managed by Broker independently of extension-owned filesystem data.

Built-in defaults preserve previous output: account usage shows the account label
and retry status; request size compaction uses a 50 MiB trigger and 40 MiB target.
Its settings enforce target < trigger and keep the host's hard limits unchanged.

Optional real-browser settings regression (uses an isolated temporary broker,
SQLite database, vault and synthetic extension, with no upstream credentials):
install Playwright and its Chromium browser in a temporary directory, then run
`BROKER_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node scripts/extension-settings.browser.mjs`.
It checks every control plus Apply/Cancel/Reset, secret redaction/preservation,
validation feedback, revision conflicts, subpath routing and a narrow viewport.

## Failure isolation and extension logs

Each extension's transformation is transactional for one hook invocation. Broker
keeps the payload entering that extension, chains its handlers on a candidate,
and validates returned patches before committing. A throw, timeout, rejected
Promise, serialization failure, invalid patch, or unexpected native-extension
panic discards that extension's candidate and passes the previous payload to the
next extension. Successful earlier extensions retain their changes. The failed
extension is skipped for the remainder of this request; a new request tries it
again. A successful synthetic response still short-circuits normally.

Initialization, module-load and incompatible saved-settings failures isolate the
affected extension as well. Invalid filename IDs are shown under a stable
`invalid-…` diagnostic ID. An unreadable/nonexistent configured source directory
or a failed host configuration/database read remains a host error. Expected
request-size rejection, authentication, actual upstream failures, cancellation,
and hard resource limits retain their host behavior. Cleanup handlers get
independent bounded best-effort calls even after an earlier failure.

Rollback applies to the payload for the failing invocation. It cannot undo logs,
`updateThread` changes, filesystem or database writes, extension closure state,
earlier successful invocations,
or bytes already sent to the client. A failed streamed-event transformation
forwards that event's previous value and continues the stream. Whole-body hooks
still require buffering and retain the existing buffering size cap.

Use the structured logger from any factory or hook:

```typescript
proxy.log('info', 'Added usage to the answer', { responseId: 'example-response' });
proxy.log('warning', 'No prior context found', { operation: 'lookup' });
proxy.log('error', 'Stored context could not be read', { operation: 'read' });
```

Levels are `debug`, `info`, `warning` (`warn` also works), and `error`. Fields must
be a JSON object. Broker supplies the extension ID, hook, request/client/native
thread/turn IDs and UTC timestamp; supplied fields cannot override that envelope.
`error` logs also update the last-known error, without disabling an otherwise
successful extension. Automatic failure logs include bounded error/stack details.
Exception formatting itself is bounded because custom `toString` methods can
throw or loop. Logging-sink failures do not fail the proxy.

Authored entries are limited to 16 KiB each, 100 entries and 256 KiB total per
request across extensions; excess entries are dropped with one warning. Oversized
entries are rejected as invalid API calls; persisted messages/details may be
truncated further after sanitization. Automatic failures are recorded separately.
Known password-setting values observed during the request, recognized secret
keys, token patterns and URL queries are redacted. This is best-effort sanitizing,
not a guarantee that arbitrary transcript text contains no sensitive data. Avoid
logging credentials or full conversations.

Entries use the existing `proxy.extension.log` and `proxy.extension.failed` events
in `<log-dir>/windowkeeper.jsonl`, with existing rotation (25 MiB/daily), archived
retention (30 days, 1 GiB directory budget), and dashboard/export support. The
recent dashboard history (up to 2,000 records) is restored from a bounded log tail
after restart. Settings → Proxy extensions shows the last error time with a
collapsed “View last error” section and a View logs link. The latest error is
also retained separately in Broker's SQLite settings table, so log rotation or a
successful later request does not erase the diagnostic. Reload Settings to see
new errors. Existing enable/disable and per-extension settings remain independent.

Relays persist sanitized diagnostics in their encrypted outbox and forward them
to the primary, where they become visible in the same dashboard. Snapshot
`ExtensionLogging` advertises receiver support: diagnostics remain queued when
connected to an older primary, while usage/failure reconciliation continues.
Batches are bounded below the receiver body cap; diagnostic entries are evicted
before operational records if critical/telemetry events need queue space. Logs
cannot be guaranteed if disk writes fail or the bounded outbox is exhausted.

This adapts Pi 0.84.4's runner pattern: catch handler exceptions, emit attributed
errors, and continue processing. Pi exposes `onError`/`emitError` internally;
interactive mode displays errors, RPC emits `extension_error`, and print mode
writes errors to stderr. Extensions can use host `console.*` and
`ctx.ui.notify(message, 'info' | 'warning' | 'error')`; notifications are UI/RPC
messages. Its inspected ExtensionAPI has no dedicated persistent per-extension
severity logger. `appendEntry` can persist custom session data, and its full host
runtime also permits authors to build file logging themselves. Broker's logger
adds service-oriented persistence and request attribution without introducing
notifications into client model responses. Normal console/stdout/stderr output is
discarded by the worker host; use `proxy.log` for attributed, persisted diagnostics
or write extension-owned log files with Node APIs.
