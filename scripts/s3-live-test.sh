#!/bin/sh
# Live backup test against a real S3-compatible bucket (e.g. Backblaze B2's
# S3-compatible endpoint), using credentials from .env at the repo root.
# Everything the test writes goes under a fresh, per-run prefix that the
# test itself deletes when it's done, pass or fail; nothing else in the
# bucket is touched, and nothing here is printed.
#
# .env must define (see .env.example or the repo's own .env):
#   B2_ENDPOINT      host only, no scheme, e.g. s3.us-west-004.backblazeb2.com
#   B2_BUCKET_NAME   the S3-compatible bucket name (not B2_BUCKET_ID)
#   B2_KEY_ID        application key ID -> S3 access key ID
#   B2_KEY_KEY       application key -> S3 secret access key
# The region is derived from B2_ENDPOINT (its second dot-separated label);
# set B2_REGION in .env to override it.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild
[ -f .env ] || { echo ".env not found; see the B2_* names above" >&2; exit 1; }
. ./.env

: "${B2_ENDPOINT:?set in .env}"
bucket="${B2_BUCKET_NAME:-${B2_BUCKET:?set B2_BUCKET_NAME or B2_BUCKET in .env}}"
: "${B2_KEY_ID:?set in .env}" "${B2_KEY_KEY:?set in .env}"
endpoint=$(printf '%s' "$B2_ENDPOINT" | sed -e 's~^https\?://~~')
region="${B2_REGION:-$(printf '%s' "$endpoint" | awk -F. '{print $2}')}"
prefix="vb-live-test-$$-$(date +%s)/"

docker run --rm -i \
  --user "$(id -u):$(id -g)" \
  -v "$PWD:/src" -w /src \
  -v "$PWD/.cache/gopath:/gopath" \
  -v "$PWD/.cache/gobuild:/tmp/gobuild" \
  -e GOPATH=/gopath -e GOCACHE=/tmp/gobuild -e GOFLAGS=-buildvcs=false -e HOME=/tmp \
  -e VB_LIVE_S3_ENDPOINT="$endpoint" -e VB_LIVE_S3_BUCKET="$bucket" \
  -e VB_LIVE_S3_REGION="$region" -e VB_LIVE_S3_ACCESS_KEY="$B2_KEY_ID" \
  -e VB_LIVE_S3_SECRET_KEY="$B2_KEY_KEY" -e VB_LIVE_S3_PREFIX="$prefix" \
  golang:1.27 go test -count=1 -tags s3live -run TestLiveS3Backup -v ./internal/engine/
