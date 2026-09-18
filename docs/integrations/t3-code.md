# T3 Code integration

Codex Broker supports [T3 Code](https://github.com/NotRllyRn/t3code) through the
fork's [`codex-broker` branch](https://github.com/NotRllyRn/t3code/tree/codex-broker).
The integration leases an account for each turn and keeps refresh tokens on the
broker host. T3 Code does not need its own `codex login`.

## Install

The integration is currently built from source. Install Node.js 24 and
[Vite+](https://viteplus.dev/guide/), then run:

```bash
git clone --branch codex-broker https://github.com/NotRllyRn/t3code.git
cd t3code
vp i
vp run build:desktop
node apps/server/dist/bin.mjs
```

Update this installation with `git pull`, then repeat the build command.

## Connect to the broker

Create a key on the broker host:

```bash
codex-broker client-key create "T3 Code"
```

In T3 Code, open **Settings → Providers → Codex** and add these environment
variables to the provider instance:

```text
CODEX_BROKER_URL=https://192.168.1.20:8787
CODEX_BROKER_CLIENT_KEY=cbk_...
CODEX_BROKER_CA_CERT=/path/to/codex-broker-ca.crt
```

Mark `CODEX_BROKER_CLIENT_KEY` as **Sensitive**. The URL must use HTTPS. Omit
`CODEX_BROKER_CA_CERT` when the broker certificate is already trusted by the
operating system; otherwise copy only the CA certificate to the T3 Code host.
Never copy the broker's private key.

Save the provider and refresh its status. It should report **Codex Broker** as
the authenticated account. Start a Codex thread to verify routing.

Broker mode applies to interactive threads, provider checks, and generated
titles, branches, commits, and pull-request text. Leased access tokens stay in
server memory and are not written to `auth.json`.
