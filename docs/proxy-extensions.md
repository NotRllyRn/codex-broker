# Proxy extensions

## Implementation contract

The gateway remains responsible for authentication, account selection, credential
injection, failover, scrubbing, upstream validation and real usage accounting.
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
