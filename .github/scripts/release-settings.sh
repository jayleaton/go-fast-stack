#!/bin/sh
# One-time setup, run by hand (never by a workflow): applies the repository
# settings that keep the Railway tokens away from pushed workflows
# (docs/RELEASES.md): the staging and production deployment environments and
# the refs each admits, and the tag ruleset that lets only administrators
# create, move or delete a v* tag. production-gate.sh refuses every release
# until these hold. Idempotent; run it with an administrator's gh login. It
# never reads or writes a secret: add each environment's
# RAILWAY_TOKEN secret yourself.
#
#   sh .github/scripts/release-settings.sh [owner/repo]   # default: this checkout's GitHub repo
set -eu
repo=${1:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}
R=repos/$repo

# environment NAME REF...: NAME admits exactly the given refs (branch:main, tag:v*).
environment() {
  name=$1
  shift
  gh api -X PUT "$R/environments/$name" --silent --input - <<'JSON'
{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}
JSON
  existing=$(gh api "$R/environments/$name/deployment-branch-policies" -q '.branch_policies[] | "\(.id) \(.type):\(.name)"')
  echo "$existing" | while read -r id ref; do
    [ -n "$id" ] || continue
    case " $* " in *" $ref "*) ;; *) gh api -X DELETE "$R/environments/$name/deployment-branch-policies/$id" --silent ;; esac
  done
  for ref in "$@"; do
    echo "$existing" | awk -v ref="$ref" '$2 == ref { found = 1 } END { exit !found }' && continue
    gh api -X POST "$R/environments/$name/deployment-branch-policies" -f "type=${ref%%:*}" -f "name=${ref#*:}" --silent
  done
  echo "$name admits: $(gh api "$R/environments/$name/deployment-branch-policies" -q '[.branch_policies[] | "\(.type):\(.name)"] | sort | join(" ")')"
}

environment production branch:main tag:v*
environment staging branch:main

ruleset='{
  "name": "Release tags",
  "target": "tag",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["refs/tags/v*"], "exclude": [] } },
  "rules": [{ "type": "creation" }, { "type": "update" }, { "type": "deletion" }, { "type": "non_fast_forward" }],
  "bypass_actors": [{ "actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always" }]
}'
id=$(gh api "$R/rulesets" -q '.[] | select(.name == "Release tags") | .id')
if [ -n "$id" ]; then
  echo "$ruleset" | gh api -X PUT "$R/rulesets/$id" --silent --input -
else
  echo "$ruleset" | gh api -X POST "$R/rulesets" --silent --input -
fi
echo "Tag ruleset 'Release tags' protects refs/tags/v* in $repo (bypass: the Admin role)."
