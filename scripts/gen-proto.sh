#!/bin/sh
# Regenerate internal/proto/agentpb from internal/proto/*.proto, in a container.
# Mounts ONLY this repo.
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm \
  -v "$PWD:/src" -w /src \
  golang:1.27 sh -euc "
    apt-get update -qq >/dev/null && apt-get install -y -qq protobuf-compiler >/dev/null
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
    mkdir -p internal/proto/agentpb
    protoc -I internal/proto \
      --go_out=internal/proto/agentpb --go_opt=paths=source_relative \
      --go-grpc_out=internal/proto/agentpb --go-grpc_opt=paths=source_relative \
      internal/proto/*.proto
    chown -R $(id -u):$(id -g) internal/proto/agentpb
  "
