#!/usr/bin/env bash
# The nightly full suite on main (`make nightly`, docs/CI.md "Nightly"):
# everything a pull request may have skipped by running only what it
# affected. In a detached checkout of origin/main (.cache/nightly) it runs
# make ci, make test-flaky (-race -count=5 -shuffle=on) and
# make secret-scan ARGS=--all.
#
# Green: attests the commit (make ci-local COMMIT=<sha>) when nothing has yet,
# so staging can catch up to it. Red: opens a "Nightly failed on main" issue,
# or comments on the open one, keeps the checkout for a look, and exits 1. It
# never posts a failed local-ci: that is final for the commit and would keep
# it off staging over what may be a flake.
#
# Run it from cron on a machine with Docker and gh (docs/CI.md "Nightly").
# NIGHTLY_COMMIT=<rev> checks another commit; NIGHTLY_REPORT=0 skips the fetch
# and every GitHub call (attest, issue).
set -euo pipefail
# shellcheck disable=SC2046 # one name per word
unset $(git rev-parse --local-env-vars)
cd "$(dirname "$0")/.."
root=$PWD
report=${NIGHTLY_REPORT:-1}
title="Nightly failed on main"

if [ "$report" != 0 ]; then git fetch -q origin main; fi
sha=$(git rev-parse --verify "${NIGHTLY_COMMIT:-origin/main}^{commit}")
dir=$root/.cache/nightly
log=$root/.cache/nightly.log
mkdir -p "$root/.cache"
if [ -e "$dir" ]; then git worktree remove --force "$dir"; fi
git worktree prune
git worktree add -q --detach "$dir" "$sha"
: >"$log"
echo "nightly: $sha in $dir (log: $log)"

steps=("make ci" "make test-flaky" "make secret-scan ARGS=--all")
failed=""
start=$(date +%s)
for step in "${steps[@]}"; do
  echo "== $step" | tee -a "$log"
  if ! (cd "$dir" && bash -c "$step") >>"$log" 2>&1; then
    failed=$step
    break
  fi
done
took=$(($(date +%s) - start))

if [ -z "$failed" ]; then
  echo "nightly: ok in ${took}s"
  if [ "$report" != 0 ]; then
    state=$(gh api "repos/{owner}/{repo}/commits/$sha/status" --jq '[.statuses[] | select(.context == "local-ci")][0].state // ""')
    if [ -z "$state" ]; then
      (cd "$dir" && make --no-print-directory ci-local COMMIT="$sha" RAN="make nightly: $(printf '%s; ' "${steps[@]}" | sed 's/; $//')")
    else
      echo "nightly: $sha already has local-ci $state; nothing to attest"
    fi
  fi
  git worktree remove --force "$dir"
  exit 0
fi

echo "nightly: FAIL at '$failed' after ${took}s; the checkout stays at $dir"
tail -40 "$log"
if [ "$report" != 0 ]; then
  body=$(printf '`%s` failed on %s (`make nightly`, %ss).\n\nLast lines of the log (%s on %s):\n\n```\n%s\n```\n' \
    "$failed" "$sha" "$took" "$log" "$(hostname)" "$(tail -60 "$log")")
  issue=$(gh issue list --state open --search "in:title \"$title\"" --json number,title \
    --jq "[.[] | select(.title == \"$title\")][0].number // \"\"")
  if [ -n "$issue" ]; then
    gh issue comment "$issue" --body "$body"
  else
    gh issue create --title "$title" --body "$body"
  fi
fi
exit 1
