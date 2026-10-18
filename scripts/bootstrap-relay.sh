#!/bin/sh
set -eu

relay_ip=${1:-}
broker_url=${2:-}
relay_id=${3:-relay-1}
[ -n "$relay_ip" ] && [ -n "$broker_url" ] || { echo "usage: $0 <relay-ip> <broker-https-origin> [relay-id]" >&2; exit 2; }
case "$relay_ip" in *[!0-9A-Fa-f:.]*|'') echo "relay-ip must be an IP address" >&2; exit 2;; esac
case "$broker_url" in https://*) ;; *) echo "broker origin must use https" >&2; exit 2;; esac
case "$relay_id" in *[!A-Za-z0-9._-]*|'') echo "relay-id may contain letters, numbers, dot, underscore, and hyphen" >&2; exit 2;; esac
[ "${#relay_id}" -le 80 ] || { echo "relay-id must not exceed 80 characters" >&2; exit 2; }
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 1; }
[ -f .env ] || { echo "run scripts/bootstrap.sh on the broker first" >&2; exit 1; }
[ -f deployment/certs/ca.crt ] && [ -f deployment/certs/ca.key ] || { echo "broker CA files are missing" >&2; exit 1; }
[ ! -e deployment/relay ] || { echo "deployment/relay already exists; refusing to overwrite it" >&2; exit 1; }

relay_key=$(sed -n 's/^WINDOWKEEPER_RELAY_KEY=//p' .env | tail -n 1)
if [ -z "$relay_key" ]; then
  relay_key="cbr_$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')"
  printf '\nWINDOWKEEPER_RELAY_KEY=%s\n' "$relay_key" >>.env
fi
case "$relay_key" in cbr_????????????????????????????????*) ;; *) echo "WINDOWKEEPER_RELAY_KEY is invalid" >&2; exit 1;; esac

umask 077
mkdir -p deployment/relay
cp deployment/certs/ca.crt deployment/relay/ca.crt
cat >deployment/relay/relay.ext <<EOF
subjectAltName=IP:$relay_ip,DNS:codex-broker
extendedKeyUsage=serverAuth
EOF
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out deployment/relay/relay.key 2>/dev/null
openssl req -new -key deployment/relay/relay.key -subj '/CN=codex-broker' -out deployment/relay/relay.csr
openssl x509 -req -in deployment/relay/relay.csr -CA deployment/certs/ca.crt -CAkey deployment/certs/ca.key -CAcreateserial -days 825 -sha256 -extfile deployment/relay/relay.ext -out deployment/relay/relay.crt 2>/dev/null
cat >deployment/relay/.env <<EOF
WINDOWKEEPER_RELAY_ID=$relay_id
WINDOWKEEPER_RELAY_KEY=$relay_key
WINDOWKEEPER_RELAY_BROKER_URL=${broker_url%/}
WINDOWKEEPER_RELAY_SYNC_SECONDS=15
WINDOWKEEPER_RELAY_MAX_STALE_SECONDS=3600
EOF
rm deployment/relay/relay.csr deployment/relay/relay.ext deployment/certs/ca.srl
chmod 600 deployment/relay/.env deployment/relay/relay.key

echo "Relay bundle created in deployment/relay."
echo "Restart the primary broker once so it loads the relay key."
echo "Copy the repository plus deployment/relay to $relay_ip, then run:"
echo "docker compose -f compose.relay.yaml up --build -d"
