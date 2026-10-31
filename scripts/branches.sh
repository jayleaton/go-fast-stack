# The long-lived branches that deploy: main, which Deploy staging ships to
# staging on every attested merge; production runs releases cut from it
# (AGENTS.md "Branches and releases", docs/RELEASES.md). Everything here uses
# the ones this clone has fetched. Sourced by the scripts that need a base
# (check-fast, affected, the bench A/B); scripts/cilocal names the same.
DEPLOY_BRANCHES=(main)

# deployed_refs prints origin/<branch> for each deployed branch this clone
# has fetched, staging first.
deployed_refs() {
  local branch
  for branch in "${DEPLOY_BRANCHES[@]}"; do
    if git rev-parse --verify --quiet "refs/remotes/origin/$branch" >/dev/null; then echo "origin/$branch"; fi
  done
}

# deployed_names prints "origin/main" (for messages).
deployed_names() { printf 'origin/%s ' "${DEPLOY_BRANCHES[@]}" | sed 's/ $//'; }

# landing_ref prints the ref work lands on and is compared with: origin/main.
# Empty when it has not been fetched.
landing_ref() { deployed_refs | head -n 1; }
