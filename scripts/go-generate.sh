#!/usr/bin/env bash
# go generate, one process per package, as many at once as there are CPUs
# (make generate, make check-fast): `go generate ./...` runs every mockgen
# directive one after another (5.5-7.1 s on 4 vCPU against 2.7 s in parallel,
# docs/ci-speed.md). Each package's own directives still run in order.
#
# Usage: scripts/go-generate.sh [<package dir>...]
#        (default: every root-module package with a //go:generate directive)
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}
if [ "$#" -gt 0 ]; then
  dirs=("$@")
else
  # The root module only (deploy/ is its own module), never testdata, as
  # `go generate ./...` from the root.
  mapfile -t dirs < <(git grep -l '^//go:generate' -- '*.go' ':!deploy/**' ':!**/testdata/**' |
    xargs -n1 dirname | sort -u | sed 's|^|./|')
fi
[ "${#dirs[@]}" -gt 0 ] || exit 0
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
printf '%s\n' "${dirs[@]}" | xargs -P "$jobs" -n 1 "$GO" generate
