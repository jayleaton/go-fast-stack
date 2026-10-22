#!/usr/bin/env bash
# Proves the test-boundary checks still bite: the clean tree passes
# check-test-boundaries.sh, planted test files outside the boundaries are named
# by it, and ~200 planted uncovered statements sink services below its floor in
# check-coverage.sh. Probes are always removed. (depguard rules for test files
# are proven by check-layering.sh.)
set -euo pipefail
cd "$(dirname "$0")/.."

mod=github.com/jayleaton/ai-patterns-go
paths=() dirs=()
cleanup() {
  rm -f "${paths[@]}"
  for d in "${dirs[@]}"; do rmdir "$d" 2>/dev/null || true; done
}
trap cleanup EXIT

failed=0
pass() { echo "ok    $1"; }
fail() { echo "FAIL  $1"; failed=1; }

# Location.
if out=$(./scripts/check-test-boundaries.sh 2>&1); then
  pass "clean tree: every test file is inside the test boundaries"
else
  fail "clean tree: test files outside the test boundaries"; echo "$out"
fi

planted=(
  apps/web/internal/validate/zz_probe_test.go
  packages/database/dbtest/zz_probe_test.go
)
for f in "${planted[@]}"; do
  d=$(dirname "$f")
  [ -d "$d" ] || { mkdir -p "$d"; dirs+=("$d"); }
  paths+=("$f")
  printf 'package %s_test\n' "$(basename "$d")" > "$f"
done
if out=$(./scripts/check-test-boundaries.sh 2>&1); then
  fail "check-test-boundaries.sh passed with planted test files"
else
  for f in "${planted[@]}"; do
    if grep -qxF "  $f" <<<"$out"; then
      pass "test file outside the boundaries is rejected: $f"
    else
      fail "test file outside the boundaries is not reported: $f"
    fi
  done
fi
rm -f "${planted[@]}"

# A test in a subpackage of a tree boundary (apps/web/ui/**) is accepted.
nested=apps/web/ui/zz_probe/zz_probe_test.go
mkdir -p "$(dirname "$nested")"; dirs+=("$(dirname "$nested")"); paths+=("$nested")
printf 'package zz_probe_test\n' > "$nested"
if out=$(./scripts/check-test-boundaries.sh 2>&1); then
  pass "test file in a tree boundary is accepted: $nested"
else
  fail "test file in a tree boundary is rejected: $nested"; echo "$out"
fi
rm -f "$nested"

# Coverage.
probe=apps/web/internal/services/zz_probe.go
profile=.cache/probe.out
mkdir -p .cache
paths+=("$probe" "$profile")
{
  printf 'package services\n\nfunc zzCoverageProbe(x int) int {\n'
  for i in $(seq 200); do printf '\tx += %d\n' "$i"; done
  printf '\treturn x\n}\n'
} > "$probe"
if ! log=$(go test -race -coverprofile="$profile" ./apps/web/internal/services/ 2>&1); then
  fail "go test on the coverage probe failed"; echo "$log"
elif out=$(./scripts/check-coverage.sh "$profile" "$mod/apps/web/internal/services"); then
  fail "201 uncovered statements did not fail the services floor: $out"
else
  pass "201 uncovered statements fail the services floor: ${out#FAIL  }"
fi

exit "$failed"
