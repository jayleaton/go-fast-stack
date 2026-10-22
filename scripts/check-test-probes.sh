#!/usr/bin/env bash
# Proves the test-boundary checks still bite: the clean tree passes
# check-test-boundaries.sh, planted test files outside the boundaries are named
# by it, planted uncovered statements sink services and the UI below their
# floors in check-coverage.sh, and scripts/testbudget fails a slow package and
# a slow test. Probes are always removed. (depguard rules for test files are
# proven by check-layering.sh.)
# PROFILE=<coverprofile of a run that tested the probed packages> reuses it
# (CI passes its test run's); otherwise the probed packages are tested here.
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
  packages/secrets/zz_probe/zz_probe_test.go
  apps/admin/internal/validate/zz_probe_test.go
  packages/database/adminrepo/model/zz_probe_test.go
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

# Coverage: a file of uncovered statements, added to a real profile, sinks
# each floor (~200 sink each app's services; apps/web/ui is ~3800 statements,
# so 300 drop it ~3 points). go test counts every statement of a package, so
# a planted profile block is what a planted untested file would produce.
probed=(apps/web/internal/services:200 apps/admin/internal/services:200 apps/web/ui:300)
profile=${PROFILE:-}
mkdir -p .cache
if [ -z "$profile" ]; then
  profile=.cache/probe-clean.out
  paths+=("$profile")
  pkgs=()
  for p in "${probed[@]}"; do pkgs+=("./${p%:*}"); done
  if ! log=$(go test -coverprofile="$profile" "${pkgs[@]}" 2>&1); then
    fail "go test on the probed packages failed"; echo "$log"
  fi
fi
planted_profile=.cache/probe.out
paths+=("$planted_profile")
for p in "${probed[@]}"; do
  dir=${p%:*} n=${p#*:}
  if ! out=$(./scripts/check-coverage.sh "$profile" "$mod/$dir"); then
    fail "$dir is below its floor before the probe: $out"
    continue
  fi
  { cat "$profile"; printf '%s/zz_probe.go:1.1,%d.2 %d 0\n' "$mod/$dir" "$n" "$n"; } >"$planted_profile"
  if out=$(./scripts/check-coverage.sh "$planted_profile" "$mod/$dir"); then
    fail "$n uncovered statements did not fail the $dir floor: $out"
  else
    pass "$n uncovered statements fail the $dir floor: ${out#FAIL  }"
  fi
done

# Test-time budgets (testbudget's defaults, which CI uses): a slow package
# and a slow test fail, a fast run passes.
budget=$(mktemp -d)
dirs+=("$budget")
go build -o "$budget/testbudget" ./scripts/testbudget
paths+=("$budget/testbudget")
run_event() { printf '{"Action":"%s","Package":"%s/zz_probe","Test":"%s","Elapsed":%s}\n' "$1" "$mod" "$2" "$3"; }
fast=$( { run_event pass TestFast 0.1; run_event pass "" 0.2; } | "$budget/testbudget" -top 0 2>&1) &&
  pass "a fast run passes the test-time budget" || { fail "a fast run fails the test-time budget"; echo "$fast"; }
# A 1s package holding a 900s subtest trips the test budget (its parent is
# not judged: it includes its subtests); a 900s package
# trips the package budget.
slow_test() { run_event pass TestSlow/case 900; run_event pass TestSlow 900; run_event pass "" 1; }
slow_package() { run_event pass "" 900; }
for kind in test package; do
  if out=$("slow_$kind" | "$budget/testbudget" -top 0 2>&1); then
    fail "a 900s $kind passed the test-time budget"
  elif grep -q "^  $kind $mod/zz_probe" <<<"$out"; then
    pass "a 900s $kind fails the test-time budget"
  else
    fail "testbudget did not name the slow $kind: $out"
  fi
done
rm -f "$budget/testbudget"

exit "$failed"
