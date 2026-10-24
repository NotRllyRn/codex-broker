#!/bin/sh
set -eu
command -v python3 >/dev/null || { echo "Python 3.9+ is required" >&2; exit 1; }
root=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
exec python3 "$root/usage-sync.py" install "$@"
