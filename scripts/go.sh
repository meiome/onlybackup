#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
export GOCACHE="${GOCACHE:-$ROOT/.cache/go-build}"
export GOMODCACHE="${GOMODCACHE:-$ROOT/.cache/mod}"
if [ -x "$ROOT/.tools/go/bin/go" ]; then
    exec "$ROOT/.tools/go/bin/go" "$@"
fi
exec go "$@"
