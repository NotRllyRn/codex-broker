# Device usage sync

## One-time setup

1. Create an identity in **Settings → Usage identities**. Copy its generated
   `cbi_…` enrollment code; it is shown only once. Use that same code on every
   device that should contribute to the identity. Names cannot authorize uploads.
2. Download the matching `codex-broker-usage-OS-ARCH.zip` from
   [Releases](https://github.com/NotRllyRn/codex-broker/releases). For an unreleased
   branch, open its successful **CI** run under
   [Actions](https://github.com/NotRllyRn/codex-broker/actions), download the
   **usage-sync-bundles** artifact, and select your platform zip inside it.
3. Extract the zip. Keep **both executables** together for installation:
   `codex-broker-usage` and `ccusage` (both end in `.exe` on Windows).
4. Run the command below, replacing the URL with your Broker address. Enter the
   identity code at its hidden prompt. Do not put the code in a URL or argument.

**Linux/macOS:**

```sh
chmod +x codex-broker-usage ccusage
./codex-broker-usage install --broker-url https://192.168.1.20:8787
```

**Windows PowerShell:**

```powershell
.\codex-broker-usage.exe install --broker-url https://192.168.1.20:8787
```

Python, Node, Go, and ccusage do **not** need to be installed beforehand. The
bundle includes the native ccusage executable. Targets are Linux, macOS
(`darwin`), and Windows, each with `amd64` (Intel/AMD x64) and `arm64` (Apple
Silicon/ARM64). Older OS releases, 32-bit systems, Android, and iOS are not
claimed to be supported. macOS or Windows may require approving a downloaded
unsigned executable through the OS's normal security UI.

For a private Broker certificate, add `--ca /path/to/broker-ca.pem` (Windows:
`--ca C:\certs\broker-ca.pem`). The helper copies the CA into its private state.
For a URL root path, include it: `--broker-url https://broker.example/broker`.
TLS verification is mandatory, and redirects are rejected. Connections are
outbound HTTPS to the existing Broker listener, over LAN/VPN/Tailscale; no SSH,
inbound device port, persistent connection, or OTel Collector is needed.

Use `--name 'Work laptop'` to override the hostname. Separate devices enrolled
with the same code have independent upload-only tokens and revocation controls.
Codes authorize enrollment only. Broker stores credential hashes and erases the
client's saved identity code after successful enrollment. Lost enrollment replies
are safely retried using the same installation ID and device token. Only an
administrator can read the dashboard or manage identities.

## What is uploaded and counted

The helper runs **ccusage 20.0.43**, pinned with published SHA-512 package hashes:

```sh
ccusage daily --by-agent --json --timezone UTC --offline --config ccusage.json
```

Every agent detected by that ccusage version is included. **Direct requests and
HTTP proxy requests are both included.** There are no direct/Broker routing
questions or agent allowlists. Broker's existing proxy/account usage view stays
separate from identity totals. Proxy requests without a corresponding client
log cannot appear in ccusage; this does not import server-only proxy history.

Each upload contains the raw ccusage **daily JSON rows**, including per-agent
and model breakdowns, plus a device credential, protocol version, pinned
ccusage version, and per-day revision. It does not contain conversation logs,
prompts, responses, or tool outputs. Raw ccusage report fields are preserved;
Broker does not maintain parsers for individual agents. Token totals and API
cost estimates come from ccusage. These costs are not subscription payments.
Because this is aggregated data, individual response counts, cost-by-speed/type,
and cache-savings estimates are not fabricated in the identity dashboard.

The first sync scans all retained history. Later syncs scan all retained logs
again, but upload only changed days. Scanning is delegated to ccusage; this
trades some local scan time for broad format support and recovery of old data.
The helper saves compact reports locally instead of copying original JSONL.

Broker keeps **one row per device and UTC day**. A newer revision replaces its
cumulative totals; it is never added on top of the previous snapshot. Identical
retries are no-ops. Conflicting contents under the same revision fail atomically.
Stale revisions fail with a recovery error instead of overwriting newer data.
Thus retrying a report or adding more usage to the current day does not count
previously uploaded requests again. ccusage handles duplicate records within
its own sources. Different devices contribute independently; copied histories
or separate installations scanning the same logs are not reconciled.

Identity snapshots survive the seven-day proxy response cleanup. Missing days
never delete existing server history. If a day's token counts shrink (including
an agent/model disappearing after log cleanup), the helper retains its previous
report, reports the issue, and still uploads other valid days. Restore the logs,
or use `sync --allow-decrease` only for an intentional correction. This option
permits replacing totals with lower values; it does not recover deleted logs.

## Background operation and outages

Installation copies both executables, configuration, and a durable report/
acknowledgement journal into `~/.codex-broker-usage-native/`. Protect and keep
this directory intact. Do not share it between devices or restore an older
journal over a newer one. Credentials and state use private file permissions
on Unix and the current user's profile directory on Windows.

It installs hourly retries plus startup/login catch-up:

- **Linux:** current-user systemd service/timer, with persistent missed runs.
  A working user service manager is required. For operation while logged out,
  enable lingering using your system's administration procedure.
- **macOS:** LaunchAgent running at login and hourly during the user session.
  Diagnostics append to `~/.codex-broker-usage-native/sync.log`.
- **Windows:** Task Scheduler, current-user logon and hourly triggers, with
  missed-run catch-up. No stored Windows password is required; the user must
  be logged in. No extra scripting runtime is used by the scheduled command.

Use `install --no-schedule` if your system has no supported scheduler. Schedule
its installed `sync` command hourly using your own scheduler. An initial
connection failure leaves installation, enrollment state, reports, and retries
intact. While offline, scheduled runs continue capturing retained local history.
Pending reports are acknowledged before newer snapshots are sent. Reports can
catch up after weeks offline; history that was deleted before ccusage could
read it cannot be recovered. A sleeping machine retries when its scheduler
next runs with connectivity; this is not an immediate network-change trigger.

Source directory environment variables such as `CODEX_HOME`,
`CLAUDE_CONFIG_DIR`, and `PI_CODING_AGENT_DIR` are saved during installation.
Run setup in the environment you use for your agents, with absolute source
paths. An empty private ccusage config avoids inheriting unrelated working-
directory filters. For custom sources/options, pass
`--ccusage-config /absolute/path/to/ccusage.json`; installation copies it.
See [ccusage configuration](https://ccusage.com/guide/configuration).
Avoid date filters if you want complete history. Rerun installation to update
copied configuration or executable versions. Scheduled runs never fetch latest
ccusage; it changes only with a tested, pinned helper release.

## Status, recovery, updates and removal

Linux/macOS commands (Windows: replace the executable with
`$HOME\.codex-broker-usage-native\codex-broker-usage.exe`):

```sh
~/.codex-broker-usage-native/codex-broker-usage status
~/.codex-broker-usage-native/codex-broker-usage sync
# Only to accept an intentional reduction/correction:
~/.codex-broker-usage-native/codex-broker-usage sync --allow-decrease
```

There is no two-day cutoff or last-upload-date filter. All retained days are
eligible, and unchanged snapshots are skipped. File locks prevent concurrent
manual/scheduled syncs. Revisions and reports are saved before network calls;
acknowledgements are saved after Broker commits. A lost response or interrupted
process leaves data for safe retry. Malformed collector output does not replace
the saved journal. Broker validates sizes, dates, numerical ranges, credentials,
and supported protocol/version; it trusts ccusage's accounting decisions.

To update, download a newer bundle and run its `install` command with the same
`--state-dir` if customized. Existing enrollment, device token, and journal are
reused. Both executables are copied from the bundle. If you downloaded only the
helper, it can fetch its exact pinned native package from npm using HTTPS and
verify its SHA-512 hash. Complete bundles work without npm connectivity.

For multiple identities on one computer, use a separate `--state-dir` on every
command for each installation. Scanning the same logs in both identities counts
those logs independently. For an additional identity, this is intentional;
for accidental duplicate installations within one identity, revoke the extra
device and correct its data before trusting the combined total.

Remove scheduling with `codex-broker-usage uninstall`. This retains credentials
and queued reports. Revoke the device in Settings to stop uploads. Deleting an
identity permanently removes its devices and identity reports. Ordinary
account/proxy history remains independent.

### Upgrading from the earlier Python helper

Run the old helper's `uninstall` command, then revoke its device in Settings.
Install the native helper as a new device and import retained logs. Old custom
event tables remain in the database for recovery/export, but are excluded from
new identity totals so that the ccusage import does not add to old event counts.
Existing identity codes and enrolled credentials are preserved by the migration;
obsolete protocol-1 uploads receive a clear upgrade error. Python state cannot
be automatically converted to a complete multi-agent ccusage report. Retain
old state until you have verified the new totals; deleted source logs must be
restored before ccusage can import them.

## Maintainers

Build all six bundles (Go is required only on the build machine):

```sh
go run ./cmd/package-usage-sync
```

The output is `dist/usage-sync/*.zip` and `SHA256SUMS`. Verify download checksums
against the published `SHA256SUMS` using your OS's hash tool. CI uploads these
as an artifact; version-tag builds attach them to GitHub Releases after tests
pass. Native client unit tests also run on Linux, macOS, and Windows CI hosts. Do not commit generated binaries. Update `usagereport.Version` and all
six integrity hashes together, test the real collector against fixtures, and
review JSON/accounting changes before upgrading the pin.
