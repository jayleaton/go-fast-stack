#!/usr/bin/env bash
# Enforces per-package statement coverage floors on a Go coverprofile from
# `go test -race -coverprofile=<profile> ./...`. A floor package missing from
# the profile fails. A package argument (or ONLY=<import path>) checks just
# that floor package.
# TESTED="<import paths>" (CI runs that test only affected packages) checks
# just the floors of packages that were tested.
# Usage: scripts/check-coverage.sh <profile> [<import path>]
set -euo pipefail

mod=github.com/jayleaton/go-fast-stack

# package | minimum statement coverage (%)
floors=(
  "$mod/apps/web/internal/services|90"
  "$mod/apps/web/internal/api|85"
  "$mod/apps/admin/internal/services|90"
  "$mod/apps/admin/internal/api|85"
  "$mod/packages/auth|80" # measured 89.7% when set
)

# --exist: fail if a floor package no longer exists (CI shards check floors
# only for the packages they test, so a renamed package would go unnoticed).
if [ "${1:-}" = --exist ]; then
  pkgs=" $(go list ./... | tr '\n' ' ') "
  failed=0
  for f in "${floors[@]}"; do
    pkg=${f%%|*}
    if [[ $pkgs != *" $pkg "* ]]; then echo "FAIL  floor package $pkg does not exist"; failed=1; fi
  done
  exit "$failed"
fi

profile=${1:?usage: $0 <coverprofile> [<import path>]}
only=${2:-${ONLY:-}}
[ -r "$profile" ] || { echo "coverage: cannot read $profile"; exit 1; }

if [ -n "$only" ]; then
  sel=()
  for f in "${floors[@]}"; do [ "${f%%|*}" = "$only" ] && sel+=("$f"); done
  [ "${#sel[@]}" -gt 0 ] || { echo "coverage: no floor for $only"; exit 1; }
  floors=("${sel[@]}")
fi

# Per package: covered and total statements. Blocks repeat across test
# binaries, so dedupe by block key and keep the max count.
pcts=$(awk 'NR > 1 && NF == 3 {
    key = $1; n = $2; c = $3
    if (!(key in stmts)) stmts[key] = n
    if (c + 0 > hit[key] + 0) hit[key] = c
  }
  END {
    for (key in stmts) {
      file = key; sub(/:[^:]*$/, "", file)
      pkg = file; sub(/\/[^\/]*$/, "", pkg)
      total[pkg] += stmts[key]
      if (hit[key] > 0) covered[pkg] += stmts[key]
    }
    for (pkg in total) printf "%s %.1f\n", pkg, (total[pkg] ? 100 * covered[pkg] / total[pkg] : 100)
  }' "$profile")

failed=0
for f in "${floors[@]}"; do
  pkg=${f%%|*} floor=${f#*|}
  if [ -n "${TESTED:-}" ] && [[ " $TESTED " != *" $pkg "* ]]; then
    echo "skip  $pkg (not affected)"
    continue
  fi
  pct=$(awk -v p="$pkg" '$1 == p { print $2 }' <<<"$pcts")
  if [ -z "$pct" ]; then
    echo "FAIL  $pkg not in profile (floor $floor%)"
    failed=1
  elif awk -v a="$pct" -v b="$floor" 'BEGIN { exit !(a + 0 >= b + 0) }'; then
    echo "ok    $pkg $pct% (floor $floor%)"
  else
    echo "FAIL  $pkg $pct% (floor $floor%)"
    failed=1
  fi
done
exit "$failed"
