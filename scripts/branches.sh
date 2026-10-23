# The long-lived branches that deploy, staging first: dev deploys staging and
# main deploys production (AGENTS.md "Branches and releases"). Either may be
# missing (a fresh clone of the template has only main): everything here uses
# the ones that exist. Sourced by the scripts that need a base (check-fast,
# affected, check-migration-order, ci-check); the CI triggers in
# .github/workflows/ci.yml list the same names.
DEPLOY_BRANCHES=(dev main)

# deployed_refs prints origin/<branch> for each deployed branch this clone
# has fetched, staging first.
deployed_refs() {
  local branch
  for branch in "${DEPLOY_BRANCHES[@]}"; do
    if git rev-parse --verify --quiet "refs/remotes/origin/$branch" >/dev/null; then echo "origin/$branch"; fi
  done
}

# deployed_names prints "origin/dev origin/main" (for messages).
deployed_names() { printf 'origin/%s ' "${DEPLOY_BRANCHES[@]}" | sed 's/ $//'; }

# landing_ref prints the ref work lands on and is compared with: origin/dev
# when it exists, else origin/main. Empty when neither has been fetched.
landing_ref() { deployed_refs | head -n 1; }
