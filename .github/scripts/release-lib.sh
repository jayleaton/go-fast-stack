#!/bin/sh
# Shared checks for "Deploy production" and "Draft release" (docs/RELEASES.md).
# Source it; it only reads GitHub (gh) and staging's /healthz.
#
# In a workflow it needs GH_TOKEN, GITHUB_REPOSITORY, and the repository
# variables passed in as RELEASERS (PRODUCTION_RELEASERS) and STAGING_WEB_URL.
# On a laptop, signed in with `gh auth login`, the repository and both
# variables are read with gh when unset (reading variables needs write access
# to the repository):
#
#   . .github/scripts/release-lib.sh
#   staging_commit; staging_ready; is_releaser "$(gh api user -q .login)" && echo yes

if [ -z "${GITHUB_ACTIONS:-}" ]; then
  : "${GITHUB_REPOSITORY:=$(gh repo view --json nameWithOwner -q .nameWithOwner)}"
  : "${RELEASERS=$(gh variable get PRODUCTION_RELEASERS -R "$GITHUB_REPOSITORY" 2>/dev/null || true)}"
  : "${STAGING_WEB_URL=$(gh variable get STAGING_WEB_URL -R "$GITHUB_REPOSITORY" 2>/dev/null || true)}"
fi

STAGING_WORKFLOW=deploy-staging.yml
STAGING_JOB='Deploy main to Railway staging'
STAGING_READY=${STAGING_WEB_URL:+${STAGING_WEB_URL%/}/healthz}

# is_releaser LOGIN: LOGIN is listed in PRODUCTION_RELEASERS (the repository
# variable, passed in as RELEASERS; logins separated by commas or spaces).
is_releaser() {
  [ -n "${RELEASERS:-}" ] && echo "$RELEASERS" | tr ',' ' ' | tr -s ' ' '\n' | grep -qxF "$1"
}

# staging_run_ok RUN: a successful "Deploy staging" run proves staging ran a
# commit only when its deploy job (which ends with the readiness check)
# succeeded. The run title names the deployed commit. A dry run (RAILWAY_DEPLOY
# not true) skips that job, so it proves nothing.
staging_run_ok() {
  [ "$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$1/jobs" \
    -q "[.jobs[] | select(.name == \"$STAGING_JOB\" and .conclusion == \"success\")] | length" 2>/dev/null)" = 1 ]
}

# staging_ran SHA: prints the id of a run that deployed SHA to staging.
staging_ran() {
  for run in $(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$STAGING_WORKFLOW/runs?status=success&head_sha=$1&per_page=20" \
    -q ".workflow_runs[] | select(.display_title == \"Deploy staging $1\") | .id" 2>/dev/null); do
    if staging_run_ok "$run"; then
      echo "$run"
      return 0
    fi
  done
  return 1
}

# staging_commit: prints the commit staging runs now (its latest proven deploy).
staging_commit() {
  for line in $(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$STAGING_WORKFLOW/runs?status=success&per_page=20" \
    -q '.workflow_runs[] | "\(.id):\(.display_title | ltrimstr("Deploy staging "))"' 2>/dev/null); do
    if staging_run_ok "${line%%:*}"; then
      echo "${line#*:}"
      return 0
    fi
  done
  return 1
}

# staging_ready: prints the HTTP status of staging's /healthz (000 when it
# does not answer), or "unset" when STAGING_WEB_URL is empty.
staging_ready() {
  if [ -z "$STAGING_READY" ]; then
    echo unset
    return 0
  fi
  curl -s -o /dev/null -m 20 -w '%{http_code}' "$STAGING_READY" || true
}

# staging_not_ready CODE: why staging_ready's CODE is not a pass.
staging_not_ready() {
  if [ "$1" = unset ]; then
    echo "The STAGING_WEB_URL repository variable is not set, so staging's /healthz cannot be checked."
  else
    echo "Staging is not ready ($STAGING_READY answered $1)."
  fi
}
