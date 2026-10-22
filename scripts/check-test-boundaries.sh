#!/usr/bin/env bash
# Fails if any _test.go (tracked or untracked, not ignored) lives outside the
# test boundaries below. A boundary is a directory (the test file's own
# directory must be it) or, ending in /**, a tree. See AGENTS.md "Test boundaries".
set -euo pipefail
cd "$(dirname "$0")/.."

boundaries=(
  apps/web/internal/services # service unit tests: generated port mocks only
  apps/web/internal/api      # route handler tests: mock service + mock session, real router
  apps/web/ui/**             # ui handler tests (same shape as api), plus the i18n key guard, render tests and the static/Datastar hash pin
  packages/database          # repository integration tests: real Postgres
  packages/auth              # auth's own self-contained suite
  packages/secrets           # secrets manager + providers: fake providers over httptest, synctest clock
  # apps/admin: the same boundaries, same rules.
  apps/admin/internal/services # admin service unit tests: generated port mocks only
  apps/admin/internal/api      # admin route handler tests: mock service + mock session, real router
  apps/admin/ui/**             # admin ui handler tests, i18n key guard, render tests, Datastar hash pin
  packages/database/adminrepo  # admin repository integration tests: real Postgres (app + admin databases)
)

# Single files with no boundary above them.
allow=(
  apps/web/internal/config/config_test.go # boot-time env validation; nothing above it to test through
  apps/web/internal/server/server_test.go # wiring: mounts, middleware order, CSRF
  apps/web/internal/server/auth_test.go   # wiring: auth adapters against real packages/auth + Postgres
  apps/admin/internal/config/config_test.go # the admin app's boot-time env validation
  apps/admin/internal/server/server_test.go # admin wiring: mounts, middleware order, CSRF, no sign-up
  apps/admin/internal/server/auth_test.go   # admin wiring: adapters against real packages/auth on two databases
)

offenders=()
while IFS= read -r f; do
  [ -e "$f" ] || continue # deleted in the working tree
  dir=$(dirname "$f")
  for b in "${boundaries[@]}" "${allow[@]}"; do
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
