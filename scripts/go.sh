#!/bin/sh
# Run the Go toolchain in a container (no Go install on the dev machine).
# Usage: scripts/go.sh test ./...   |   scripts/go.sh version
# Mounts ONLY this repo and its ./.cache.
# GOPATH lives in ./.cache/gopath so the module cache and the checksum
# database cache are both persistent and writable by the calling user.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild
exec docker run --rm -i \
  --user "$(id -u):$(id -g)" \
  -v "$PWD:/src" -w /src \
  -v "$PWD/.cache/gopath:/gopath" \
  -v "$PWD/.cache/gobuild:/tmp/gobuild" \
  -e GOPATH=/gopath -e GOCACHE=/tmp/gobuild -e GOFLAGS=-buildvcs=false -e HOME=/tmp \
  golang:1.27 go "$@"
