# Public account enrollment

Codex Broker can run a deliberately isolated public sign-in site on a second HTTPS port. It starts a ChatGPT device-code flow, displays the one-time code and full OpenAI URL, and automatically adds the authenticated account under its email address.

This listener is disabled by default. It is a separate FastAPI process and does not mount the administrator dashboard, account API, routing API, vault, database, or runtime directories.

## Security boundary

The public process exposes only:

- `GET /` — static enrollment instructions;
- `POST /start` — start one enrollment;
- `GET /status` — poll the caller's opaque enrollment cookie;
- `GET /health/live` — process liveness.

It calls two private broker endpoints over authenticated HTTPS. Its dedicated shared key cannot authenticate to `/api/v1/route` or administrator routes. Enrollment secrets are held only in an `HttpOnly`, `Secure`, `SameSite=Strict` cookie and are never placed in a URL. Responses use a restrictive CSP, HSTS, `no-store`, and no referrer. Starts are limited per source address and globally capped; active flows expire with the normal login timeout.

A successful login verifies the identity returned by Codex, rejects a duplicate managed/pending email, renames the account to the normalized email, and promotes the credential through the existing encrypted checkpoint path. Failed or interrupted placeholder accounts are soft-deleted.

This feature intentionally grants the broker ongoing access to the visitor's ChatGPT account. The page says so explicitly. Operate it only for users who understand and authorize that access.

## Production setup

The public browser endpoint needs a publicly trusted certificate for its public DNS name. Do not serve the local broker CA certificate to Internet browsers, and do not copy the broker or CA private key into the public container.

Set these protected `.env` values:

```dotenv
WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY=<openssl-rand-hex-32>
CODEX_BROKER_PUBLIC_PORT=8788
CODEX_BROKER_PUBLIC_TLS_CERT_FILE=/absolute/path/to/public/fullchain.pem
CODEX_BROKER_PUBLIC_TLS_KEY_FILE=/absolute/path/to/public/privkey.pem
```

Generate a key for an existing deployment with:

```bash
printf 'WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY=%s\n' "$(openssl rand -hex 32)" >> .env
chmod 600 .env
```

Start the private broker and isolated public listener:

```bash
docker compose -f compose.yaml -f compose.public.yaml up --build -d
```

Expose only the public port (default `8788`) to the Internet. Keep broker port `8787` restricted to the LAN/firewall. The override connects the public process to the broker by its private Compose network name and mounts only:

- the public TLS certificate and key;
- the broker CA certificate at `/certs/broker-ca.crt`.

The public process runs read-only with all Linux capabilities dropped and no persistent data mount.

If a reverse proxy or CDN fronts the public port, terminate or re-encrypt TLS safely and apply its rate limiting there. The application deliberately does not trust forwarded client-IP headers; without proxy-side limiting, all proxied visitors share one application throttle bucket.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_ENABLED` | `false` | Enables the private broker enrollment API. The Compose override sets it to `true`. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY` | unset | Dedicated random shared key, at least 32 characters. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_BROKER_URL` | `https://codex-broker:8787` | Private HTTPS broker URL used by the public process. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_CA_CERT` | unset | Broker CA file used by the public process. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_HOST` | `127.0.0.1` | Public listener bind address. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_PORT` | `8788` | Public listener container port. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_CERT_FILE` | unset | Public listener certificate. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_KEY_FILE` | unset | Public listener private key. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_ATTEMPTS_PER_HOUR` | `3` | Per-source start limit. |
| `WINDOWKEEPER_PUBLIC_ENROLLMENT_MAX_ACTIVE` | `4` | Global concurrent enrollment cap. |

For a non-Compose installation, run the second process with:

```bash
codex-broker public-serve
```

It refuses to start without enrollment enabled, the dedicated key, and broker CA. Non-loopback binds also require the public TLS certificate/key.
