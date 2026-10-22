#!/usr/bin/env bash
# Splits the packages to test across CI shards (round-robin over the sorted
# list) and reports whether this shard needs Postgres.
# Usage: PACKAGES="<import paths>|./..." scripts/ci-shard.sh <index> <count>
# Prints (and appends to $GITHUB_OUTPUT when set):
#   packages=<import paths for this shard>   (empty: nothing to test)
#   db_dirs=<dirs of this shard's packages whose tests use Postgres>
set -euo pipefail
cd "$(dirname "$0")/.."
index=$1 count=$2

if [ "${PACKAGES:-}" = ./... ]; then
  mapfile -t all < <(go list ./...)
else
  read -r -a all <<<"${PACKAGES:-}"
fi
mapfile -t sorted < <(printf '%s\n' "${all[@]}" | sed '/^$/d' | sort)

mine=()
for i in "${!sorted[@]}"; do
  if [ $((i % count)) -eq "$index" ]; then mine+=("${sorted[$i]}"); fi
done

mod=$(go list -m)
db_dirs=()
for p in "${mine[@]}"; do
  d=${p/#$mod/.}
  if grep -lqE 'dbtest|TEST_DATABASE_URL' "$d"/*_test.go 2>/dev/null; then db_dirs+=("$(cd "$d" && pwd)"); fi
done

out() {
  printf '%s=%s\n' "$1" "$2"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then printf '%s=%s\n' "$1" "$2" >>"$GITHUB_OUTPUT"; fi
}
out packages "${mine[*]}"
out db_dirs "${db_dirs[*]}"
