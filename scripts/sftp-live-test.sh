#!/bin/sh
# Live backup test against a real SFTP destination -- a Hetzner Storage Box
# by default -- using credentials from .env at the repo root. Everything
# the test writes goes under a fresh, per-run directory that the test
# itself removes when it's done, pass or fail; nothing else on the box is
# touched, and nothing here is printed.
#
# .env must define:
#   HETZNER_LINK      the storage box host, e.g. u123456.your-storagebox.de
#   HETZNER_USER      the account, e.g. u123456
#   HETZNER_SSH_PORT  usually 23 for a Storage Box (defaults to 22)
#   HETZNER_KEY       absolute path to a private key already authorized on
#                      the box (a leading ~/ is expanded); or set
#                      HETZNER_PASSWORD instead
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild
[ -f .env ] || { echo ".env not found; see the HETZNER_* names above" >&2; exit 1; }
. ./.env

: "${HETZNER_LINK:?set in .env}" "${HETZNER_USER:?set in .env}"
port="${HETZNER_SSH_PORT:-22}"
key="${HETZNER_KEY:-}"
case "$key" in
"~/"*) key="$HOME/${key#\~/}" ;;
esac
if [ -n "$key" ] && [ "${key#/}" = "$key" ]; then
  echo "HETZNER_KEY must be an absolute path (Kopia requires it); got: $key" >&2
  exit 1
fi
if [ -z "$key" ] && [ -z "${HETZNER_PASSWORD:-}" ]; then
  echo "set HETZNER_KEY (absolute path) or HETZNER_PASSWORD in .env" >&2
  exit 1
fi
path="vb-live-test-$$-$(date +%s)"

docker run --rm -i \
  --user "$(id -u):$(id -g)" \
  -v "$PWD:/src" -w /src \
  -v "$PWD/.cache/gopath:/gopath" \
  -v "$PWD/.cache/gobuild:/tmp/gobuild" \
  ${key:+-v "$key:$key:ro"} \
  -e GOPATH=/gopath -e GOCACHE=/tmp/gobuild -e GOFLAGS=-buildvcs=false -e HOME=/tmp \
  -e VB_LIVE_SFTP_HOST="$HETZNER_LINK" -e VB_LIVE_SFTP_PORT="$port" \
  -e VB_LIVE_SFTP_USER="$HETZNER_USER" -e VB_LIVE_SFTP_KEY_FILE="$key" \
  -e VB_LIVE_SFTP_PASSWORD="${HETZNER_PASSWORD:-}" -e VB_LIVE_SFTP_PATH="$path" \
  golang:1.27 go test -count=1 -tags sftplive -run TestLiveSFTPBackup -v ./internal/engine/
