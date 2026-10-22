#!/usr/bin/env bash
# Picks the packages one CI job tests: "db" = packages whose tests use
# Postgres (dbtest or TEST_DATABASE_URL in their _test.go files), "other" =
# the rest, "all" = both (a single job that does everything).
# Usage: PACKAGES="<import paths>|./..." scripts/ci-shard.sh db|other|all
# Prints (and appends to $GITHUB_OUTPUT when set):
#   packages=<import paths>   (empty: nothing to test)
#   db=true|false             some of them need Postgres
set -euo pipefail
cd "$(dirname "$0")/.."
shard=$1

if [ "${PACKAGES:-}" = ./... ]; then
  mapfile -t all < <(go list ./...)
else
  read -r -a all <<<"${PACKAGES:-}"
fi

mod=$(go list -m)
mine=() db=false
for p in "${all[@]}"; do
  [ -n "$p" ] || continue
  d=${p/#$mod/.}
  if grep -lqE 'dbtest|TEST_DATABASE_URL' "$d"/*_test.go 2>/dev/null; then kind=db; else kind=other; fi
  if [ "$shard" = all ] || [ "$kind" = "$shard" ]; then
    mine+=("$p")
    if [ "$kind" = db ]; then db=true; fi
  fi
done

out() {
  printf '%s=%s\n' "$1" "$2"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then printf '%s=%s\n' "$1" "$2" >>"$GITHUB_OUTPUT"; fi
}
out packages "${mine[*]}"
out db "$db"
