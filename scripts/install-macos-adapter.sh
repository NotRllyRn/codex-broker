#!/bin/sh
set -eu

[ "$(uname -s)" = Darwin ] || { echo "macOS is required" >&2; exit 1; }
[ "$#" -ge 1 ] && [ "$#" -le 2 ] || { echo "usage: $0 <broker-https-origin> [broker-ca.pem]" >&2; exit 2; }
for command in go security launchctl curl; do command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 1; }; done

broker_url=$1
ca_source=${2:-}
case "$broker_url" in https://*) ;; *) echo "broker URL must use HTTPS" >&2; exit 2;; esac
[ -z "$ca_source" ] || [ -f "$ca_source" ] || { echo "broker CA certificate not found" >&2; exit 2; }

key=${CODEX_BROKER_CLIENT_KEY:-}
if [ -z "$key" ]; then
    key=$(security find-generic-password -a "$USER" -s dev.codex-broker.adapter -w 2>/dev/null || true)
fi
if [ -z "$key" ]; then
    printf "Codex Broker client key: " >&2
    trap 'stty echo 2>/dev/null || true; printf "\n" >&2' EXIT HUP INT TERM
    stty -echo
    IFS= read -r key
    stty echo
    printf "\n" >&2
    trap - EXIT HUP INT TERM
fi
case "$key" in cbk_*) ;; *) echo "client key must use cbk_ format" >&2; exit 2;; esac

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
support="$HOME/Library/Application Support/Codex Broker"
bin="$support/bin"
logs="$support/logs"
plist="$HOME/Library/LaunchAgents/dev.codex-broker.adapter.plist"
service=dev.codex-broker.adapter
domain="gui/$(id -u)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM

mkdir -p "$bin" "$logs" "$(dirname "$plist")"
chmod 700 "$support" "$bin" "$logs"
(cd "$root" && go build -trimpath -o "$work/codex-broker-adapter" ./cmd/codex-broker-adapter)
install -m 0755 "$work/codex-broker-adapter" "$bin/codex-broker-adapter"

ca_path=
if [ -n "$ca_source" ]; then
    ca_path="$support/broker-ca.pem"
    install -m 0644 "$ca_source" "$ca_path"
fi

security add-generic-password -U -a "$USER" -s "$service" -w "$key" >/dev/null
unset key

xml() { printf %s "$1" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g'; }
arguments="        <string>$(xml "$bin/codex-broker-adapter")</string>
        <string>--broker-url</string>
        <string>$(xml "$broker_url")</string>
        <string>--keychain-account</string>
        <string>$(xml "$USER")</string>"
if [ -n "$ca_path" ]; then
    arguments="$arguments
        <string>--broker-ca</string>
        <string>$(xml "$ca_path")</string>"
fi

launchctl bootout "$domain/$service" >/dev/null 2>&1 || true
cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$service</string>
    <key>ProgramArguments</key>
    <array>
$arguments
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$(xml "$logs/adapter.log")</string>
    <key>StandardErrorPath</key>
    <string>$(xml "$logs/adapter.log")</string>
</dict>
</plist>
EOF
chmod 600 "$plist"
launchctl bootstrap "$domain" "$plist"

for attempt in 1 2 3 4 5 6 7 8 9 10; do
    if curl --silent --fail --output /dev/null http://127.0.0.1:8789/health; then
        echo "Codex Broker adapter installed and connected."
        echo "Restart the ChatGPT app after adding the provider configuration from docs/integrations/chatgpt-macos.md."
        exit 0
    fi
    sleep 1
done

echo "adapter installed but its broker health check failed; inspect $logs/adapter.log" >&2
exit 1
