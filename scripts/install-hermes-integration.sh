#!/bin/sh
set -eu

fork_url=https://github.com/NotRllyRn/hermes-agent-codex-broker.git
fork_ref=codex-broker/v0.21.0-r2
pinned_commit=4c13968c344ac063369921425ef536964a9b1fbb
upstream_base=b0ab2e163a50d4e6c36507eba955a6067fde6abc
target=${HERMES_AGENT_DIR:-/home/hermes/.hermes/hermes-agent}
service=${HERMES_GATEWAY_SERVICE:-hermes-gateway.service}
url=${HERMES_CODEX_BROKER_URL:-${CODEX_BROKER_URL:-}}
key=${HERMES_CODEX_BROKER_CLIENT_KEY:-${CODEX_BROKER_CLIENT_KEY:-}}
ca_source=${HERMES_CODEX_BROKER_CA_CERT:-${CODEX_BROKER_CA_CERT:-}}

[ "$(id -u)" -eq 0 ] || {
    echo "run as root" >&2
    exit 1
}
[ -d "$target/.git" ] || {
    echo "Hermes git installation not found at $target" >&2
    exit 1
}
[ -n "$url" ] && [ -n "$key" ] || {
    echo "export CODEX_BROKER_URL and CODEX_BROKER_CLIENT_KEY" >&2
    exit 1
}
case "$url" in https://*) ;; *)
    echo "Codex Broker URL must use HTTPS" >&2
    exit 1
    ;;
esac
[ -z "$ca_source" ] || [ -f "$ca_source" ] || {
    echo "Codex Broker CA certificate not found" >&2
    exit 1
}

owner=$(stat -c %U "$target")
group=$(id -gn "$owner")
home=$(getent passwd "$owner" | cut -d: -f6)
env_file=$home/.hermes/.env
cert_dir=$home/.hermes/certs
cert_file=$cert_dir/codex-broker-ca.crt
python=$target/venv/bin/python
[ -x "$python" ] || {
    echo "Hermes virtual environment not found at $python" >&2
    exit 1
}
[ -z "$(runuser -u "$owner" -- git -C "$target" status --porcelain)" ] || {
    echo "Hermes checkout has uncommitted changes" >&2
    exit 1
}

work=$(mktemp -d)
chmod 700 "$work"
old_head=$(runuser -u "$owner" -- git -C "$target" rev-parse HEAD)
old_branch=$(runuser -u "$owner" -- git -C "$target" symbolic-ref --quiet --short HEAD || true)
env_existed=0
cert_existed=0
changed=0
success=0
[ ! -f "$env_file" ] || {
    cp -p "$env_file" "$work/env"
    env_existed=1
}
[ ! -f "$cert_file" ] || {
    cp -p "$cert_file" "$work/ca.crt"
    cert_existed=1
}

rollback() {
    status=$?
    if [ "$success" -ne 1 ] && [ "$changed" -eq 1 ]; then
        echo "installation failed; rolling back" >&2
        if [ "$env_existed" -eq 1 ]; then cp -p "$work/env" "$env_file"; else rm -f "$env_file"; fi
        if [ "$cert_existed" -eq 1 ]; then cp -p "$work/ca.crt" "$cert_file"; else rm -f "$cert_file"; fi
        if [ -n "$old_branch" ]; then
            runuser -u "$owner" -- git -C "$target" switch -C "$old_branch" "$old_head" >/dev/null 2>&1 || true
        else
            runuser -u "$owner" -- git -C "$target" switch --detach "$old_head" >/dev/null 2>&1 || true
        fi
        systemctl restart "$service" >/dev/null 2>&1 || true
    fi
    rm -rf "$work"
    exit "$status"
}
trap rollback EXIT
trap 'exit 1' HUP INT TERM

runuser -u "$owner" -- git -C "$target" fetch --no-tags "$fork_url" "$fork_ref"
[ "$(runuser -u "$owner" -- git -C "$target" rev-parse FETCH_HEAD)" = "$pinned_commit" ] || {
    echo "fork branch no longer matches the reviewed pin" >&2
    exit 1
}
[ "$(runuser -u "$owner" -- git -C "$target" rev-parse FETCH_HEAD~3)" = "$upstream_base" ] || {
    echo "fork branch has an unexpected upstream base" >&2
    exit 1
}
runuser -u "$owner" -- git -C "$target" merge-base --is-ancestor "$old_head" "$pinned_commit" || {
    echo "installed Hermes revision is not compatible with the v0.21.0 pin" >&2
    exit 1
}

runuser -u "$owner" -- git -C "$target" switch -C "$fork_ref" "$pinned_commit"
changed=1
install -d -o "$owner" -g "$group" -m 0700 "$cert_dir"
if [ -n "$ca_source" ]; then
    if [ "$(readlink -f "$ca_source")" != "$(readlink -f "$cert_file")" ]; then
        install -o "$owner" -g "$group" -m 0644 "$ca_source" "$cert_file"
    else
        chown "$owner:$group" "$cert_file"
        chmod 644 "$cert_file"
    fi
    ca_value=$cert_file
else
    ca_value=
fi
[ -e "$env_file" ] || install -o "$owner" -g "$group" -m 0600 /dev/null "$env_file"

BROKER_URL=$url BROKER_KEY=$key BROKER_CA=$ca_value ENV_FILE=$env_file \
    runuser -u "$owner" -p -- "$python" - <<'PY'
import os
from pathlib import Path

import httpx

path = Path(os.environ["ENV_FILE"])
updates = {
    "HERMES_CODEX_BROKER_URL": os.environ["BROKER_URL"],
    "HERMES_CODEX_BROKER_CLIENT_KEY": os.environ["BROKER_KEY"],
    "HERMES_CODEX_BROKER_CA_CERT": os.environ["BROKER_CA"],
}
lines = path.read_text(encoding="utf-8").splitlines() if path.exists() else []
lines = [line for line in lines if line.split("=", 1)[0].strip() not in updates]
lines.extend(f"{name}={value}" for name, value in updates.items() if value)
path.write_text("\n".join(lines) + "\n", encoding="utf-8")
response = httpx.get(
    f"{updates['HERMES_CODEX_BROKER_URL'].rstrip('/')}/api/v1/health",
    headers={"Authorization": f"Bearer {updates['HERMES_CODEX_BROKER_CLIENT_KEY']}"},
    verify=updates["HERMES_CODEX_BROKER_CA_CERT"] or True,
    timeout=10,
)
if response.status_code != 200:
    raise RuntimeError("Codex Broker health authentication failed")
PY
chown "$owner:$group" "$env_file"
chmod 600 "$env_file"

systemctl restart "$service"
sleep 8
systemctl is-active --quiet "$service" || {
    echo "Hermes gateway failed to start" >&2
    exit 1
}
success=1
echo "Hermes Codex Broker integration installed: v0.21.0-r2 @ $pinned_commit"
