# Codex Broker device usage sync

With Node.js 22 or newer:

```sh
npx codex-broker@latest install --broker-url https://YOUR-BROKER
```

Or use `pnpm dlx codex-broker@latest install --broker-url https://YOUR-BROKER`.
Enter your generated identity code at the hidden prompt. Add `--ca /path/to/ca.pem`
for a private CA. Linux, macOS and Windows on x64/ARM64 are supported.

Setup downloads a pinned, integrity-checked native ccusage collector. Background
syncing runs independently of Node and npm. Only numerical usage reports are
uploaded, not conversations.

Upgrade with `npx codex-broker@latest install`. Existing credentials and queued
reports are retained. Include the same `--state-dir` if customized. Use `status`,
`sync`, or `uninstall` instead of `install` to manage an installation; uninstall
removes scheduling and retains state. Updates are explicit, not automatic.

[Full setup and troubleshooting](https://github.com/NotRllyRn/codex-broker/blob/main/docs/usage-sync.md).

If setup saved the wrong Broker URL, see [connection recovery](https://github.com/NotRllyRn/codex-broker/blob/main/docs/usage-sync.md#wrong-broker-url-or-failed-first-sync).
Version `0.91.1` requires editing the saved `config.json`; reinstalling that version
ignores a new `--broker-url`. Version `0.92.1` and later honor
explicit `--broker-url` and `--ca` overrides without resetting device history.
Installation can remain in place after a failed first sync so offline retries work.

Linux setup automatically prefers user systemd and falls back to a running cron
service when no user service manager is available (including root server sessions).
Existing cron jobs are preserved. If neither scheduler is running, start cron and
rerun setup, or use `--no-schedule` with an external scheduler. Container state must
be persisted across recreation. macOS uses launchd and Windows uses Task Scheduler.
