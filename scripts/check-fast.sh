#!/usr/bin/env bash
# The pre-push check (`make check-fast`, and the hook `make hooks` installs):
# gofmt, go vet, golangci-lint and plain go test for only the packages this
# checkout's changes can affect, the generators whose inputs changed, and the
# deploy/ module when it changed, and the migration order when a migration
# changed. "Changes" are everything that differs from
# the merge base with origin/dev (origin/main when there is no dev; see
# scripts/branches.sh): commits, staged, unstaged and untracked
# files. scripts/affected.sh maps them to packages exactly as CI does, so a
# change to a shared input (go.mod, tooling, lint config, migrations,
# generator config) checks everything.
#
# No -race and no Postgres: database tests skip unless TEST_DATABASE_URL is
# set, and CI still runs the full suite. It catches what fails CI most often
# (format, lint, vet, a broken test, stale generated code) in seconds.
#
# Usage: scripts/check-fast.sh   (CHECK_BASE=<ref> compares with another base)
set -euo pipefail

# A git hook (or a caller in another checkout) can export repository env such
# as GIT_DIR or GIT_INDEX_FILE; git and go (VCS stamping) would then read the
# wrong repository. Nothing below inherits it.
# shellcheck disable=SC2046
unset $(git rev-parse --local-env-vars)
cd "$(dirname "$0")/.."
. scripts/branches.sh

GO=${GO:-go}
lint() { "$GO" tool -modfile=tools/go.mod golangci-lint "$@"; }
now() { date +%s%N | cut -c1-13; }
start=$(now)
step_start=$start
failed=()
step() { step_start=$(now); printf '%-26s' "$1"; }
done_() { awk -v d=$(($(now) - step_start)) 'BEGIN { printf "%.1fs\n", d / 1000 }'; }
fail() { failed+=("$1"); }

base_ref=${CHECK_BASE:-$(landing_ref)}
if [ -z "$base_ref" ] || ! base=$(git merge-base "$base_ref" HEAD 2>/dev/null); then
  echo "check-fast: no merge base with $base_ref (git fetch origin, or set CHECK_BASE)" >&2
  exit 2
fi

# Committed, staged and unstaged changes, then untracked files.
mapfile -t changed < <({
  git diff --name-only --no-renames "$base" --
  git ls-files --others --exclude-standard
} | sort -u)
if [ "${#changed[@]}" -eq 0 ]; then
  echo "check-fast: nothing differs from $base_ref"
  exit 0
fi

step "affected packages"
full=false packages="" dirs="" sqlc=false templ=false generate=""
if ! aff=$(AFFECTED_FILES="${changed[*]}" ./scripts/affected.sh 2>/dev/null); then
  echo "check-fast: scripts/affected.sh failed" >&2
  exit 2
fi
while IFS='=' read -r k v; do
  case $k in
  full) full=$v ;; packages) packages=$v ;; dirs) dirs=$v ;;
  sqlc) sqlc=$v ;; templ) templ=$v ;; generate) generate=$v ;;
  esac
done <<<"$aff"
deploy=false
for f in "${changed[@]}"; do
  case $f in deploy/* | .golangci.yml | tools/*) deploy=true ;; esac
done
[ "$full" = true ] && deploy=true
done_
read -r -a pkgs <<<"$packages"
read -r -a pkgdirs <<<"$dirs"
if [ "$full" = true ]; then
  echo "  everything (a shared input changed)"
else
  echo "  ${#changed[@]} changed files -> ${#pkgs[@]} packages$([ "$deploy" = true ] && echo ' + deploy/')"
fi

# Migration order, when a migration changed: against the tips of dev and main
# (or CHECK_BASE), not the merge base, so a branch either has moved past fails
# too (goose refuses a version below one already applied).
if printf '%s\n' "${changed[@]}" | grep -qE '(^|/)migrations/[^/]*\.sql$'; then
  step "migration order"
  if ! out=$(./scripts/check-migration-order.sh ${CHECK_BASE:+"$CHECK_BASE"} 2>&1); then done_; echo "$out" | sed 's/^/  /'; fail migration-order; else done_; fi
fi

# Format: the changed Go files (everything on a full run).
step "gofmt"
mapfile -t gofiles < <(for f in "${changed[@]}"; do [[ $f == *.go && -f $f ]] && echo "$f"; done)
if [ "$full" = true ]; then unformatted=$(gofmt -l . 2>&1 || true); else unformatted=$( ((${#gofiles[@]})) && gofmt -l "${gofiles[@]}" 2>&1 || true); fi
done_
if [ -n "$unformatted" ]; then echo "$unformatted" | sed 's/^/  /'; echo '  run: gofmt -w <files>'; fail gofmt; fi

# Generators whose inputs or outputs changed: regenerate and see whether any
# Go file changes (fingerprint of the Go files' diff and untracked contents).
if [ "$sqlc" = true ] || [ "$templ" = true ] || [ -n "$generate" ]; then
  fingerprint() {
    {
      git diff --binary -- '*.go'
      git ls-files --others --exclude-standard -- '*.go' | while read -r f; do echo "$f"; cat "$f"; done
    } | sha1sum
  }
  step "generated code"
  before=$(fingerprint)
  if ! out=$({
    if [ "$sqlc" = true ]; then "$GO" tool -modfile=tools/go.mod sqlc generate -f packages/database/sqlc.yaml; fi
    for p in $generate; do "$GO" generate "$p"; done
    if [ "$templ" = true ]; then
      for ui in packages/uikit apps/web/ui apps/admin/ui; do [ ! -d "$ui" ] || "$GO" tool templ generate -path "$ui"; done
    fi
  } 2>&1); then
    echo "$out" | sed 's/^/  /'
    fail generate
  fi
  done_
  if [ "$before" != "$(fingerprint)" ]; then
    echo '  generated code was stale and has been regenerated: review `git status` and commit it'
    fail generate
  fi
fi

if [ "${#pkgs[@]}" -gt 0 ]; then
  step "go vet"
  if ! out=$("$GO" vet "${pkgs[@]}" 2>&1); then done_; echo "$out" | sed 's/^/  /'; fail vet; else done_; fi

  step "golangci-lint"
  if ! out=$(lint run "${pkgdirs[@]}" 2>&1); then done_; echo "$out" | sed 's/^/  /'; fail golangci-lint; else done_; fi

  step "test locations"
  if ! out=$(./scripts/check-test-boundaries.sh 2>&1); then done_; echo "$out" | sed 's/^/  /'; fail test-boundaries; else done_; fi

  # Only an explicit TEST_DATABASE_URL reaches the tests: make exports the dev
  # DATABASE_URL, which suites fall back to and would wait on when it is down.
  step "go test (no -race)"
  if ! out=$(env -u DATABASE_URL -u ADMIN_DATABASE_URL "$GO" test "${pkgs[@]}" 2>&1); then
    done_
    echo "$out" | grep -vE '^(ok|\?) ' | sed 's/^/  /'
    fail test
  else
    done_
  fi
fi

if [ "$deploy" = true ] && [ -d deploy ]; then
  step "deploy/ vet, lint"
  if ! out=$(cd deploy && { "$GO" vet ./... && "$GO" tool -modfile=../tools/go.mod golangci-lint run -c ../.golangci.yml ./...; } 2>&1); then
    done_
    echo "$out" | grep -vE '^(ok|\?) ' | sed 's/^/  /'
    fail deploy
  else
    done_
  fi
fi

total=$(awk -v d=$(($(now) - start)) 'BEGIN { printf "%.1fs", d / 1000 }')
if [ "${#failed[@]}" -gt 0 ]; then
  echo "check-fast: FAIL (${failed[*]}) in $total"
  exit 1
fi
echo "check-fast: ok in $total (CI still runs -race, Postgres and the probes)"
