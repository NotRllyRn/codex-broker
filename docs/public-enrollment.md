# Public account enrollment

Codex Broker runs a deliberately isolated public sign-in site on a second HTTPS port. It starts a ChatGPT device-code flow, displays the one-time code and full OpenAI URL, and automatically adds the authenticated account under its email address.

The public process starts with the default Compose stack. Administrators enable or disable its page from **Settings → Public enrollment**. When disabled, every public route except process liveness returns an empty `404`; no enrollment can start.

## Security boundary

The public process exposes only:

- `GET /` or `GET /{label}/` — enrollment instructions;
- `POST /start` — start one enrollment;
- `GET /status` — poll the caller's opaque enrollment cookie;
- `GET /health/live` — process liveness.

It calls narrowly scoped private broker endpoints over authenticated HTTPS. `WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY` is an internal service-to-service secret: visitors never enter or receive it, and it cannot authenticate to `/api/v1/route` or administrator routes. Enrollment secrets are held only in an `HttpOnly`, `Secure`, `SameSite=Strict` cookie and are never placed in a URL. Responses use a restrictive CSP, HSTS, `no-store`, and no referrer. Starts are limited per source address and globally capped; active flows expire with the normal login timeout.

A successful login verifies the identity returned by Codex, rejects a duplicate managed/pending email, renames the account to the normalized email, and promotes the original credential through the existing encrypted checkpoint path. It does not fork or proactively rotate tokens, or create downloadable auth files. Failed or interrupted placeholder accounts are soft-deleted.

This feature intentionally grants the broker ongoing access to the visitor's ChatGPT account. The page says so explicitly. Operate it only for users who understand and authorize that access.

## Production setup

The public browser endpoint needs a publicly trusted certificate for its public DNS name. Do not serve the local broker CA certificate to Internet browsers, and do not copy the broker or CA private key into the public container.

The normal bootstrap creates the internal key. Existing `.env` files can add it with:

```bash
printf 'WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY=%s\n' "$(openssl rand -hex 32)" >> .env
chmod 600 .env
```

Configure the public listener as needed:

```dotenv
CODEX_BROKER_PUBLIC_PORT=8788
CODEX_BROKER_PUBLIC_TLS_CERT_FILE=/absolute/path/to/public/fullchain.pem
CODEX_BROKER_PUBLIC_TLS_KEY_FILE=/absolute/path/to/public/privkey.pem
```

Start both processes with the normal command:

```bash
docker compose up --build -d
```

Expose only public port `8788` to the Internet. Keep broker port `8787` restricted to the LAN/firewall. The public process connects to the broker by its private Compose network name and mounts only:

- the public TLS certificate and key;
- the broker CA certificate at `/certs/broker-ca.crt`.

The public process runs read-only with all Linux capabilities dropped and no persistent data mount. Its certificate and key must be readable by container UID/GID `10001:10001` (or that identity's host-remapped UID/GID), while the private key remains restricted. Copy ACME renewal output to a deployment path with suitable ownership rather than weakening `/etc/letsencrypt` permissions, then recreate the service after renewal.

New bootstraps create a separate local `public.crt`/`public.key` so the process can start without sharing the broker's private key. Internet browsers will not trust that local certificate and its names may not match the public hostname. Configure a publicly trusted hostname certificate before exposing port `8788`. Existing deployments must either set the two public certificate paths above or create `deployment/certs/public.crt` and `deployment/certs/public.key`.

If a reverse proxy or CDN fronts the public port, terminate or re-encrypt TLS safely and apply its rate limiting there. The application deliberately does not trust forwarded client-IP headers; without proxy-side limiting, all proxied visitors share one application throttle bucket.

## Enable or disable

Sign in to the private administrator dashboard, open **Settings**, and use the **Public enrollment** control. The state is persisted in SQLite and takes effect immediately without restarting either process.

Disabling does not stop the isolated container because its liveness endpoint remains available for supervision. It makes all visitor routes return an empty `404` and causes the broker to reject new starts. In-progress login workers may finish safely, but no new public flow can begin.

## Enrollment labels

Create labels in **Settings → Labels** before sharing tagged signup links:

- `https://codex.example.com/cindy/` assigns the existing `cindy` label.
- `https://codex.example.com/?labels=cindy,team` assigns both existing labels.
- `https://codex.example.com/cindy/?labels=team` combines the path and query labels.
- Repeated query parameters (`?labels=cindy&labels=team`) also work. URL-encode spaces and other special characters in label names; use the query form for names containing `/` or reserved routes such as `status`.

Matching ignores case, trims whitespace, and deduplicates names. Unknown labels are ignored and never created by enrollment; signup still works without a matching label. Links support up to 20 distinct names of at most 40 characters each. An existing enrollment cookie resumes its original signup; visiting another link does not retag that enrollment.

Labels created in Settings or through administrator account edits share the existing label table. Unused labels persist until explicitly deleted. Deleting a label removes it from every account, including pending enrollments. Labels referenced by client key access rules must be removed from those rules before deletion; this prevents deletion from silently changing access permissions.

Implementation uses the existing public start endpoint and account creation transaction: the page passes link labels to the public process, the private broker attaches only matching label rows, and the existing foreign key cascade removes account assignments on deletion. No new tables, migration, or public label-list endpoint are needed.

## Configuration

By default, the broker runs up to eight authentication workers and accepts up to
16 active public enrollments. Attempts beyond the available workers wait in a
queue; once all 16 enrollment slots are occupied, new starts are rejected until
capacity becomes available. Authentication workers remain occupied while users
complete OpenAI authorization, and are shared with administrator sign-ins.

| Variable | Default | Purpose |
| --- | --- | --- |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY` | generated by bootstrap | Internal public-process-to-broker key, at least 32 characters; never entered by visitors. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_BROKER_URL` | `https://codex-broker:8787` | Private HTTPS broker URL used by the public process. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_CA_CERT` | unset | Broker CA file used by the public process. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_HOST` | `127.0.0.1` | Public listener bind address. Compose sets `0.0.0.0`. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_PORT` | `8788` | Public listener container port. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_CERT_FILE` | unset | Public listener certificate. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_KEY_FILE` | unset | Public listener private key. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_ATTEMPTS_PER_HOUR` | `20` | Per-source start limit. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_MAX_ACTIVE` | `16` | Global concurrent enrollment cap, including queued attempts. |
| `WINDOWKEEPER_AUTH_CONCURRENCY` | `8` | Concurrent authentication workers shared by public enrollment and administrator sign-ins. |

For a non-Compose installation, configure the internal key, broker CA, and TLS files, then run:

```bash
codex-broker public-serve
```

Non-loopback binds require the public TLS certificate/key.
