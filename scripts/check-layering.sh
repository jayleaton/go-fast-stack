#!/usr/bin/env bash
# Proves the depguard layering rules in .golangci.yml still bite: drops a probe
# package with a forbidden import into each guarded layer, and an external
# _test.go file with a forbidden import into each test boundary, runs
# golangci-lint, and fails unless depguard reports every probe. Then plants
# code that must not compile (reading a secret around reveal.Value) and fails
# unless the compiler rejects it. Probes are always removed.
# GOLANGCI_LINT overrides the binary (CI uses the prebuilt one).
set -euo pipefail
cd "$(dirname "$0")/.."

lint=${GOLANGCI_LINT:-go tool -modfile=tools/go.mod golangci-lint}
mod=github.com/jayleaton/go-fast-stack
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
  # Admin isolation, both ways.
  "apps/web/internal/server|$mod/packages/database/adminrepo"
  "apps/web/internal/services|$mod/packages/database/adminrepo/model"
  "apps/web/ui|$mod/apps/admin/ui"
  "packages/database|$mod/packages/database/adminrepo"
  "apps/admin/internal/server|$mod/apps/web/ui"
  "apps/admin/ui|$mod/apps/web/ui/view"
  # Admin layering.
  "apps/admin/internal/api|github.com/jackc/pgx/v5"
  "apps/admin/internal/api|$mod/packages/auth"
  "apps/admin/ui|$mod/packages/database/adminrepo"
  "apps/admin/internal/services|net/http"
  "apps/admin/internal/services|$mod/packages/database/adminrepo/adminsql"
  "apps/admin/internal/services|$mod/packages/auth"
  "apps/admin/internal/api|go.uber.org/mock/gomock"
  # Secrets never reach templates, SSE or JSON; the UI never uses the JSON API.
  "apps/web/ui|$mod/packages/secrets"
  "apps/admin/ui|$mod/packages/secrets"
  "apps/admin/internal/api|$mod/packages/secrets"
  # A secret's value is read only through packages/secrets/reveal, in apps/*/cmd.
  "apps/web/ui|$mod/packages/secrets/reveal"
  "apps/admin/ui|$mod/packages/secrets/reveal"
  "apps/web/internal/config|$mod/packages/secrets/reveal"
  "apps/web/internal/server|$mod/packages/secrets/reveal"
  "apps/admin/internal/server|$mod/packages/secrets/reveal"
  "apps/web/internal/api|$mod/packages/secrets/reveal"
  "apps/web/internal/services|$mod/packages/secrets/reveal"
  "apps/web/ui|$mod/apps/web/internal/api"
  # The shared component set renders values only, for either app.
  "packages/uikit|$mod/apps/web/ui/view"
  "packages/uikit|$mod/apps/admin/ui/view"
  "packages/uikit|$mod/packages/database"
  "packages/uikit|$mod/packages/auth"
  "packages/uikit|$mod/packages/secrets"
  "packages/uikit|github.com/jackc/pgx/v5"
  "apps/admin/ui|$mod/apps/admin/internal/api"
  "packages/backup|$mod/packages/database"
  "apps/web/internal/server|$mod/packages/backup"
)

# Test files. boundary dir | forbidden import
test_cases=(
  "apps/web/internal/services|github.com/jackc/pgx/v5"
  "apps/web/internal/services|$mod/packages/database/model"
  "apps/web/internal/services|$mod/packages/auth"
  "apps/web/internal/api|github.com/jackc/pgx/v5"
  "apps/web/internal/api|$mod/apps/web/internal/services/mocks"
  "apps/admin/internal/services|github.com/jackc/pgx/v5"
  "apps/admin/internal/services|$mod/packages/database/adminrepo/model"
  "apps/admin/internal/services|$mod/packages/auth"
  "apps/admin/internal/api|$mod/apps/admin/internal/services/mocks"
  "apps/admin/internal/api|$mod/packages/database/adminrepo"
)

paths=()
cleanup() { for p in "${paths[@]}"; do rm -rf "$p"; done; rmdir apps/web/ui 2>/dev/null || true; }
trap cleanup EXIT

# Forbidden calls (forbidigo, not depguard: the package may be imported).
# auth's user-administration entry points. layer dir | import | call
calls=(
  "apps/web/internal/server|$mod/packages/auth|auth.NewUserAdmin(nil)"
  "apps/web/ui|$mod/packages/auth|(*auth.Auth)(nil).Users()"
)

# Code that must not compile: a lint rule can be routed around, the compiler
# cannot. secrets.Secret has no method returning its value (so an interface
# cannot reach it), and only packages/secrets/... may import the internal
# package reveal.Value reads it through.
# layer dir | what | declaration | compiler error (grep -E)
builds=(
  "apps/web/ui|reach secrets.Secret through interface{ Reveal() string }|import \"$mod/packages/secrets\"\n\nvar _ interface{ Reveal() string } = secrets.Secret{}|missing method|does not implement"
  "apps/admin/internal/server|reach secrets.Secret through interface{ Reveal() string }|import \"$mod/packages/secrets\"\n\nvar _ interface{ Reveal() string } = secrets.Secret{}|missing method|does not implement"
  "apps/web/internal/server|import $mod/packages/secrets/internal/access|import _ \"$mod/packages/secrets/internal/access\"|use of internal package"
)

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

called=()
for i in "${!calls[@]}"; do
  layer=${calls[$i]%%|*} rest=${calls[$i]#*|}
  imp=${rest%%|*} call=${rest#*|}
  dir="$layer/${probe}_call$i"
  mkdir -p "$dir"
  paths+=("$dir")
  printf 'package %s\n\nimport "%s"\n\nvar _ = %s\n' "${probe}_call$i" "$imp" "$call" > "$dir/probe.go"
  called+=("$layer must not call $call|$dir/probe.go|./$dir/")
done

# Where reveal.Value is allowed, the rule must stay quiet.
allowed=(./apps/web/cmd/server/ ./apps/admin/cmd/admin/ ./packages/secrets/)

pkgs=("${allowed[@]}")
for p in "${planted[@]}" "${called[@]}"; do pkgs+=("${p##*|}"); done
out=$($lint run --enable-only depguard,forbidigo --max-issues-per-linter 0 --max-same-issues 0 \
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
for p in "${called[@]}"; do
  label=${p%%|*} rest=${p#*|}
  if grep -q "${rest%%|*}:.*(forbidigo)" <<<"$out"; then
    echo "ok    $label"
  else
    echo "FAIL  $label (not reported by forbidigo)"
    failed=1
  fi
done
for p in "${allowed[@]}"; do
  if grep "^${p#./}[^/:]*:" <<<"$out" | grep -qv "$probe"; then
    echo "FAIL  ${p#./} may import $mod/packages/secrets/reveal (golangci-lint reported it)"
    failed=1
  else
    echo "ok    ${p#./} may import $mod/packages/secrets/reveal"
  fi
done
if [ "$failed" = 1 ]; then
  echo "--- golangci-lint output:"; echo "$out"
  exit 1
fi

for i in "${!builds[@]}"; do
  layer=${builds[$i]%%|*} rest=${builds[$i]#*|}
  what=${rest%%|*} rest=${rest#*|}
  decl=${rest%%|*} want=${rest#*|}
  label="$layer must not compile: $what"
  dir="$layer/${probe}_build$i"
  mkdir -p "$dir"
  paths+=("$dir")
  printf 'package %s\n\n%b\n' "${probe}_build$i" "$decl" > "$dir/probe.go"
  if bout=$(go build "./$dir/" 2>&1); then
    echo "FAIL  $label (it compiled)"
    failed=1
  elif grep -qE "$want" <<<"$bout"; then
    echo "ok    $label"
  else
    echo "FAIL  $label (unexpected error)"; echo "$bout"
    failed=1
  fi
done
[ "$failed" = 0 ] || exit 1
