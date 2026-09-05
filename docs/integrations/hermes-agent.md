# Hermes Agent integration

Codex Broker maintains a dedicated Hermes Agent fork at
[`NotRllyRn/hermes-agent-codex-broker`](https://github.com/NotRllyRn/hermes-agent-codex-broker).
The fork remains linked to `NousResearch/hermes-agent` so upstream synchronization stays explicit.

## Version pin

| Item | Pin |
| --- | --- |
| Hermes release reported in production | `v0.21.0` (`v2026.8.31`) |
| Tested upstream production revision | `b0ab2e163a50d4e6c36507eba955a6067fde6abc` |
| Integration branch | `codex-broker/v0.21.0-r7` |
| Integration tag | `codex-broker-v0.21.0-r7` |
| Tested integration commit | `e37f33b542ee6995cca75f7b96c4958e186e543b` |
| Rolling rebase branch | `codex-broker-next` |

The production Git installer identified itself as Hermes Agent `v0.21.0` while running upstream
revision `b0ab2e16`, which is newer than the official `v2026.8.31` tag. The integration is therefore
pinned to the exact production revision, not merely to the older release tag. The submodule records
the tested integration commit directly.

Initialize the source checkout after cloning this monorepo:

```bash
git submodule update --init integrations/hermes-agent
```

The submodule is for review, testing, and maintenance. Hermes installations fetch the pinned fork
commit directly; they do not need to clone this whole monorepo.

## Install on Hermes

Install Hermes using its normal Git installer first. The integration can be installed without exporting broker settings:

```bash
curl -fsSL https://raw.githubusercontent.com/NotRllyRn/codex-broker/main/scripts/install-hermes-integration.sh | sudo -E sh
```

For higher assurance, download and inspect the script before running it rather than piping it into a
privileged shell.

The installer:

1. requires an existing clean Hermes Git installation (default
   `/home/hermes/.hermes/hermes-agent`);
2. verifies the immutable fork commit and its expected upstream base;
3. refuses an unsupported newer or divergent Hermes checkout instead of silently downgrading it;
4. checks out `codex-broker/v0.21.0-r7` at the exact tested commit;
5. optionally preserves the legacy environment-driven setup path when all broker values are supplied;
6. restarts `hermes-gateway.service`; and
7. restores the previous checkout and configuration if validation or startup fails.

After installation, use a private administrator chat to configure and verify the connection:

```text
/broker-status set https://192.168.1.20:8787 cbk_... /home/hermes/.hermes/certs/codex-broker-ca.crt
```

The CA file must already exist on the Hermes host. Delete the token-bearing command from chat history afterward. `/broker-status` displays the settings with the token redacted and supports changing one value with `url`, `token`, or `ca`. Settings are verified before being saved to `/home/hermes/.hermes/.env` with mode `0600` and take effect without a gateway restart.

This pin adds cycle-safe account failover, an account/usage pre-message before each provider
attempt, and gateway `/broker-status` settings/status. Hermes continues across distinct broker accounts until one
succeeds or the broker waits for the exact pool reset. If quota/auth fails after visible output,
Hermes preserves that partial response and asks the replacement account to continue without
repeating it.

Override `HERMES_AGENT_DIR` or `HERMES_GATEWAY_SERVICE` only for a nonstandard installation. The
broker server certificate and private key must remain on the broker host.

## Maintain across Hermes releases

Published `codex-broker/vX.Y.Z[-rN]` branches and matching tags are immutable production pins. Rebase only `codex-broker-next` while adapting to a new Hermes release:

```bash
cd integrations/hermes-agent
git remote get-url upstream >/dev/null 2>&1 || git remote add upstream https://github.com/NousResearch/hermes-agent.git
git fetch upstream --tags
git switch codex-broker-next
git rebase <new-tested-upstream-revision>
# Resolve seams, run the focused and upstream regression suites, and perform live acceptance.
git push --force-with-lease origin codex-broker-next
```

Once validated, create a new release-specific branch and tag, then update the submodule pointer and
the four constants at the top of `scripts/install-hermes-integration.sh`:

```bash
git switch -c codex-broker/vX.Y.Z-rN
git tag -a codex-broker-vX.Y.Z-rN -m "Codex Broker integration rN for Hermes Agent vX.Y.Z"
git push origin codex-broker/vX.Y.Z-rN codex-broker-vX.Y.Z-rN
```

Never move an existing integration tag. This preserves rollback and lets each production host use a
known Hermes release plus a known broker patch.

The implementation and live acceptance requirements remain documented in
[`hermes-agent-patch.md`](hermes-agent-patch.md).
