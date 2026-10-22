#!/usr/bin/env bash
# Splits the packages to test across the two CI test shards: "db" gets the
# packages whose tests use Postgres (dbtest or TEST_DATABASE_URL in their
# _test.go files), "other" gets the rest.
# Usage: PACKAGES="<import paths>|./..." scripts/ci-shard.sh db|other
# Prints (and appends to $GITHUB_OUTPUT when set):
#   packages=<import paths for this shard>   (empty: nothing to test)
set -euo pipefail
cd "$(dirname "$0")/.."
shard=$1

if [ "${PACKAGES:-}" = ./... ]; then
  mapfile -t all < <(go list ./...)
else
  read -r -a all <<<"${PACKAGES:-}"
fi

mod=$(go list -m)
mine=()
for p in "${all[@]}"; do
  [ -n "$p" ] || continue
  d=${p/#$mod/.}
  if grep -lqE 'dbtest|TEST_DATABASE_URL' "$d"/*_test.go 2>/dev/null; then kind=db; else kind=other; fi
  if [ "$kind" = "$shard" ]; then mine+=("$p"); fi
done

printf 'packages=%s\n' "${mine[*]}"
if [ -n "${GITHUB_OUTPUT:-}" ]; then printf 'packages=%s\n' "${mine[*]}" >>"$GITHUB_OUTPUT"; fi
