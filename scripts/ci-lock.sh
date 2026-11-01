# shellcheck shell=bash
# The machine-wide CI lock, /tmp/gfs-ci.lock, shared by make ci-pg and make
# land (sourced; docs/CI.md "Local verification"). A directory, because mkdir
# is atomic. Its `owner` file is one line: "pid <pid> (<user>) <what>, since
# <time>".
#
#   ci_lock_take <what> <wait seconds>   wait for it, take it, write owner
#   ci_lock_release                      give it back, if this process holds it
#
# Stale locks. A holder that died without its trap (kill -9, a crash, a trap
# that failed) leaves the lock behind:
#   - owner names a pid that is no longer running: a waiter takes the lock
#     over, says so, and rewrites owner. The takeover runs under a second,
#     short lock ($ci_lock.takeover) and re-reads owner inside it, so two
#     waiters never both take it, and a lock someone else just took is never
#     taken from them.
#   - no owner file (a script that does not write one) and older than
#     $ci_lock_stale_minutes: reported loudly, never broken; its holder may
#     still be running.
#
# Env (tests only): CI_LOCK_PATH, CI_LOCK_POLL_SECONDS, CI_LOCK_REPORT_SECONDS.

ci_lock=${CI_LOCK_PATH:-/tmp/gfs-ci.lock}
ci_lock_poll_seconds=${CI_LOCK_POLL_SECONDS:-10}
ci_lock_report_seconds=${CI_LOCK_REPORT_SECONDS:-60}
readonly ci_lock_stale_minutes=30
ci_locked=false

# ci_lock_owner_pid: the pid the owner file names, or nothing.
ci_lock_owner_pid() {
  sed -n 's/^pid \([0-9][0-9]*\) .*/\1/p' "$ci_lock/owner" 2>/dev/null | head -n 1
}

# ci_lock_running pid: whether a process with that pid exists (any user's).
ci_lock_running() {
  ps -p "$1" >/dev/null 2>&1
}

ci_lock_write_owner() { # what
  echo "pid $$ ($(whoami)) $1, since $(date '+%F %T')" >"$ci_lock/owner"
}

# ci_lock_take_over what dead_pid: take a lock whose owner died, under the
# takeover lock, only if owner still names that same dead pid.
ci_lock_take_over() {
  local what=$1 dead=$2 previous
  mkdir "$ci_lock.takeover" 2>/dev/null || return 1
  if [ "$(ci_lock_owner_pid)" = "$dead" ] && ! ci_lock_running "$dead"; then
    previous=$(cat "$ci_lock/owner" 2>/dev/null)
    ci_lock_write_owner "$what"
    ci_locked=true
    echo "ci-lock: took over $ci_lock: its owner is no longer running (was: $previous)" >&2
  fi
  rmdir "$ci_lock.takeover"
  [ "$ci_locked" = true ]
}

ci_lock_take() { # what wait_seconds
  local what=$1 wait=$2 waited=0 owner pid
  until mkdir "$ci_lock" 2>/dev/null; do
    owner=$(cat "$ci_lock/owner" 2>/dev/null || true)
    pid=$(ci_lock_owner_pid)
    if [ -n "$pid" ] && ! ci_lock_running "$pid" && ci_lock_take_over "$what" "$pid"; then
      return 0
    fi
    if [ $((waited % ci_lock_report_seconds)) -eq 0 ]; then
      if [ -z "$owner" ] && [ -n "$(find "$ci_lock" -maxdepth 0 -mmin +"$ci_lock_stale_minutes" 2>/dev/null)" ]; then
        echo "ci-lock: WARNING: $ci_lock has no owner file and is older than $ci_lock_stale_minutes minutes ($(ls -ld "$ci_lock")): maybe a run that died without releasing it, maybe a long one that writes no owner. Not breaking it. Whoever made it: release it with rmdir $ci_lock" >&2
      fi
      echo "ci-lock: waiting for $ci_lock, held by: ${owner:-unknown (no owner file)} (${waited}s)" >&2
    fi
    if [ "$waited" -ge "$wait" ]; then
      echo "ci-lock: $ci_lock is still held after ${wait}s by: ${owner:-unknown (no owner file)}" >&2
      return 1
    fi
    sleep "$ci_lock_poll_seconds"
    waited=$((waited + ci_lock_poll_seconds))
  done
  ci_locked=true
  ci_lock_write_owner "$what"
}

# ci_lock_release: never fails, never rm -r; only a lock this process holds.
ci_lock_release() {
  [ "$ci_locked" = true ] || return 0
  ci_locked=false
  if [ "$(ci_lock_owner_pid)" != "$$" ]; then
    echo "ci-lock: $ci_lock is no longer this run's (owner: $(cat "$ci_lock/owner" 2>/dev/null || echo none)): left it" >&2
    return 0
  fi
  rm -f "$ci_lock/owner"
  rmdir "$ci_lock" 2>/dev/null || echo "ci-lock: $ci_lock has files this run did not put there: left it" >&2
  return 0
}
