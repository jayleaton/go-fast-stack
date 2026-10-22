#!/usr/bin/env bash
# Decides whether CI checks the affected packages in one job or fans out to
# two parallel jobs (lint + DB-backed tests | other tests). Every job bills
# rounded up to a whole minute, so one job is cheaper whenever it finishes in
# about a minute; fan out only when it would not.
# Usage: FULL=true|false PACKAGES="<import paths>" scripts/ci-plan.sh
# Prints (and appends to $GITHUB_OUTPUT when set):
#   fanout=true|false   tested=<n>   (n = affected packages that have tests)
set -euo pipefail
cd "$(dirname "$0")/.."

# Fan out above this many affected packages with tests. Measured single-job
# times (warm caches): 1 tested package (web) 36-52 s, 4 (admin) 67-76 s;
# fanning out adds the planning job (~12 s + scheduling) in front of two
# ~45-68 s jobs, so it only pays off for sets whose single job would clearly
# pass ~90 s, e.g. full runs (16 tested packages).
threshold=${CI_FANOUT_TESTED:-8}

mod=$(go list -m)
tested=0
read -r -a pkgs <<<"${PACKAGES:-}"
for p in "${pkgs[@]}"; do
  d=${p/#$mod/.}
  if compgen -G "$d/*_test.go" >/dev/null; then tested=$((tested + 1)); fi
done

fanout=false
if [ "${FULL:-false}" = true ] || [ "$tested" -gt "$threshold" ]; then fanout=true; fi

out() {
  printf '%s=%s\n' "$1" "$2"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then printf '%s=%s\n' "$1" "$2" >>"$GITHUB_OUTPUT"; fi
}
out fanout "$fanout"
out tested "$tested"
