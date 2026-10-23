#!/usr/bin/env bash
# Browsers only ever talk to the UI (AGENTS.md "Architecture"): a page or a
# Datastar attribute never links to, fetches or posts to the JSON API.
# Forms and Datastar actions post to the UI's own action routes, which call
# the API in-process (apiclient.Client). This fails on any API path in what
# the UIs render or serve: templates, their generated Go, UI Go code and
# static assets. forbidigo separately rejects config.APIRoutes in UI code.
#
#   scripts/check-ui-api-paths.sh            # the UIs
#   scripts/check-ui-api-paths.sh <dir>...   # other dirs (the probe)
set -euo pipefail
cd "$(dirname "$0")/.."

# apiPrefix is the JSON API subtree (server.apiPrefix, config.APIRoutes).
apiPrefix=/api/

dirs=("$@")
[ "${#dirs[@]}" -gt 0 ] || dirs=(apps/web/ui apps/admin/ui)
existing=()
for dir in "${dirs[@]}"; do [ -d "$dir" ] && existing+=("$dir"); done
[ "${#existing[@]}" -gt 0 ] || exit 0

# The preview command is wiring (it mounts the real API in-process), not UI.
if hits=$(grep -rnF "$apiPrefix" "${existing[@]}" \
  --include='*.templ' --include='*.go' --include='*.js' --include='*.css' --include='*.html' \
  --exclude-dir=cmd); then
  echo "$hits"
  echo "UI renders or serves an API path ($apiPrefix): post to a UI action route that calls apiclient.Client instead (AGENTS.md \"Architecture\")"
  exit 1
fi
