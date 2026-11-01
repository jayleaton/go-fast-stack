#!/usr/bin/env bash
# shellcheck disable=SC2016 # in_lock_shell's scripts are single-quoted: they expand in that shell
# Tests scripts/ci-lock.sh and the cleanup trap of scripts/ci-pg.sh, on a
# private lock path (CI_LOCK_PATH), never the real /tmp/gfs-ci.lock. Runs in
# a few seconds; check-fast runs it when either script changes.
#
#   1. a free lock is taken and released (owner names this pid)
#   2. an owner pid that is no longer running: taken over, and said so
#   3. a live owner: never taken, owner untouched
#   4. no owner file, older than 30 minutes: a loud warning, never broken
#   5. no owner file, fresh: no warning, never broken
#   6. release of a lock owned by another pid leaves it
#   7. ci-pg's trap, in a clone of HEAD with a fake Postgres whose initdb and
#      pg_ctl fail (a cluster that never started): it still deletes its run
#      root and releases the lock, and keeps the failing exit status
set -euo pipefail
cd "$(dirname "$0")/.."
repo=$(pwd)

scratch=$(mktemp -d /tmp/gfs-ci-lock-test.XXXXXX)
live=""
finish() {
  set +eu
  [ -z "$live" ] || kill "$live" 2>/dev/null
  rm -rf "$scratch"
}
trap finish EXIT

export CI_LOCK_PATH=$scratch/lock CI_LOCK_POLL_SECONDS=1 CI_LOCK_REPORT_SECONDS=1
failures=0
check() { # name condition...
  local name=$1
  shift
  if "$@"; then echo "ok    $name"; else
    echo "FAIL  $name"
    failures=$((failures + 1))
  fi
}
# in_lock_shell script: run bash with the library sourced, as its own process
# (the scripts are single-quoted on purpose: they expand in that shell).
in_lock_shell() { bash -c ". '$repo/scripts/ci-lock.sh'; $1"; }
dead_pid() { # a pid that just exited
  local pid
  sleep 0 &
  pid=$!
  wait "$pid"
  echo "$pid"
}
fake_owner() { # pid
  mkdir "$CI_LOCK_PATH"
  echo "pid $1 (someone) a fake run, since 2026-01-01 00:00:00" >"$CI_LOCK_PATH/owner"
}
cleanup_lock() {
  rm -f "$CI_LOCK_PATH/owner"
  [ ! -d "$CI_LOCK_PATH" ] || rmdir "$CI_LOCK_PATH"
}

# 1
out=$(in_lock_shell 'ci_lock_take test 2 && sed -n "s/^pid \([0-9]*\) .*/\1/p" "$ci_lock/owner" && echo "self $$" && ci_lock_release && [ ! -e "$ci_lock" ] && echo released')
check "a free lock is taken with this pid as owner, and released" \
  test "$(sed -n 1p <<<"$out")" = "$(sed -n 's/^self //p' <<<"$out")" -a "$(tail -n 1 <<<"$out")" = released

# 2
fake_owner "$(dead_pid)"
out=$(in_lock_shell 'ci_lock_take test 5 && echo "took $$" && cat "$ci_lock/owner"' 2>&1) || true
check "a dead owner's lock is taken over" grep -q '^took ' <<<"$out"
check "the takeover is logged with the dead owner" grep -q 'took over .*no longer running (was: pid [0-9]* (someone) a fake run' <<<"$out"
check "the owner file names the new holder" grep -q "^pid $(sed -n 's/^took //p' <<<"$out") " <<<"$out"
check "no takeover lock is left" test ! -e "$CI_LOCK_PATH.takeover"
cleanup_lock

# 3
sleep 60 &
live=$!
fake_owner "$live"
before=$(cat "$CI_LOCK_PATH/owner")
out=$(in_lock_shell 'ci_lock_take test 2 && echo took' 2>&1) || true
check "a live owner's lock is never taken" test -z "$(grep '^took' <<<"$out")"
check "a live owner's lock still names it" test "$(cat "$CI_LOCK_PATH/owner")" = "$before"
check "a refusal says who holds it" grep -q "still held after 2s by: pid $live" <<<"$out"
kill "$live"
live=""
cleanup_lock

# 4
mkdir "$CI_LOCK_PATH"
touch -t "$(date -v-40M '+%Y%m%d%H%M' 2>/dev/null || date -d '40 minutes ago' '+%Y%m%d%H%M')" "$CI_LOCK_PATH"
out=$(in_lock_shell 'ci_lock_take test 1 && echo took' 2>&1) || true
check "an old lock with no owner is reported loudly" grep -q 'WARNING: .* has no owner file and is older than 30 minutes' <<<"$out"
check "an old lock with no owner is not broken" test -d "$CI_LOCK_PATH" -a -z "$(grep '^took' <<<"$out")"
cleanup_lock

# 5
mkdir "$CI_LOCK_PATH"
out=$(in_lock_shell 'ci_lock_take test 1 && echo took' 2>&1) || true
check "a fresh lock with no owner gets no warning" test -z "$(grep WARNING <<<"$out")"
check "a fresh lock with no owner is not broken" test -d "$CI_LOCK_PATH" -a -z "$(grep '^took' <<<"$out")"
cleanup_lock

# 6
out=$(in_lock_shell 'ci_lock_take test 2 && echo "pid 1 (other) took it, since now" >"$ci_lock/owner" && ci_lock_release' 2>&1)
check "release leaves a lock another pid owns" test -f "$CI_LOCK_PATH/owner" -a -n "$(grep 'no longer this run' <<<"$out")"
cleanup_lock

# 7
clone=$scratch/clone
git clone -q --local --no-checkout "$repo" "$clone"
git -C "$clone" checkout -q --detach "$(git rev-parse HEAD)"
mkdir "$scratch/pg"
cat >"$scratch/pg/initdb" <<'EOF'
#!/usr/bin/env bash
# A Postgres 18 initdb that fails after leaving a postmaster.pid behind.
[ "$1" != --version ] || { echo "initdb (PostgreSQL) 18.0"; exit 0; }
echo "$2" >"$FAKE_PG_RECORD"
mkdir -p "$2" && echo 1 >"$2/postmaster.pid"
exit 1
EOF
printf '#!/bin/sh\nexit 1\n' >"$scratch/pg/pg_ctl"
printf '#!/bin/sh\nexit 0\n' >"$scratch/pg/createdb"
chmod +x "$scratch/pg"/*
status=0
out=$(env -u CI_LOCK_HOLDER FAKE_PG_RECORD="$scratch/initdb-data" PG_BIN="$scratch/pg" CI_PG_LOCK_WAIT=2 "$clone/scripts/ci-pg.sh" 2>&1) || status=$?
data=$(cat "$scratch/initdb-data" 2>/dev/null || true)
check "ci-pg with a Postgres that never started fails" test "$status" -eq 1
check "its trap still released the lock" test ! -e "$CI_LOCK_PATH"
check "its fake initdb ran in a run root" test -n "$data"
check "its trap still deleted its run root" test ! -e "${data%/data}"

if [ "$failures" -gt 0 ]; then
  echo "ci-lock-test: $failures failed"
  echo "$out" | tail -n 20
  exit 1
fi
echo "ci-lock-test: ok"
