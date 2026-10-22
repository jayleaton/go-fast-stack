#!/usr/bin/env bash
# Proves the depguard layering rules in .golangci.yml still bite: drops a probe
# package with a forbidden import into each guarded layer, and an external
# _test.go file with a forbidden import into each test boundary, runs
# golangci-lint, and fails unless depguard reports every probe. Probes are
# always removed.
# GOLANGCI_LINT overrides the binary (CI uses the prebuilt one).
set -euo pipefail
cd "$(dirname "$0")/.."

lint=${GOLANGCI_LINT:-go tool -modfile=tools/go.mod golangci-lint}
mod=github.com/jayleaton/ai-patterns-go
probe=zz_layering_probe

# Production code. layer dir | forbidden import
cases=(
  "apps/web/internal/api|github.com/jackc/pgx/v5"
  "apps/web/internal/api|$mod/packages/database"
  "apps/web/internal/api|$mod/packages/database/sqlc"
  "apps/web/ui|$mod/packages/database"
  "apps/web/internal/services|net/http"
  "apps/web/internal/services|$mod/packages/database/sqlc"
  "packages/auth|$mod/packages/database"
  "apps/web/internal/api|go.uber.org/mock/gomock"
  "apps/web/internal/services|$mod/packages/secrets"
  "apps/web/internal/api|$mod/packages/secrets"
  "packages/secrets|$mod/packages/auth"
)

# Test files. boundary dir | forbidden import
test_cases=(
  "apps/web/internal/services|github.com/jackc/pgx/v5"
  "apps/web/internal/services|$mod/packages/database/model"
  "apps/web/internal/services|$mod/packages/auth"
  "apps/web/internal/api|github.com/jackc/pgx/v5"
  "apps/web/internal/api|$mod/apps/web/internal/services/mocks"
  "packages/database|go.uber.org/mock/gomock"
  "packages/secrets|go.uber.org/mock/gomock"
)

paths=()
cleanup() { for p in "${paths[@]}"; do rm -rf "$p"; done; rmdir apps/web/ui 2>/dev/null || true; }
trap cleanup EXIT

# label | probe file | package to lint, one per planted probe
planted=()
for i in "${!cases[@]}"; do
  layer=${cases[$i]%%|*} imp=${cases[$i]#*|}
  label="$layer must not import $imp"
  dir="$layer/$probe$i"
  mkdir -p "$dir"
  paths+=("$dir")
  printf 'package %s\n\nimport _ "%s"\n' "$probe$i" "$imp" > "$dir/probe.go"
  planted+=("$label|$dir/probe.go|./$dir/")
done
for i in "${!test_cases[@]}"; do
  dir=${test_cases[$i]%%|*} imp=${test_cases[$i]#*|}
  label="$dir tests must not import $imp"
  file="$dir/${probe}${i}_test.go"
  paths+=("$file")
  printf 'package %s_test\n\nimport _ "%s"\n' "$(basename "$dir")" "$imp" > "$file"
  planted+=("$label|$file|./$dir/")
done

pkgs=()
for p in "${planted[@]}"; do pkgs+=("${p##*|}"); done
out=$($lint run --enable-only depguard --max-issues-per-linter 0 --max-same-issues 0 \
  $(printf '%s\n' "${pkgs[@]}" | sort -u) 2>&1 || true)

failed=0
for p in "${planted[@]}"; do
  label=${p%%|*} rest=${p#*|}
  if grep -q "${rest%%|*}:.*(depguard)" <<<"$out"; then
    echo "ok    $label"
  else
    echo "FAIL  $label (not reported by depguard)"
    failed=1
  fi
done
if [ "$failed" = 1 ]; then
  echo "--- golangci-lint output:"; echo "$out"
  exit 1
fi
