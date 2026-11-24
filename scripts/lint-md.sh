#!/usr/bin/env bash
# Docker-first markdown lint: markdownlint-cli2 (pinned, same in every voidgrid repo) over the repo's
# TRACKED markdown files, using .markdownlint-cli2.jsonc. Local-only files (notes, plans) are never
# linted or rewritten. Extra arguments go to markdownlint-cli2, e.g. --fix to let it rewrite what it can.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mapfile -d '' files < <(git -C "$ROOT" ls-files -z -- '*.md')
if [ "${#files[@]}" -eq 0 ]; then
  echo "lint-md: no tracked markdown files"
  exit 0
fi
exec docker run --rm --user "$(id -u):$(id -g)" -v "$ROOT":/workdir \
  davidanson/markdownlint-cli2:v0.24.2 "$@" "${files[@]}"
