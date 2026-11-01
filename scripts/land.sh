#!/usr/bin/env bash
# make land PR=<n>: land a pull request inside one hold of the machine-wide CI
# lock /tmp/gfs-ci.lock, so nothing merges between its rebase and its merge
# and no other PR is left behind main by it:
#
#   take the lock -> fetch, rebase onto the base, push -> full CI (make ci-pg,
#   inside this hold) -> attest (make ci-local) -> wait for the required
#   checks -> make pr-merge -> release the lock.
#
# Run it in a clean worktree checked out on the PR's branch. Any failure
# stops it, says why, and releases the lock; nothing merges.
#
# Env: LAND_LOCK_WAIT     seconds to wait for the lock (default 3600)
#      LAND_CHECKS_WAIT   seconds to wait for the PR's checks (default 900)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

readonly lock_wait=${LAND_LOCK_WAIT:-3600}
readonly checks_wait=${LAND_CHECKS_WAIT:-900}
readonly poll_seconds=10
readonly report_seconds=60

pr=${1:-}
fail() {
  echo "land${pr:+ #$pr}: $*" >&2
  exit 1
}
say() { printf '\n== land #%s: %s (%ss)\n' "$pr" "$1" "$SECONDS"; }
case "$pr" in '' | *[!0-9]*) fail "usage: make land PR=<number>" ;; esac

# ---- main's make land ----
# A queued PR's branch carries the land.sh and cilocal it was cut with,
# which may predate fixes on main (a new wait, the migration renumber). So
# make land always runs main's: it copies origin/main's land.sh and
# ci-lock.sh, builds its cilocal, and re-runs that land.sh in this checkout
# with LAND_MAIN naming the copy (removed when it ends). The PR's own tree
# is still what make ci-pg tests and make ci-local attests.
if [ -z "${LAND_MAIN:-}" ]; then
  git fetch -q origin main || fail "git fetch origin main failed"
  main_tools=$(mktemp -d /tmp/gfs-land.XXXXXX)
  if ! git archive origin/main go.mod scripts/land.sh scripts/ci-lock.sh scripts/cilocal | tar -x -C "$main_tools" ||
    ! (cd "$main_tools" && "${GO:-go}" build -o cilocal ./scripts/cilocal); then
    rm -rf "$main_tools"
    fail "could not copy and build origin/main's make land"
  fi
  echo "land #$pr: running origin/main's make land ($(git rev-parse --short origin/main))"
  LAND_MAIN=$main_tools exec bash "$main_tools/scripts/land.sh" "$@"
fi
readonly cilocal=$LAND_MAIN/cilocal
# Not for what this runs: a nested land.sh (the pre-push hook's check-fast
# tests one) must hand over itself, and must never remove this copy.
export -n LAND_MAIN
# drop_main_tools removes the copy, and only a copy make land made.
drop_main_tools() {
  case $LAND_MAIN in /tmp/gfs-land.*) rm -rf "$LAND_MAIN" ;; esac
}
trap drop_main_tools EXIT

# ---- the PR and this checkout ----
IFS=$'\t' read -r state base branch < <(gh pr view "$pr" --json state,baseRefName,headRefName -q '[.state, .baseRefName, .headRefName] | @tsv') ||
  fail "gh pr view $pr failed"
[ "$state" = OPEN ] || fail "it is $state, not open"
[ "$(git branch --show-current)" = "$branch" ] || fail "this worktree is on $(git branch --show-current), not the PR's branch $branch: git switch $branch (or run it in that branch's worktree)"
[ -z "$(git status --porcelain)" ] || fail "the tree has uncommitted or untracked files (git status): commit or remove them first"

# ---- the lock, held to the end ----
# shellcheck source=scripts/ci-lock.sh
. "$LAND_MAIN/scripts/ci-lock.sh"
# release: set +e, so no failing step can keep the lock, which goes last.
release() {
  local status=$?
  set +eu
  trap - EXIT INT TERM
  drop_main_tools
  [ "$status" -eq 0 ] || echo "land #$pr: FAILED (exit $status); nothing was merged unless make pr-merge said so above" >&2
  if [ "$ci_locked" = true ]; then
    ci_lock_release
    echo "land #$pr: released $ci_lock"
  fi
  exit "$status"
}
trap release EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

say "take $ci_lock"
ci_lock_take "make land PR=$pr on $(pwd)" "$lock_wait" ||
  fail "Wait for it, or if its process is gone, release it: rm -f $ci_lock/owner && rmdir $ci_lock"

# ---- fetch, rebase, push ----
# A PR stacked on a parent that has since been squash-merged still holds
# the parent's own commits: a plain rebase would replay them on top of the
# squash. cilocal land-rebase (verify.PlanStackRebase) finds that parent,
# through the PR's base branch or, once retargeted, a merged PR whose head is
# one of this branch's commits, retargets the PR to the parent's base, and
# the rebase replays only this branch's own commits (--onto, as make
# pr-merge prints for each PR it retargets).
git fetch -q origin
plan=$("$cilocal" land-rebase "$pr") || fail "no rebase plan (above)"
base=$(sed -n 's/^base //p' <<<"$plan")
upstream=$(sed -n 's/^upstream //p' <<<"$plan")
echo "${plan##*reason }"
if [ "$upstream" = - ]; then
  say "rebase onto origin/$base"
  rebase=(git rebase -q "origin/$base")
else
  say "rebase onto origin/$base, after the merged parent's ${upstream:0:12}"
  rebase=(git rebase -q --onto "origin/$base" "$upstream")
fi
if ! "${rebase[@]}"; then
  git rebase --abort || true
  fail "it does not rebase cleanly (${rebase[*]}): resolve the conflict, push, then make land again"
fi
# New migrations above main's highest, now that the rebase took in whatever
# merged while this PR waited (cilocal land-migrations: renames them in
# order, rewrites their mentions, commits "chore: renumber migration"; a
# changed existing migration, or two new ones with one number, stop here).
say "migrations above origin/$base"
"$cilocal" land-migrations "$base" || fail "its migrations cannot be renumbered (above): fix that, push, then make land again"
if [ "$(git rev-parse HEAD)" != "$(git rev-parse "origin/$branch" 2>/dev/null || echo none)" ]; then
  git push -q --force-with-lease origin "HEAD:$branch"
  echo "pushed $(git rev-parse --short HEAD)"
fi
head=$(git rev-parse HEAD)

# ---- full CI, inside this hold ----
say "full CI on ${head:0:12} (make ci-pg)"
CI_LOCK_HOLDER=$$ make ci-pg
[ "$(git rev-parse HEAD)" = "$head" ] || fail "HEAD moved during CI"

# ---- attest ----
say "attest"
make ci-local RAN="make land PR=$pr: make ci-pg (make ci, make check-migrations, release smoke on a throwaway Postgres 18) on ${head:0:12}, rebased onto origin/$base inside one hold of $ci_lock"

# ---- wait for the required checks ----
# make pr-merge decides (verify.Decide). --require-actions: until GitHub
# starts the checks the attestation's body edit triggers, nothing has run,
# and that is not yet, never "merge on local-ci alone".
say "wait for the checks"
waited=0
while :; do
  verdict=$("$cilocal" merge --dry-run --require-actions "$pr" 2>&1 || true)
  # Its verdict is the "#<n>: <action> - <reason>" line; a ready PR also
  # prints a note line ("#<n>: GitHub's checks are green").
  line=$(grep -m1 -E "^#$pr: (merge|skip|done) - " <<<"$verdict" || echo "#$pr: no verdict: $verdict")
  case "$line" in
  "#$pr: merge - "*) break ;;
  *"has checks still running"* | *"mergeability is still being computed"* | *"has no GitHub CI job that ran"* | *"has no GitHub CI job on the head yet"*) ;;
  *) fail "make pr-merge would not merge it: ${line#"#$pr: "}" ;;
  esac
  [ "$waited" -lt "$checks_wait" ] || fail "its checks were not green after ${checks_wait}s: ${line#"#$pr: "}"
  [ $((waited % report_seconds)) -ne 0 ] || echo "land #$pr: ${line#"#$pr: "} (${waited}s)"
  sleep "$poll_seconds"
  waited=$((waited + poll_seconds))
done
echo "${line}"

# ---- merge ----
say "merge"
"$cilocal" merge --require-actions "$pr"
[ "$(gh pr view "$pr" --json state -q .state)" = MERGED ] || fail "make pr-merge did not merge it (above)"
say "landed ${head:0:12}"
