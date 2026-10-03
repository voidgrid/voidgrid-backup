#!/bin/sh
# Run golangci-lint in a container with this repo's .golangci.yml. The release
# workflow runs this before building the image, so any finding blocks a
# release; run it locally before tagging.
# Usage: scripts/lint.sh            (lint everything)
#        scripts/lint.sh run ./internal/server/...
#        scripts/lint.sh config verify
# The linter version is pinned so a new release of it can't fail an unchanged
# tree; bump it on purpose.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild .cache/golangci
[ $# -gt 0 ] || set -- run
exec docker run --rm \
  --user "$(id -u):$(id -g)" \
  -v "$PWD:/src:ro" -w /src \
  -v "$PWD/.cache/gopath:/gopath" \
  -v "$PWD/.cache/gobuild:/tmp/gobuild" \
  -v "$PWD/.cache/golangci:/tmp/golangci" \
  -e GOPATH=/gopath -e GOCACHE=/tmp/gobuild -e GOLANGCI_LINT_CACHE=/tmp/golangci \
  -e GOFLAGS=-buildvcs=false -e HOME=/tmp \
  golangci/golangci-lint:v2.14.0 golangci-lint "$@"
