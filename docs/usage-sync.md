# Device usage sync

## One-time setup

1. Create an identity in **Settings → Usage identities**. Copy its generated
   `cbi_…` enrollment code; it is shown only once. Use that same code on every
   device that should contribute to the identity. Names cannot authorize uploads.
2. Install Node.js 22 or newer, then run on Linux, macOS, or Windows:

   ```sh
   npx codex-broker@latest install --broker-url https://YOUR-BROKER
   ```

   Alternatively, use `pnpm dlx codex-broker@latest install --broker-url https://YOUR-BROKER`.
3. Enter the generated identity code at the hidden prompt. Never put the code
   in a URL or command argument.

npm verifies the package integrity and selects the included native helper for
Linux, macOS, or Windows on x64 or ARM64. The helper downloads its pinned ccusage
executable from npm and verifies the pinned SHA-512 hash before installation.
Node is needed only for setup; scheduled syncs run the installed native binaries,
not files in the npm cache. No Go, Python, or preinstalled ccusage is needed.
Unsupported OS/CPU combinations are rejected. Native execution still depends on
your OS version and security policy.

For devices without Node, CI's **usage-sync-bundles** artifact contains the six
native zip bundles. Extract both executables, then run `./codex-broker-usage
install --broker-url https://YOUR-BROKER` (Windows: `.\codex-broker-usage.exe`).
Complete bundles can install without accessing npm. Builds do not require a
GitHub Release or tag.

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

To update, run `npx codex-broker@latest install` (or `pnpm dlx
codex-broker@latest install`). Include the same `--state-dir` if customized.
Existing enrollment, device token, private CA, collector configuration, and
queued reports are reused. No identity code is requested for an existing install.
The collector is replaced when its pinned version changes; its replacement is
validated before overwriting the old executable. If a sync is running, wait for
it to finish and rerun installation. Updates are explicit, not automatic.
If a download fails, rerun the command when connectivity returns. An initial
sync can fail offline even after successful installation; scheduled retries remain.

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
as an artifact. Native client unit tests also run on Linux, macOS, and Windows CI hosts. Do not commit generated binaries. Update `usagereport.Version` and all
six integrity hashes together, test the real collector against fixtures, and
review JSON/accounting changes before upgrading the pin.

### npm publishing (no GitHub Releases)

See [npm setup](npm-publishing.md) for the one-time account setup. Every successful
main-branch CI run publishes `codex-broker` with version
`0.<GitHub run number>.<run attempt>`. Branches and PRs build a downloadable npm
tarball without publishing. A manual CI run on main also publishes. No version
commit, release, or tag is required. CI tests native upgrades on all three OSes.
