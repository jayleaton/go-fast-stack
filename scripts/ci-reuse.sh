#!/usr/bin/env bash
# Decides whether a CI run can skip its checks (run by the ci job, see
# .github/workflows/ci.yml "Reuse or skip"):
#   push:     the pushed commit's tree is identical to the merge commit a PR's
#             CI tested and passed. Every condition must hold: the PR's latest
#             ci-ok status on its head is a success, posted by
#             github-actions[bot], recording exactly this tree
#             ("success tree=<sha>"). Then ci-ok success is posted on the
#             pushed commit with a pointer to that result.
#             Also skipped when the pushed commit itself already has that
#             success (dev or main fast-forwarded to a merge-queue commit CI tested).
#   schedule: the build cache for this commit already exists.
# Anything else, or any doubt: skip=false and the run checks everything.
# Env: EVENT, MESSAGE (head commit message), GH_TOKEN, GITHUB_REPOSITORY,
#      GITHUB_SHA, GITHUB_SERVER_URL, GITHUB_RUN_ID, RUNNER_OS
# Prints (and appends to $GITHUB_OUTPUT when set): skip=true|false
set -euo pipefail
cd "$(dirname "$0")/.."

out() {
  echo "skip=$1"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "skip=$1" >>"$GITHUB_OUTPUT"; fi
  exit 0
}
repo=$GITHUB_REPOSITORY sha=$GITHUB_SHA

case ${EVENT:-} in
schedule)
  key="v7-go-${RUNNER_OS:-Linux}-$sha"
  if [ "$(gh cache list --key "$key" --json key --jq "[.[] | select(.key == \"$key\")] | length" 2>/dev/null || echo 0)" != 0 ]; then
    echo "cache $key already saved: nothing to refresh"
    out true
  fi
  out false
  ;;
push) ;;
*) out false ;;
esac

tree=$(git rev-parse "$sha^{tree}")

# The commit itself already passed (e.g. a branch fast-forwarded to a merge-queue
# commit that CI tested): nothing to do.
self=$(gh api "repos/$repo/commits/$sha/statuses" \
  --jq '[.[] | select(.context == "ci-ok")][0] | "\(.state)|\(.creator.login)|\(.description)"' 2>/dev/null || true)
if [ "$self" = "success|github-actions[bot]|success tree=$tree" ]; then
  echo "commit ${sha:0:7} already has ci-ok success for tree $tree"
  out true
fi
# PRs this commit came from: GitHub's association (squash/merge/rebase via the
# PR), plus "(#N)" in the first line, which GitHub adds to squash-merge titles.
prs=$(
  {
    gh api "repos/$repo/commits/$sha/pulls" --jq '.[].number' 2>/dev/null || true
    head -n1 <<<"${MESSAGE:-}" | grep -oE '\(#[0-9]+\)' | tr -dc '0-9\n' || true
  } | sort -un
)
for pr in $prs; do
  head=$(gh api "repos/$repo/pulls/$pr" --jq .head.sha 2>/dev/null) || continue
  # Newest first; only the latest ci-ok counts.
  st=$(gh api "repos/$repo/commits/$head/statuses" \
    --jq '[.[] | select(.context == "ci-ok")][0] | "\(.state)|\(.creator.login)|\(.description)|\(.target_url)"' 2>/dev/null) || continue
  IFS='|' read -r state creator desc url <<<"$st"
  echo "PR #$pr head ${head:0:7}: ci-ok=$state by $creator ($desc)"
  if [ "$state" = success ] && [ "$creator" = "github-actions[bot]" ] && [ "$desc" = "success tree=$tree" ]; then
    gh api "repos/$repo/statuses/$sha" -f state=success -f context=ci-ok \
      -f description="reused PR #$pr (tree ${tree:0:12})" -f target_url="$url" >/dev/null
    echo "tree $tree passed CI on PR #$pr: reusing that result"
    out true
  fi
done
echo "no PR result for tree $tree: running the checks"
out false
