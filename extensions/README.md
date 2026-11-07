# Built-in extensions

This directory contains every extension compiled into Codex Broker. The catalog
in `builtins.go` supplies their Settings entries. Both are enabled by default;
settings apply globally to new requests and propagate to relays in snapshots.

- [`accountusage`](accountusage/): account/quota answer prefix and replay cleanup.
- [`requestsize`](requestsize/): oversized request-history cleanup and the related
  native Codex compaction handoff.

These are native Go extensions so existing byte handling and large-body memory
behavior remain intact. They run at the same preparation/presentation boundaries
as the TypeScript extension pipeline, without serializing large bodies into a VM.

Custom TypeScript modules are currently provisioned through
`WINDOWKEEPER_PROXY_EXTENSIONS_DIR`. This repository folder is the source catalog
for built-ins; it is not an installer or a writable production data directory.
Custom extension installation and persistent storage APIs are separate future
work. See [the extension API](../docs/proxy-extensions.md) for authoring details.
