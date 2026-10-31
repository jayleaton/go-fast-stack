#!/usr/bin/env bash
# What a pull request needs before make ci-local (`make check-pr`): runs the
# checks docs/CI.md "What to run" lists for this checkout's changes against
# the merge base with origin/main, in order, stopping at the first failure,
# then prints the `make ci-local RAN="…"` line that says exactly what ran.
#
#   Go packages          make lint LINT_DIRS=<dirs>, make test TEST_PKGS=<pkgs>
#   generator inputs     make check-generated
#   deploy/, .env.example make deploy-check
#   workflows, actions   make actionlint
#   package.json, pnpm-lock.yaml   make vulncheck (go.mod and scripts/ run make ci, which has it)
#   a Dockerfile or what it builds   make release-smoke
#   a shared input (scripts/affected.sh: go.mod, tooling, migrations, sqlc)
#                        make ci instead of lint, test and check-generated
#   every change         make secret-scan
#
# scripts/affected.sh picks the packages and fails when its selection does
# not match the diff. apps/mobile-app has its own pnpm checks, which this
# names but does not run.
#
# Usage: scripts/check-pr.sh [--dry-run]   (CHECK_BASE=<ref> compares with another base)
set -euo pipefail
# shellcheck disable=SC2046 # one name per word
unset $(git rev-parse --local-env-vars)
cd "$(dirname "$0")/.."
. scripts/branches.sh

dry=false
case ${1:-} in
--dry-run) dry=true ;;
"") ;;
*) echo "usage: scripts/check-pr.sh [--dry-run]" >&2 && exit 2 ;;
esac

base_ref=${CHECK_BASE:-$(landing_ref)}
if [ -z "$base_ref" ] || ! base=$(git merge-base "$base_ref" HEAD 2>/dev/null); then
  echo "check-pr: no merge base with ${base_ref:-$(deployed_names)} (git fetch origin, or set CHECK_BASE)" >&2
  exit 2
fi

# Committed, staged and unstaged changes, then untracked files: the checks
# run on the working tree, so they cover what it holds.
mapfile -t changed < <({
  git diff --name-only --no-renames "$base" --
  git ls-files --others --exclude-standard
} | sort -u)
if [ "${#changed[@]}" -eq 0 ]; then
  echo "check-pr: nothing differs from $base_ref"
  exit 0
fi

if ! aff=$(AFFECTED_FILES="${changed[*]}" ./scripts/affected.sh); then
  echo "check-pr: scripts/affected.sh failed; nothing ran" >&2
  exit 2
fi
full=false dirs="" packages="" sqlc=false templ=false generate="" smoke=false
while IFS='=' read -r k v; do
  case $k in
  full) full=$v ;; dirs) dirs=$v ;; packages) packages=$v ;; sqlc) sqlc=$v ;;
  templ) templ=$v ;; generate) generate=$v ;; smoke) smoke=$v ;;
  esac
done <<<"$aff"

deploy=false workflows=false mobile=false deps=false
for f in "${changed[@]}"; do
  case $f in
  deploy/* | .env.example) deploy=true ;;
  .github/workflows/* | .github/actions/*) workflows=true ;;
  package.json | */package.json | pnpm-lock.yaml | */pnpm-lock.yaml) deps=true ;;
  apps/mobile-app/* | apps/web/internal/config/routes.go) mobile=true ;;
  esac
done

plan=()
if [ "$full" = true ]; then
  plan+=("make ci") # deploy-check included
else
  if [ -n "$packages" ]; then
    plan+=("make lint LINT_DIRS='$dirs'" "make test TEST_PKGS='$packages'")
  fi
  if [ "$sqlc" = true ] || [ "$templ" = true ] || [ -n "$generate" ]; then plan+=("make check-generated"); fi
  if [ "$deploy" = true ]; then plan+=("make deploy-check"); fi
  if [ "$deps" = true ]; then plan+=("make vulncheck"); fi
fi
if [ "$workflows" = true ]; then plan+=("make actionlint"); fi
if [ "$smoke" = true ]; then plan+=("make release-smoke"); fi
plan+=("make secret-scan")

echo "check-pr: ${#changed[@]} changed files vs $base_ref$([ "$full" = true ] && echo ', a shared input: everything')"
printf '  %s\n' "${plan[@]}"
if [ "$mobile" = true ]; then
  echo "  (not run here: apps/mobile-app's pnpm checks, docs/CI.md \"What to run\")"
fi
[ "$dry" = false ] || exit 0

ran=()
for step in "${plan[@]}"; do
  echo "== $step"
  if ! bash -c "$step"; then
    echo "check-pr: FAIL: $step"
    echo "fix it and run make check-pr again; never attest a failing head"
    exit 1
  fi
  ran+=("$step")
done

statement=$(printf '%s; ' "${ran[@]}")
statement=${statement%; }
statement=${statement//\'/}
echo "check-pr: ok. Commit and push, then attest:"
echo "  make ci-local RAN=\"$statement\""
