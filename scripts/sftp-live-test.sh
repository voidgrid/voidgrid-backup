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
if [ ! -f .env ]; then
  echo "################################################################" >&2
  echo "## ERROR sftp-live-test: .env not found at the repo root" >&2
  echo "## Run: cp env.example .env, then fill in the SFTP section." >&2
  echo "################################################################" >&2
  exit 1
fi
. ./.env

# env.example ships CHANGEME placeholders. Skip loudly, never silently, if
# any variable this test reads is still one.
for v in HETZNER_LINK HETZNER_USER HETZNER_SSH_PORT HETZNER_KEY HETZNER_PASSWORD; do
  eval "val=\${$v:-}"
  if [ "$val" = CHANGEME ]; then
    echo "################################################################" >&2
    echo "## SKIPPING sftp-live-test: $v is still CHANGEME in .env" >&2
    echo "## Nothing was tested. Fill in the SFTP section to run it." >&2
    echo "################################################################" >&2
    exit 0
  fi
done

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
