#!/usr/bin/env bash
# Tests that make land runs origin/main's land.sh and cilocal, never the
# PR branch's older copy (scripts/land.sh, "main's make land"). In a
# throwaway repo with a local bare origin:
#
#   main   carries this land.sh, marked to say "main's land.sh" and stop
#          right after it knows its cilocal (before the lock or gh);
#   old    a PR branch whose land.sh is marked "the branch's land.sh".
#
# Running the branch's ./scripts/land.sh must print main's mark, call a
# cilocal built from main's source, and leave no copy behind. check-fast
# runs it when land.sh or this test changes.
set -euo pipefail
cd "$(dirname "$0")/.."
repo=$(pwd)

scratch=$(mktemp -d /tmp/gfs-land-main-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
failures=0
check() { # $1: what, then the command that must succeed
  local what=$1
  shift
  if "$@"; then echo "ok   $what"; else echo "FAIL $what"; failures=$((failures + 1)); fi
}
has() { grep -q -- "$1" <<<"$out"; }
lacks() { ! has "$1"; }
removed() { [ -n "$copy" ] && [ ! -e "$copy" ]; }

# mark prints land.sh with $1 said right after `trap drop_main_tools EXIT`.
mark() {
  awk -v said="$1" '{ print } /^trap drop_main_tools EXIT$/ {
    print "echo \"" said " ran, with cilocal $cilocal\""
    print "\"$cilocal\" land-migrations 2>&1 || true"
    print "exit 0"
  }' "$repo/scripts/land.sh"
}

git init -q --bare "$scratch/origin.git"
git init -q -b main "$scratch/work"
cd "$scratch/work"
git config user.email test@example.test
git config user.name test
git remote add origin "$scratch/origin.git"
mkdir scripts
cp "$repo/go.mod" .
cp "$repo/scripts/ci-lock.sh" scripts/
cp -R "$repo/scripts/cilocal" scripts/
mark "main's land.sh" >scripts/land.sh
chmod +x scripts/land.sh
git add -A && git commit -qm main && git push -q origin main

git switch -q -c old
mark "the branch's land.sh" >scripts/land.sh
git commit -qam "an older land.sh"

out=$(env -u LAND_MAIN ./scripts/land.sh 7 2>&1) || { echo "$out"; echo "land-main-test: land.sh failed"; exit 1; }
echo "$out" | sed 's/^/  | /'
copy=$(sed -n "s#^main's land.sh ran, with cilocal \(/tmp/gfs-land\.[^/]*\)/cilocal\$#\1#p" <<<"$out")

check "the branch's land.sh hands over to main's" has "^land #7: running origin/main's make land"
check "main's land.sh runs" has "^main's land.sh ran"
check "the branch's land.sh does not go on" lacks "the branch's land.sh ran"
check "it calls a cilocal built from main's source" has "usage: cilocal land-migrations"
check "the copy of main's make land is removed" removed

if [ "$failures" -gt 0 ]; then
  echo "land-main-test: $failures failed"
  exit 1
fi
echo "land-main-test: ok"
