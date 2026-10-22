#!/usr/bin/env bash
# Proves the depguard layering rules in .golangci.yml still bite: drops a probe
# package with a forbidden import into each guarded layer, runs golangci-lint,
# and fails unless depguard reports every probe. Probes are always removed.
# GOLANGCI_LINT overrides the binary (CI uses the prebuilt one).
set -euo pipefail
cd "$(dirname "$0")/.."

lint=${GOLANGCI_LINT:-go tool -modfile=tools/go.mod golangci-lint}
mod=github.com/jayleaton/ai-patterns-go
probe=zz_layering_probe

# layer dir | forbidden import
cases=(
  "apps/web/internal/api|github.com/jackc/pgx/v5"
  "apps/web/internal/api|$mod/packages/database"
  "apps/web/internal/api|$mod/packages/database/sqlc"
  "apps/web/ui|$mod/packages/database"
  "apps/web/internal/services|net/http"
  "apps/web/internal/services|$mod/packages/database/sqlc"
  "packages/auth|$mod/packages/database"
)

dirs=()
cleanup() { for d in "${dirs[@]}"; do rm -rf "$d"; done; rmdir apps/web/ui 2>/dev/null || true; }
trap cleanup EXIT

for i in "${!cases[@]}"; do
  dir="${cases[$i]%%|*}/$probe$i"
  mkdir -p "$dir"
  dirs+=("$dir")
  printf 'package %s\n\nimport _ "%s"\n' "$probe$i" "${cases[$i]#*|}" > "$dir/probe.go"
done

out=$($lint run --enable-only depguard --max-issues-per-linter 0 --max-same-issues 0 \
  $(printf './%s/ ' "${dirs[@]}") 2>&1 || true)

failed=0
for i in "${!cases[@]}"; do
  if grep -q "${dirs[$i]}/probe.go.*(depguard)" <<<"$out"; then
    echo "ok    ${cases[$i]/|/ must not import }"
  else
    echo "FAIL  ${cases[$i]/|/ must not import } (not reported by depguard)"
    failed=1
  fi
done
if [ "$failed" = 1 ]; then
  echo "--- golangci-lint output:"; echo "$out"
  exit 1
fi
