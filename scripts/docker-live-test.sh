#!/bin/sh
# Run internal/docker's live tests against the local Docker daemon: starts
# throwaway containers (alpine with compose labels, plus PostgreSQL, MariaDB
# and Valkey), runs the tests in a Go container with the Docker socket
# mounted, then removes every container it started.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild
id=$$
probe=vb-live-probe-$id pg=vb-live-pg-$id maria=vb-live-maria-$id valkey=vb-live-valkey-$id
trap 'docker rm -f "$probe" "$pg" "$maria" "$valkey" >/dev/null 2>&1' EXIT
docker run -d --rm --name "$probe" \
  --label com.docker.compose.project=hblive \
  --label com.docker.compose.service=probe \
  --label com.docker.compose.project.working_dir=/srv/hblive \
  alpine:3 sleep 600 >/dev/null
docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=vb-live postgres:16-alpine >/dev/null
docker run -d --rm --name "$maria" -e MARIADB_ROOT_PASSWORD=vb-live mariadb:11.4 >/dev/null
docker run -d --rm --name "$valkey" valkey/valkey:9-alpine >/dev/null
docker run --rm -i \
  --user "$(id -u):$(id -g)" --group-add "$(stat -c %g /var/run/docker.sock)" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD:/src" -w /src \
  -v "$PWD/.cache/gopath:/gopath" \
  -v "$PWD/.cache/gobuild:/tmp/gobuild" \
  -e GOPATH=/gopath -e GOCACHE=/tmp/gobuild -e GOFLAGS=-buildvcs=false -e HOME=/tmp \
  -e VB_LIVE_CONTAINER="$probe" -e VB_LIVE_PG="$pg" -e VB_LIVE_MARIA="$maria" -e VB_LIVE_VALKEY="$valkey" \
  golang:1.27 go test -count=1 -tags dockerlive -run 'TestLive' -v ./internal/docker/
