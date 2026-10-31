#!/bin/sh
# Decides whether a release may go to production (docs/RELEASES.md). Every
# check runs and every failure is reported, then the script exits 1 if any
# failed, so the person releasing sees the whole picture. Anything it cannot
# prove counts as a failure (fail closed). It reads GitHub and staging's
# /healthz only; it never deploys.
#
#   TAG          release tag, vX.Y.Z
#   SHA          commit the tag points at (resolved by the workflow)
#   ACTOR        GitHub user who published or dispatched
#   RELEASERS    PRODUCTION_RELEASERS repo variable: logins allowed to release,
#                separated by commas or spaces
#   STAGING_WEB_URL  staging's web origin (repo variable)
#   GH_TOKEN, GITHUB_REPOSITORY
#
# Needs a checkout with the full history of origin/main. Read-only from a
# laptop signed in with gh (RELEASERS, STAGING_WEB_URL and the repository are
# read with gh when unset; see release-lib.sh):
#
#   git fetch origin main --tags
#   TAG=v1.2.3 SHA=$(git rev-parse 'v1.2.3^{commit}') ACTOR=$(gh api user -q .login) \
#     sh .github/scripts/production-gate.sh
set -u

: "${TAG:?set TAG (vX.Y.Z)}" "${SHA:?set SHA (the commit TAG points at)}" "${ACTOR:?set ACTOR (a GitHub login)}"

# shellcheck source=.github/scripts/release-lib.sh
. "$(dirname "$0")/release-lib.sh"
R=$GITHUB_REPOSITORY

failures=0
report() {
  echo "$1"
  [ -n "${GITHUB_STEP_SUMMARY:-}" ] && echo "- $1" >>"$GITHUB_STEP_SUMMARY"
  return 0
}
pass() { report "PASS: $1"; }
fail() {
  report "FAIL: $1"
  # The annotation repeats the line; only Actions shows it.
  [ -n "${GITHUB_ACTIONS:-}" ] && echo "::error::$1"
  failures=$((failures + 1))
}

[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '## Production gate for %s (%s)\n\n' "$TAG" "$SHA" >>"$GITHUB_STEP_SUMMARY"

# 1. The deploy environments must exist and admit only the refs that may
# deploy: production main and v* tags, staging main. GitHub creates a missing
# environment on first use with no restrictions, so without this check the
# first deploy would run in an unconfigured one.
check_environment() {
  environment=$1
  want=$2
  if env_json=$(gh api "repos/$R/environments/$environment" 2>/dev/null) &&
    policies=$(gh api "repos/$R/environments/$environment/deployment-branch-policies" \
      -q '[.branch_policies[] | "\(.type):\(.name)"] | sort | join(" ")' 2>/dev/null); then
    if [ "$(echo "$env_json" | jq -r '.deployment_branch_policy.custom_branch_policies // false')" = true ] &&
      [ "$policies" = "$want" ]; then
      pass "The $environment environment admits only $want"
    else
      fail "The $environment environment must admit only $want (found: ${policies:-none}). Run sh .github/scripts/release-settings.sh (docs/RELEASES.md, one-time setup)."
    fi
  else
    fail "The $environment environment does not exist or cannot be read. Run sh .github/scripts/release-settings.sh (docs/RELEASES.md, one-time setup)."
  fi
}
check_environment production "branch:main tag:v*"
check_environment staging "branch:main"

# 1b. Only releasers may create, move or delete a v* tag. The production
# environment admits v* tags, and a tag runs the workflow file at its own
# commit, so without this ruleset anyone who can push could tag a commit whose
# workflow skips this gate and read the production token.
protected=false
for id in $(gh api "repos/$R/rulesets?includes_parents=false" -q '.[] | select(.target == "tag" and .enforcement == "active") | .id' 2>/dev/null); do
  if gh api "repos/$R/rulesets/$id" -q '[.conditions.ref_name.include[]] | index("refs/tags/v*") != null' 2>/dev/null | grep -qx true &&
    [ "$(gh api "repos/$R/rulesets/$id" -q '[.rules[].type | select(. == "creation" or . == "update" or . == "deletion")] | unique | length' 2>/dev/null)" = 3 ]; then
    protected=true
  fi
done
if [ "$protected" = true ]; then
  pass "An active tag ruleset restricts creating, moving and deleting v* tags"
else
  fail "No active tag ruleset restricts creating, moving and deleting v* tags. Run sh .github/scripts/release-settings.sh (docs/RELEASES.md, one-time setup)."
fi

# 2. Only allowed people release.
if [ -z "${RELEASERS:-}" ]; then
  fail "The PRODUCTION_RELEASERS repository variable is not set."
elif is_releaser "$ACTOR"; then
  pass "$ACTOR may release"
else
  fail "$ACTOR is not in PRODUCTION_RELEASERS."
fi

# 3. A published, final release for a vX.Y.Z tag.
if echo "$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  state=$(gh release view "$TAG" -R "$R" --json isDraft,isPrerelease -q '"\(.isDraft) \(.isPrerelease)"' 2>/dev/null || echo missing)
  case $state in
    "false false") pass "$TAG is a published release" ;;
    missing) fail "No release exists for $TAG." ;;
    *) fail "$TAG is a draft or a pre-release (draft, pre-release: $state)." ;;
  esac
else
  fail "$TAG is not a vX.Y.Z tag."
fi

# 4. The commit is on main: no branch builds, no hotfixes past staging.
if git merge-base --is-ancestor "$SHA" origin/main 2>/dev/null; then
  pass "$SHA is on main"
else
  fail "$SHA is not on main. Production only takes commits merged to main."
fi

# 5. Staging ran this exact commit and passed its readiness check. A dry run
# (RAILWAY_DEPLOY not true) deploys nothing, so it does not count.
proved=$(staging_ran "$SHA")
if [ -n "$proved" ]; then
  pass "Staging ran $SHA and passed readiness (run $proved)"
else
  fail "No successful Deploy staging run, with its \"$STAGING_JOB\" job succeeded, deployed $SHA. Production only takes a commit staging has run."
fi

# 6. Staging still runs this commit or a later main commit, so production never
# leads staging.
current=$(staging_commit)
if [ -z "$current" ]; then
  fail "Cannot tell which commit staging runs: no successful Deploy staging run whose \"$STAGING_JOB\" job succeeded."
elif git merge-base --is-ancestor "$SHA" "$current" 2>/dev/null; then
  pass "Staging runs $current, which includes $SHA"
else
  fail "Staging runs $current, which does not include $SHA. Production may lag staging, never lead it."
fi

# 7. Staging is healthy now.
code=$(staging_ready)
if [ "$code" = 200 ]; then
  pass "Staging is ready ($STAGING_READY answered 200)"
else
  fail "$(staging_not_ready "$code")"
fi

if [ "$failures" -gt 0 ]; then
  echo "${GITHUB_ACTIONS:+::error::}Production gate refused $TAG: $failures check(s) failed."
  exit 1
fi
echo "Production gate passed for $TAG ($SHA)."
