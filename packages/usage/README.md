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
