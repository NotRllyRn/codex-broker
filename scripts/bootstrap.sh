#!/bin/sh
set -eu

ip=${1:-}
[ -n "$ip" ] || { echo "usage: $0 <broker-local-ip>" >&2; exit 2; }
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 1; }
[ ! -e .env ] || { echo ".env already exists; refusing to overwrite secrets" >&2; exit 1; }
[ ! -e deployment/certs ] || { echo "deployment/certs already exists; refusing to overwrite certificates" >&2; exit 1; }

umask 077
mkdir -p deployment/certs
certs=deployment/certs
admin_password=$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')
vault_key=$(python3 -c 'import base64, os; print(base64.urlsafe_b64encode(os.urandom(32)).decode())')
cat >.env <<EOF
WINDOWKEEPER_ADMIN_PASSWORD=$admin_password
WINDOWKEEPER_VAULT_KEY=$vault_key
CODEX_BROKER_BIND_ADDRESS=0.0.0.0
WINDOWKEEPER_BROWSER_OAUTH_MODE=manual
EOF

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$certs/ca.key" 2>/dev/null
openssl req -x509 -new -key "$certs/ca.key" -sha256 -days 3650 -subj '/CN=Codex Broker Local CA' -out "$certs/ca.crt"

cat >"$certs/server.ext" <<EOF
subjectAltName=IP:$ip,DNS:codex-broker
extendedKeyUsage=serverAuth
EOF
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$certs/server.key" 2>/dev/null
openssl req -new -key "$certs/server.key" -subj '/CN=codex-broker' -out "$certs/server.csr"
openssl x509 -req -in "$certs/server.csr" -CA "$certs/ca.crt" -CAkey "$certs/ca.key" -CAcreateserial -days 825 -sha256 -extfile "$certs/server.ext" -out "$certs/server.crt" 2>/dev/null

rm "$certs"/*.csr "$certs"/*.ext "$certs"/*.srl
chmod 600 .env "$certs"/*.key

echo "Bootstrap complete."
echo "Broker URL: https://$ip:8787"
echo "Admin password: $admin_password"
echo "Install $certs/ca.crt on trusted client hosts."
echo "Next: docker compose up --build -d"
