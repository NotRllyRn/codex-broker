# Relay Edge

Relay Edge is the backup Codex gateway. It runs the same proxy engine as the
primary, including every gateway route, streaming, header stripping, account
notices, request limits, Responses payload scrubbing, and response telemetry.
It connects directly to OpenAI; it never proxies inference through the primary.

## Bootstrap

On the primary host, from the repository checkout:

```bash
scripts/bootstrap-relay.sh <relay-ip> https://<primary-ip>:8787 [relay-id]
docker compose up -d --build broker
```

Use the primary broker's direct address for `<primary-ip>`, never the shared
VIP, Nginx Proxy Manager hostname, or HAProxy frontend. This keeps relay
control synchronization from looping back to the relay during an outage.

The script creates a dedicated `cbr_` relay key when one is not already in the
primary `.env`, and writes a self-contained `deployment/relay/` bundle:

- `.env`: relay ID, relay key, broker URL, and sync limits;
- `ca.crt`: primary identity verification;
- `relay.crt` and `relay.key`: relay HTTPS identity.

Clone the same repository revision on the relay host and copy only that
directory from the primary. Never copy the primary `.env`, SQLite database,
vault key, admin password, public-enrollment key, `auth.json`, refresh tokens,
or `deployment/certs/ca.key`.

```bash
# Primary host
ssh root@<relay-ip> 'mkdir -p /opt/stacks/codex-broker/deployment'
scp -r deployment/relay root@<relay-ip>:/opt/stacks/codex-broker/deployment/

# Relay host (after cloning/updating /opt/stacks/codex-broker)
cd /opt/stacks/codex-broker
git checkout <same-commit-as-primary>
```

On the relay host:

```bash
docker compose -f compose.relay.yaml up --build -d
docker compose -f compose.relay.yaml ps
curl --cacert deployment/relay/ca.crt https://<relay-ip>:8787/health/ready
```

`/health/live` means the process is running. `/health/ready` returns 200 only
when the encrypted cache is protocol-compatible, recent, and contains a usable
unexpired access token.

## Load balancer

Configure the primary as the normal server and the relay as `backup`. Health
check `/health/ready`; disable response buffering and allow 80 MiB request
bodies and long-lived streams. Do not configure retries that replay a POST
after it may have reached a backend. Existing streams cannot migrate; the next
request uses the surviving gateway.

For HAProxy, use the primary and relay as verified-TLS backends:

```haproxy
backend codex_brokers
    option httpchk
    http-check send meth GET uri /health/ready ver HTTP/1.1 hdr Host codex-broker
    http-check expect status 200
    server primary <primary-ip>:8787 ssl check verify required verifyhost codex-broker ca-file /path/to/ca.crt
    server relay   <relay-ip>:8787 ssl check verify required verifyhost codex-broker ca-file /path/to/ca.crt backup
```

## Failure behavior

- Online relay requests ask the primary for the authoritative route.
- Offline requests use the encrypted snapshot and per-client local exclusions.
- The assigned held reserve activates only after cached in-rotation accounts
  are unavailable.
- Activation, upstream failure, and terminal response telemetry events are
  durably queued and idempotently reconciled after recovery.
- A revoked client key remains usable only until the snapshot staleness
  deadline; access-token expiry remains a hard limit.
- Refresh tokens remain exclusively on the primary, so an outage cannot be
  extended past the cached access-token lifetime.

To rotate authority, set a new `WINDOWKEEPER_RELAY_KEY` on the primary, replace
the same value in `deployment/relay/.env`, and restart the primary. Then discard
the cache encrypted by the old key and recreate the relay:

```bash
docker compose -f compose.relay.yaml down -v
docker compose -f compose.relay.yaml up --build -d
```
