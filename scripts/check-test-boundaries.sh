#!/usr/bin/env bash
# Fails if any _test.go (tracked or untracked, not ignored) lives outside the
# test boundaries below. A boundary is a directory (the test file's own
# directory must be it). See AGENTS.md "Test boundaries".
set -euo pipefail
cd "$(dirname "$0")/.."

boundaries=(
  apps/web/internal/services   # business logic: service unit tests with generated port mocks
  apps/web/internal/api        # route handlers: mock service + mock session, real router
  apps/admin/internal/services # admin business logic, same rules
  apps/admin/internal/api      # admin route handlers, same rules
  packages/auth                # backend auth: its own suite against real Postgres
  scripts/cilocal/verify       # local verification's rules: selection, reuse keys, refusals (pure)
)

# No other tests: no UI, config, wiring, repository, secrets or deploy tests.
# Repositories are exercised for real by migrations and the app; services see
# them only through mocked ports.
allow=()

offenders=()
while IFS= read -r f; do
  [ -e "$f" ] || continue # deleted in the working tree
  dir=$(dirname "$f")
  for b in "${boundaries[@]}" ${allow[@]+"${allow[@]}"}; do
    case $b in
    */\*\*) [ "$dir" = "${b%/\*\*}" ] || [[ $dir == "${b%\*\*}"* ]] && continue 2 ;;
    *) [ "$dir" = "$b" ] || [ "$f" = "$b" ] && continue 2 ;;
    esac
  done
  offenders+=("$f")
done < <(git ls-files --cached --others --exclude-standard -- '*_test.go' | sort -u)

if [ "${#offenders[@]}" -gt 0 ]; then
  echo 'test files outside the test boundaries:'
  printf '  %s\n' "${offenders[@]}"
  echo 'test through a boundary package instead (see AGENTS.md "Test boundaries").'
  exit 1
fi
