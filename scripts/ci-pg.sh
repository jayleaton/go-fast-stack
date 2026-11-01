#!/usr/bin/env bash
# make ci-pg: the full local CI without Docker or a Davit box. On a clean
# checkout it starts a throwaway Postgres 18 cluster (initdb + pg_ctl) on a
# free port and runs, against it:
#
#   1. make ci                (TEST_DATABASE_URL points at the cluster)
#   2. make check-migrations
#   3. release smoke          (fresh smoke_app and smoke_admin databases, on
#                              free smoke ports)
#
# It takes the machine-wide CI mutex /tmp/gfs-ci.lock (a directory: mkdir is
# atomic) so two runs never share ports or starve each other, and tears the
# cluster down and releases the lock on any exit, Ctrl-C or kill included.
# The last line names the commit and what ran, for make ci-local RAN="…".
#
# Under a landing (make land) the caller already holds the lock for the whole
# rebase, CI, attest and merge: CI_LOCK_HOLDER=<its pid> runs the checks
# inside that hold, without taking or releasing the lock, once
# /tmp/gfs-ci.lock/owner names that pid and it is an ancestor of this run.
#
# Env: PG_BIN               the Postgres 18 bin dir (else initdb on PATH, else
#                           the usual Homebrew and Debian locations)
#      CI_PG_LOCK_WAIT      seconds to wait for the lock (default 3600)
#      CI_LOCK_HOLDER       the pid of an ancestor that holds the lock (above)
set -euo pipefail
cd "$(dirname "$0")/.."

readonly postgres_major=18 # CI's and the compose file's postgres:18
readonly lock_wait=${CI_PG_LOCK_WAIT:-3600}
readonly port_low=20000 port_span=40000 # free ports are picked in [20000, 60000)

fail() {
  echo "ci-pg: $*" >&2
  exit 1
}
step() { printf '\n== ci-pg: %s (%ss)\n' "$1" "$SECONDS"; }

# ---- the tree is clean ----
# What runs is what the head says: an edit or an untracked file would be in
# the build but not in the commit an attestation names.
[ -z "$(git status --porcelain)" ] ||
  fail "the tree has uncommitted or untracked files (git status): commit or remove them first, so what runs is exactly HEAD"
head=$(git rev-parse HEAD)

# ---- Postgres 18 ----
pg_bin=${PG_BIN:-}
if [ -z "$pg_bin" ]; then
  for candidate in "$(dirname "$(command -v initdb 2>/dev/null || echo /nonexistent/initdb)")" \
    /opt/homebrew/opt/postgresql@$postgres_major/bin /usr/local/opt/postgresql@$postgres_major/bin \
    /usr/lib/postgresql/$postgres_major/bin; do
    if [ -x "$candidate/initdb" ] && "$candidate/initdb" --version | grep -q " $postgres_major\."; then
      pg_bin=$candidate
      break
    fi
  done
fi
[ -n "$pg_bin" ] && [ -x "$pg_bin/initdb" ] ||
  fail "no Postgres $postgres_major initdb found: install it (brew install postgresql@$postgres_major, apt install postgresql-$postgres_major) or set PG_BIN to its bin dir"
"$pg_bin/initdb" --version | grep -q " $postgres_major\." ||
  fail "$pg_bin/initdb is not Postgres $postgres_major ($("$pg_bin/initdb" --version)): CI runs $postgres_major; set PG_BIN"
for tool in pg_ctl createdb; do [ -x "$pg_bin/$tool" ] || fail "$pg_bin/$tool is missing"; done

# ---- teardown, set before anything needs it ----
# Cleanup deletes exactly one thing: run_root, the directory this run made
# with mktemp (below), named by this quoted variable, never by a pattern. The
# lock (scripts/ci-lock.sh) is released with rm of its own owner file and
# rmdir, not rm -r, and only when this run took it. Nothing else under /tmp
# is ever removed.
# shellcheck source=scripts/ci-lock.sh
. scripts/ci-lock.sh
readonly run_root_prefix=/tmp/gfs-ci-pg.
run_root="" child=""
kill_tree() { # pid: the process and everything under it, children first
  local kid
  for kid in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$kid"; done
  kill -TERM "$1" 2>/dev/null || true
}
# remove_run_root: rm -rf of run_root, only while it is still what mktemp
# made: a real directory (not a symlink) directly in /tmp, named
# gfs-ci-pg. plus mktemp's 6 characters.
remove_run_root() {
  local suffix=${run_root#"$run_root_prefix"}
  if [ "$suffix" = "$run_root" ] || [ "${#suffix}" -ne 6 ] || [ "${suffix//\//}" != "$suffix" ] ||
    [ -L "$run_root" ] || [ ! -d "$run_root" ]; then
    echo "ci-pg: not deleting '$run_root': it is not the run root this run made" >&2
    return
  fi
  rm -rf "$run_root"
}
# cleanup: every step may fail without stopping the next (set +e: a failed
# step must never leave the lock behind, as a trap under set -e once did when
# it stopped a Postgres that never started), and the lock goes last, once
# nothing of this run is left to collide with the next one.
cleanup() {
  local status=$?
  set +eu
  trap - EXIT INT TERM
  [ -z "$child" ] || kill_tree "$child"
  if [ -n "$run_root" ]; then
    [ ! -f "$run_root/data/postmaster.pid" ] || "$pg_bin/pg_ctl" -D "$run_root/data" -m immediate -w stop >/dev/null 2>&1
    remove_run_root
  fi
  [ "$status" -eq 0 ] || echo "ci-pg: FAILED on ${head:-?} (exit $status); cleaned up: no cluster left, and $ci_lock released if this run held it" >&2
  ci_lock_release
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the lock ----
# held_by_ancestor pid: the lock's owner file names `pid` and it is this
# run's parent, grandparent or further up.
held_by_ancestor() {
  local holder=$1 ancestor=$$
  case "$holder" in '' | *[!0-9]*) return 1 ;; esac
  [ "$(ci_lock_owner_pid)" = "$holder" ] || return 1
  while [ "$ancestor" -gt 1 ]; do
    ancestor=$(ps -o ppid= -p "$ancestor" | tr -d ' ')
    [ "$ancestor" != "$holder" ] || return 0
  done
  return 1
}
if [ -n "${CI_LOCK_HOLDER:-}" ]; then
  held_by_ancestor "$CI_LOCK_HOLDER" ||
    fail "CI_LOCK_HOLDER=$CI_LOCK_HOLDER, but $ci_lock is not held by that pid, or it is not an ancestor of this run (owner: $(cat "$ci_lock/owner" 2>/dev/null || echo none)): unset it, and ci-pg takes the lock itself"
  echo "ci-pg: inside the lock held by pid $CI_LOCK_HOLDER ($(cat "$ci_lock/owner"))"
else
  ci_lock_take "make ci-pg on $(pwd) at ${head:0:12}" "$lock_wait" ||
    fail "Wait for that run, or if its process is gone, release it: rm -f $ci_lock/owner && rmdir $ci_lock"
fi

# free_port: one nothing answers on now (the lock keeps other CI runs off it).
free_port() {
  local port
  while :; do
    port=$((port_low + RANDOM * RANDOM % port_span))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      echo "$port"
      return
    fi
  done
}

# run: a step in the background, so INT/TERM reach the trap at once and it
# can kill the whole step (bash runs traps only between foreground commands).
run() {
  "$@" &
  child=$!
  wait "$child"
  child=""
}

# ---- the cluster ----
# TCP on 127.0.0.1 only, no Unix socket: everything connects by host and
# port, and a socket under a long $TMPDIR exceeds the 103-byte path limit.
#
# Every temp file of the run lives under this one root in /tmp, which cleanup
# deletes: the cluster, and (TMPDIR, GOTMPDIR) the checks' own, Go's build
# dirs included. A killed go leaves its build dir behind, and a TMPDIR inside
# the checkout (some agent harnesses set one) would put its generated
# .cover.go files where the next run's gofmt walks.
step "Postgres $postgres_major cluster"
run_root=$(mktemp -d "${run_root_prefix}XXXXXX")
pgdata=$run_root
mkdir "$pgdata/tmp"
export TMPDIR=$pgdata/tmp GOTMPDIR=$pgdata/tmp
pgport=$(free_port)
"$pg_bin/initdb" -D "$pgdata/data" -U postgres --auth=trust -E UTF8 --no-instructions >"$pgdata/initdb.log" 2>&1 ||
  { cat "$pgdata/initdb.log" >&2; fail "initdb failed"; }
"$pg_bin/pg_ctl" -D "$pgdata/data" -l "$pgdata/postgres.log" -w \
  -o "-p $pgport -k '' -c listen_addresses=127.0.0.1 -c max_connections=200 -c fsync=off" start >/dev/null ||
  { tail -n 40 "$pgdata/postgres.log" >&2; fail "Postgres did not start on port $pgport"; }
base=postgres://postgres@127.0.0.1:$pgport
echo "postgres $("$pg_bin/postgres" --version | sed -E 's/^[^0-9]*([0-9][0-9.]*).*/\1/') on 127.0.0.1:$pgport, data in $pgdata"

# ---- 1-3 ----
step "make ci"
run env TEST_DATABASE_URL="$base/postgres?sslmode=disable" make ci
step "make check-migrations"
run make check-migrations
step "release smoke"
for database in smoke_app smoke_admin; do "$pg_bin/createdb" -h 127.0.0.1 -p "$pgport" -U postgres "$database"; done
web_port=$(free_port)
admin_port=$(free_port)
while [ "$admin_port" = "$web_port" ]; do admin_port=$(free_port); done
run env SMOKE_DATABASE_URL="$base/smoke_app?sslmode=disable" SMOKE_ADMIN_DATABASE_URL="$base/smoke_admin?sslmode=disable" \
  SMOKE_WEB_PORT="$web_port" SMOKE_ADMIN_PORT="$admin_port" ./scripts/release-smoke.sh

# ---- what ran is still HEAD ----
[ "$(git rev-parse HEAD)" = "$head" ] || fail "HEAD moved during the run (was $head): run it again"
[ -z "$(git status --porcelain)" ] || fail "the run left the tree dirty (git status): a check wrote a tracked file; fix that, then run it again"

step "ok"
echo "CI-PG-OK $head: make ci, make check-migrations and release smoke on Postgres $postgres_major (scripts/ci-pg.sh)"
echo "attest it: make ci-local RAN=\"make ci-pg (make ci, make check-migrations, release smoke on a throwaway Postgres $postgres_major)\""
