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

# Affected packages with tests above which a single job is expected to pass
# ~60 s. See the measurements in .github/workflows/ci.yml ("Fan-out").
threshold=${CI_FANOUT_TESTED:-12}

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
