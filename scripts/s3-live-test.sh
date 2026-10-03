#!/bin/sh
# Live backup test against a real S3-compatible bucket (e.g. Backblaze B2's
# S3-compatible endpoint), using credentials from .env at the repo root.
# Everything the test writes goes under a fresh, per-run prefix that the
# test itself deletes when it's done, pass or fail; nothing else in the
# bucket is touched, and nothing here is printed.
#
# .env must define (copy env.example at the repo root to .env):
#   B2_ENDPOINT      host only, no scheme, e.g. s3.us-west-004.backblazeb2.com
#   B2_BUCKET_NAME   the S3-compatible bucket name (not B2_BUCKET_ID)
#   B2_KEY_ID        application key ID -> S3 access key ID
#   B2_KEY_KEY       application key -> S3 secret access key
# The region is derived from B2_ENDPOINT (its second dot-separated label);
# set B2_REGION in .env to override it.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild
if [ ! -f .env ]; then
  echo "################################################################" >&2
  echo "## ERROR s3-live-test: .env not found at the repo root" >&2
  echo "## Run: cp env.example .env, then fill in the S3 section." >&2
  echo "################################################################" >&2
  exit 1
fi
. ./.env

# env.example ships CHANGEME placeholders. Skip loudly, never silently, if
# any variable this test reads is still one.
for v in B2_ENDPOINT B2_BUCKET_NAME B2_BUCKET B2_KEY_ID B2_KEY_KEY B2_REGION; do
  eval "val=\${$v:-}"
  if [ "$val" = CHANGEME ]; then
    echo "################################################################" >&2
    echo "## SKIPPING s3-live-test: $v is still CHANGEME in .env" >&2
    echo "## Nothing was tested. Fill in the S3 section to run it." >&2
    echo "################################################################" >&2
    exit 0
  fi
done

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
