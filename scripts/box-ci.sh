#!/usr/bin/env bash
# `make box-ci [COMMIT=<sha>]`: the full checks on exactly one commit, in a
# Davit box. A detached checkout of the commit is synced into the box
# (`davit box run`), the box is pruned to the commit's tracked files (Davit's
# sync never deletes, so a file deleted since the box last ran would linger),
# and then make check-generated (generate plus the diff), lint, build and
# test run. It ends with one line: `box-ci: PASS <sha> ...` or
# `box-ci: FAIL <sha> (<steps>) ...`, and exits 0 only on PASS.
#
# Kept in the box: .env, bin/, tmp/, node_modules/, .cache/ (caches and
# local config, never part of a commit). Everything else that is not tracked
# at the commit is deleted there, never on this machine.
#
# Use it instead of a hand-written `davit box run -- bash -lc "..."`; attest
# what it printed (make ci-local RAN="make box-ci COMMIT=<sha>: PASS").
#
#   COMMIT     the commit to check (default HEAD; any rev git knows)
#   BOX        the box (default <repo>/box-ci: one warm box per repository)
#   BOX_CPUS   vCPUs (default 4), BOX_MEMORY (default 8GiB)
set -euo pipefail
# shellcheck disable=SC2046 # one name per word
unset $(git rev-parse --local-env-vars)
cd "$(dirname "$0")/.."

sha=$(git rev-parse --verify "${COMMIT:-HEAD}^{commit}")
repo=$(basename -s .git "$(git remote get-url origin)")
box=${BOX:-$repo/box-ci}
command -v davit >/dev/null || { echo "box-ci: FAIL ${sha:0:12}: davit is not installed" >&2; exit 2; }

# The commit's files, exactly: a detached checkout, removed on exit.
mkdir -p .cache
dir=$(mktemp -d "$PWD/.cache/box-ci.XXXXXX")
root=$PWD
cleanup() {
  cd "$root" || return
  git worktree remove --force "$dir" 2>/dev/null || true
  git worktree prune || true
}
trap cleanup EXIT
rmdir "$dir"
git worktree add -q --detach "$dir" "$sha"

# What the box keeps: the commit's tracked files, gzip+base64 in an env var.
keep=$(git ls-tree -r --name-only "$sha" | gzip -c | base64 | tr -d '\n')

# Runs in the box, in the synced tree.
read -r -d '' inside <<'BOX' || true
set -uo pipefail
echo "$KEEP" | base64 -d | gunzip -c | sort -u >/tmp/box-ci.keep
find . -path ./.git -prune -o \( -path ./.env -o -path ./bin -o -path ./tmp -o -path ./.cache -o -name node_modules \) -prune -o -type f -print |
  sed 's|^\./||' | sort -u >/tmp/box-ci.have
comm -13 /tmp/box-ci.keep /tmp/box-ci.have >/tmp/box-ci.stale
stale=$(wc -l </tmp/box-ci.stale | tr -d ' ')
while IFS= read -r file; do
  rm -f -- "$file"
done </tmp/box-ci.stale
find . -path ./.git -prune -o -type d -empty -delete 2>/dev/null
# The box's index is its own: make it the commit's tree, so check-generated's
# git diff compares against exactly what was synced.
git add -A >/dev/null 2>&1
git -c user.name=box-ci -c user.email=box-ci@localhost -c commit.gpgsign=false commit -q --allow-empty -m "box-ci $SHA" >/dev/null 2>&1
echo "box-ci: $SHA synced, $stale stale files removed"
failed=() summary=()
start=$(date +%s)
for step in check-generated lint build test; do
  began=$(date +%s)
  echo "== make $step"
  if make --no-print-directory "$step"; then
    summary+=("$step $(($(date +%s) - began))s")
  else
    failed+=("$step")
    summary+=("$step FAILED")
  fi
done
took=$(($(date +%s) - start))
if [ "${#failed[@]}" -eq 0 ]; then
  echo "box-ci: PASS ${SHA:0:12} (${summary[*]}) in ${took}s"
  exit 0
fi
echo "box-ci: FAIL ${SHA:0:12} (${failed[*]}) in ${took}s"
exit 1
BOX

cd "$dir"
status=0
davit box run "$box" --cpus "${BOX_CPUS:-4}" --memory "${BOX_MEMORY:-8GiB}" --wait 20m \
  -e "KEEP=$keep" -e "SHA=$sha" -- bash -c "$inside" || status=$?
if [ "$status" -ne 0 ] && [ "$status" -ne 1 ]; then
  # 1 is the checks failing (its summary is above); anything else is the box.
  echo "box-ci: FAIL ${sha:0:12}: the box run itself failed (exit $status); retry, or see davit box ls"
fi
exit "$status"
