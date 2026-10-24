# Device usage sync

## What belongs to an identity

An identity owns usage, not account access. Create “Tim” in Settings and enroll
several devices with the same generated code. Each device gets a separate ID
and upload-only credential. The identity code cannot authorize proxy requests
or uploads; it only enrolls devices. Upload credentials cannot read usage,
change an identity, or enroll another device. Server-side credentials are
hashed. The administrator alone can read dashboards and manage identities.

Codes are shown only when created or rotated. If one is lost, choose **Generate
new code**. Existing enrolled devices remain connected. Revoke compromised
devices separately. Deleting an identity permanently removes its devices,
proxy-key assignments, and identity usage history. Ordinary account/proxy
history remains unchanged.

## Download and install

Use **Settings → Download the usage sync helper**, then run the command in the
README. The downloaded file is standalone: Python 3.9+ is the only runtime
dependency. Neither Node, ccusage, SSH, nor an OpenTelemetry Collector is needed.

For a command-line download on Linux/macOS using an existing trusted private CA:

```sh
curl --fail --show-error --cacert broker-ca.pem \
  https://192.168.1.20:8787/static/usage-sync.py -o usage-sync.py
python3 usage-sync.py install --broker-url https://192.168.1.20:8787 \
  --ca broker-ca.pem --name 'Work laptop'
```

With a publicly trusted or system-trusted certificate, omit the CA arguments.
If Broker uses a root path, include it in both URLs, for example
`https://broker.example/broker/static/usage-sync.py` and
`--broker-url https://broker.example/broker`. On Windows, download using the
Settings link, then run:

```powershell
py -3 .\usage-sync.py install --broker-url https://192.168.1.20:8787 `
  --ca C:\certs\broker-ca.pem --name 'Windows desktop'
```

The private prompt accepts the `cbi_…` code, never the identity name. Do not put
the code in command-line arguments, URLs, or scheduled tasks. Automation may
use `--code-stdin` with a private stdin stream. Installation saves the code
privately only until enrollment succeeds, then removes it. If enrollment's
reply is lost, retrying uses the same installation ID and device secret.

From a repository checkout, the equivalent command is:

```sh
scripts/install-usage-sync.sh --broker-url https://192.168.1.20:8787 --ca broker-ca.pem
```

The helper installs itself in `~/.codex-broker-usage/`, alongside its restricted
configuration, SQLite checkpoints and numerical upload queue. Keep this folder
on the device; do not copy it to another device. The default device name is its
hostname. Running `install` again reuses that installation's configuration and
repairs its schedule. To enroll a separate identity from the same computer,
use a different `--state-dir` on every command for that installation.

## Direct and proxied requests

Assign each inference client key to its identity in Broker Settings. Future
proxied requests then contribute original upstream usage, including responses
through a relay after it reconnects. Earlier proxy traffic is not backfilled
or reassigned when a key's identity changes. Existing identity history keeps its
original owner. Proxy totals appear under **Assigned proxy keys**, independently
of enrolled device names. Give each app its own key for access/revocation.

At client setup, choose a route for each source:

- `direct`: import the local usage records, including requests made through
  another provider or gateway which this Broker does not otherwise count.
- `broker`: omit this source's local counts. This Broker already accounts for
  requests made with the identity's assigned proxy key.
- `off`: do not collect that source.

These choices are explicit because logs do not always identify their proxy
destination, and Broker sometimes changes returned input counts to trigger
compaction. We cannot reliably infer the route from a model/provider name.
**Do not mark a mixed direct/proxy directory as direct.** Keep separate agent
homes/session directories and give the helper only direct directories:

```sh
python3 usage-sync.py install --broker-url https://broker.example \
  --codex-mode direct --codex-path ~/.codex-direct/sessions \
  --codex-path ~/.codex-direct/archived_sessions --pi-mode broker
```

Default discovery uses `$CODEX_HOME/sessions` and `archived_sessions` (or
`~/.codex`), and `$PI_CODING_AGENT_SESSION_DIR` or
`$PI_CODING_AGENT_DIR/sessions` (or `~/.pi/agent/sessions`). Repeat `--codex-path`
or `--pi-path` for extra direct-only directories. Run installation from a
terminal with the relevant environment variables set; resolved paths are saved
so scheduled runs do not depend on shell environment initialization.

## Accuracy and retention

The helper supports plain JSONL Codex rollouts and Pi sessions. It prefers
Codex's modern `token_usage_record` per-response records, ignores mirrored
`token_count` events, and includes separately recorded compaction requests.
Older rollouts use advancing cumulative token totals and legacy fork-prefix
suppression. Model metadata is required; malformed/incomplete usage is reported
instead of silently assigned a model. Pi assistant, tool-result, compaction,
branch-summary and standalone usage records are included. Reasoning is part of
output; cached reads and writes are parts of input, not extra tokens.

Missing/deleted logs and usage the agent never recorded cannot be recovered.
Compressed `.jsonl.zst` rollouts are not supported by this standard-library
helper; retain or restore plain JSONL for import. ccusage and T3 can differ
because their supported record types, compaction and replay handling differ.
Unknown prices and cache-write costs unsupported by the current price model
are excluded from API cost estimates; token counts remain visible.

Only numerical usage and minimal event metadata leave the device. Provider
response IDs (Codex) or stable entry identities (Pi/legacy rollouts) produce
deduplication keys independent of the uploading device. A copied event counts
once within an identity; its device view shows the first uploader. Codex per-response copies
re-stamped by the agent use the earliest observed time. Conflicting numerical
payloads are rejected atomically and retained for investigation, never summed.
Deduplication across legacy fork formats is limited to the history identity
and replay information those formats retained.

The server retains identity usage events and deduplication keys independently
of its seven-day proxy response cleanup. Daily charts and **All time** totals
continue to work. The sync helper stores file positions/parser state and queues
events before upload. Server acknowledgements follow a database commit. Lost
replies can cause retransmission but cannot add the same event twice.

Changed files are checked regardless of their date. Incomplete final lines are
left for a later scan. File replacement/truncation and a changed checkpoint
tail trigger rescanning; a file edited earlier in place without changing its
checkpoint tail requires an explicit `rescan`. The helper does not perform a
two-day-only import: any unsynced retained history is eligible, even after
weeks offline. Do not delete unscanned logs or the local upload queue before
successful synchronization.

## Scheduling and connectivity

Clients make outbound HTTPS requests to Broker's existing listener, through
the LAN or your existing VPN/Tailscale connection. They need no inbound port,
SSH key, additional server, or persistent socket. Trust the actual Broker CA;
redirects are rejected, and TLS verification is never disabled.

- **Linux:** installs a current-user systemd service/timer under
  `~/.config/systemd/user/`, with hourly checks, startup catch-up and persistent
  missed-calendar runs. The user's service manager must be available. To run
  while logged out, enable lingering for that user using your normal system
  administration procedure (`loginctl enable-linger USER`).
- **macOS:** installs a LaunchAgent under `~/Library/LaunchAgents/`, running at
  login and hourly while the user session is available. Diagnostics go to
  `~/.codex-broker-usage/sync.log`.
- **Windows:** installs an interactive current-user Task Scheduler task,
  triggered at login and hourly, with missed-run catch-up. No stored Windows
  password is needed. Python must remain installed at the same executable path.

Offline failures leave checkpoints and queued events intact; the next hourly
check retries. A sleeping/offline machine does not maintain a connection.
Upload resumes after the scheduler runs with connectivity. No promise of an
immediate network-change trigger is required for this first implementation.

For servers without a supported per-user scheduler, use `--no-schedule` and
schedule `python3 ~/.codex-broker-usage/usage-sync.py sync` with your existing
scheduler (for example hourly cron). First install still imports immediately.

## Status, recovery, updates and removal

Linux/macOS commands (on Windows substitute `py -3` and
`$HOME\.codex-broker-usage\usage-sync.py`):

```sh
python3 ~/.codex-broker-usage/usage-sync.py status
python3 ~/.codex-broker-usage/usage-sync.py sync
```

The status command shows tracked files and queued events without credentials.
Settings shows last successful sync and per-device revocation. A failed batch
does not advance the server acknowledgement. Authentication errors need a
valid enrollment/device credential; an offline or failed first enrollment is
automatically retried with the saved identity code.

To recheck all retained history after restoring files or correcting an earlier
in-place edit:

```sh
python3 ~/.codex-broker-usage/usage-sync.py rescan
python3 ~/.codex-broker-usage/usage-sync.py sync
```

Existing events are deduplicated. Conflicting usage requires investigation
instead of overwriting historical counts. Download a newer helper and rerun
`install` to update the installed script without changing its identity or
checkpoints. If route configuration needs changing, use a new state directory
and revoke the old device; previously misclassified usage needs administrator
correction before totals can be considered accurate.

Remove the background schedule with:

```sh
python3 ~/.codex-broker-usage/usage-sync.py uninstall
```

This retains local queued data and credentials for recovery. Revoke the device
in Settings to prevent future uploads; remove the local state directory only
after any wanted queued usage has reached Broker. Server history remains until
its identity is explicitly deleted.
