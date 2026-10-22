#!/usr/bin/env bash
# Fails if any _test.go (tracked or untracked, not ignored) lives outside the
# test boundaries below. Boundaries are directories, not trees: a test file's
# own directory must be one of them. See AGENTS.md "Test boundaries".
set -euo pipefail
cd "$(dirname "$0")/.."

boundaries=(
  apps/web/internal/services # service unit tests: generated port mocks only
  apps/web/internal/api      # route handler tests: mock service + mock session, real router
  apps/web/ui                # ui handler tests: same shape as api
  packages/database          # repository integration tests: real Postgres
  packages/auth              # auth's own self-contained suite
)

# Files, or directories when a whole non-handler package is allowed.
allow=(
  apps/web/internal/config/config_test.go # boot-time env validation; nothing above it to test through
  apps/web/internal/server/server_test.go # wiring: mounts, middleware order, CSRF
  apps/web/internal/server/auth_test.go   # wiring: auth adapters against real packages/auth + Postgres
  apps/web/ui/i18n                        # dir: every message key exists in every locale
  apps/web/ui/static                      # dir: pinned Datastar bundle hash
)

offenders=()
while IFS= read -r f; do
  [ -e "$f" ] || continue # deleted in the working tree
  dir=$(dirname "$f")
  for b in "${boundaries[@]}" "${allow[@]}"; do
    if [ "$dir" = "$b" ] || [ "$f" = "$b" ]; then continue 2; fi
  done
  offenders+=("$f")
done < <(git ls-files --cached --others --exclude-standard -- '*_test.go' | sort -u)

if [ "${#offenders[@]}" -gt 0 ]; then
  echo 'test files outside the test boundaries:'
  printf '  %s\n' "${offenders[@]}"
  echo 'test through a boundary package instead (see AGENTS.md "Test boundaries").'
  exit 1
fi
