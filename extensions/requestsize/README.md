# Request size compaction

ID: `request-size-compaction`. Enabled by default; globally toggleable under
Settings → Proxy extensions.

This is the existing incoming Responses request-size workaround, moved intact
from the proxy. It cleans history at 50 MiB, targets 40 MiB, preserves protected
turns, call/output pairs and opaque payloads, and retains its existing rejection
errors and metrics. After cleanup changes a normal Responses request, it sends
the existing client-visible native-compaction signal on a successful terminal
event. It does not arm that signal for dedicated compact requests or compaction
triggers. The host records the real upstream token usage before this signal.

Disabling the extension stops both automatic trimming and this handoff. The host
still enforces its 128 MiB Responses inbound cap, 64 MiB forwarding cap and
large-body concurrency guard. A custom request extension can reduce a body itself;
an unreduced request above the forwarding cap is rejected. Account-usage replay
cleanup is independent and continues to remove old answer prefixes.

A request snapshots its enabled state. Changing the global setting does not
interrupt active requests. Relays use their last synchronized setting, including
while running from their encrypted offline cache.

Settings expose `triggerMiB` (default 50, range 1–64) and `targetMiB`
(default 40, range 1–63). Target must be smaller than trigger. Changes apply to
new requests; host inbound and forwarding caps remain enforced independently.
