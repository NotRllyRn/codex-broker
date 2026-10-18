#!/bin/sh
set -eu

install -d -o 10001 -g 10001 -m 0700 /run/windowkeeper/certs
for name in broker-ca.crt relay.crt relay.key; do
  cp "/certs/$name" "/run/windowkeeper/certs/$name"
  chmod 0600 "/run/windowkeeper/certs/$name"
  chown 10001:10001 "/run/windowkeeper/certs/$name"
done

export WINDOWKEEPER_RELAY_CA_CERT=/run/windowkeeper/certs/broker-ca.crt
export WINDOWKEEPER_RELAY_TLS_CERT_FILE=/run/windowkeeper/certs/relay.crt
export WINDOWKEEPER_RELAY_TLS_KEY_FILE=/run/windowkeeper/certs/relay.key
exec codex-broker "$@"
