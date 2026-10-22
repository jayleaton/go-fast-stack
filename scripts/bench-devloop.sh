#!/usr/bin/env bash
# Prints the dev-loop timing tables (markdown): build per binary (cold, warm,
# after a one-line edit) with size, cold start and memory; the generators;
# golangci-lint (whole run and per linter); the CI probes; and tests per
# package with and without -race, plus the slowest tests. `make bench-devloop`
# runs it; the "Bench dev loop" workflow runs it on GitHub Actions with
# BENCH_COLD=1 so numbers are comparable across branches.
#
# BENCH_COLD=1 also measures cold caches (a fresh GOCACHE and lint cache in a
# temp dir; your own caches are never touched). Tests that use Postgres, and
# cold start, need TEST_DATABASE_URL (`make bench-devloop` starts the test
# database) and psql. BENCH_ONLY=build,generate,lint,probes,tests picks
# sections. Appends the report to $GITHUB_STEP_SUMMARY when set.
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}
cold=${BENCH_COLD:-}
only=,${BENCH_ONLY:-build,generate,lint,probes,tests},
want() { [[ $only == *",$1,"* ]]; }
mkdir -p .cache
tmp=$(mktemp -d "$PWD/.cache/bench-devloop.XXXXXX") # not /tmp: often RAM, and cold caches are GBs
pids=() dbs=() edited=""
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  for d in "${dbs[@]}"; do psql -q "$TEST_DATABASE_URL" -c "DROP DATABASE IF EXISTS $d WITH (FORCE)" >/dev/null 2>&1 || true; done
  if [ -n "$edited" ]; then cp "$tmp/edited.orig" "$edited"; fi
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

report=$tmp/report.md
log=$tmp/cmd.log
say() { printf '%s\n' "$*" | tee -a "$report"; }
now() { date +%s%N | cut -c1-13; } # milliseconds
secs() { awk -v d="$1" 'BEGIN { printf "%.1fs", d / 1000 }'; }

# t <cmd...>: runs cmd quietly and prints its wall time; shows the log on failure.
t() {
  local s
  s=$(now)
  if ! "$@" >"$log" 2>&1; then
    echo "bench: failed: $*" >&2
    tail -50 "$log" >&2
    return 1
  fi
  secs $(($(now) - s))
}

# cold <cmd...>: runs cmd with a fresh, empty build and lint cache.
cold() {
  chmod -R u+w "$tmp/cold" 2>/dev/null || true
  rm -rf "$tmp/cold" && mkdir -p "$tmp/cold/go" "$tmp/cold/lint"
  GOCACHE=$tmp/cold/go GOLANGCI_LINT_CACHE=$tmp/cold/lint "$@"
}

toolrun() { "$GO" tool -modfile=tools/go.mod "$@"; }

say "## Dev loop timings"
say ""
say "$($GO version | cut -d' ' -f3-), $(nproc) CPUs, $(git rev-parse --short HEAD); $([ -n "$cold" ] && echo 'cold = empty build and lint cache, warm = second run' || echo 'warm caches only (BENCH_COLD=1 adds cold)')."
say ""

# ---- build ----
if want build; then
build() { CGO_ENABLED=0 "$GO" build -trimpath -ldflags='-s -w' -o "$@"; }

# coldstart <name> <binary>: migrates throwaway databases, starts the binary
# and prints "<ms to first 200 from /healthz> <RSS then>".
coldstart() {
  local name=$1 bin=$2 port app admin url pid s rss
  command -v psql >/dev/null || return 1
  app=bench_app_$$ admin=bench_admin_$$
  url() { local u=${TEST_DATABASE_URL%%\?*}; echo "${u%/*}/$1?${TEST_DATABASE_URL#*\?}"; }
  if [ ! -e "$tmp/migrated" ]; then
    for d in $app $admin; do dbs+=("$d"); psql -q "$TEST_DATABASE_URL" -c "CREATE DATABASE $d" >/dev/null; done
    DATABASE_URL=$(url $app) PUBLIC_URL=http://bench.test AUTH_SECRET=bench-secret-bench-secret-0123456789abcdef "$tmp/server" migrate >/dev/null 2>&1
    DATABASE_URL=$(url $app) ADMIN_DATABASE_URL=$(url $admin) ADMIN_PUBLIC_URL=http://bench.test ADMIN_AUTH_SECRET=bench-admin-secret-0123456789abcdef0123 \
      "$tmp/admin" migrate >/dev/null 2>&1
    touch "$tmp/migrated"
  fi
  port=$((20000 + RANDOM % 20000))
  s=$(now)
  env -i PATH="$PATH" APP_ENV=development PORT=$port ADMIN_PORT=$port PUBLIC_URL=http://bench.test ADMIN_PUBLIC_URL=http://bench.test \
    AUTH_SECRET=bench-secret-bench-secret-0123456789abcdef ADMIN_AUTH_SECRET=bench-admin-secret-0123456789abcdef0123 \
    DATABASE_URL="$(url $app)" ADMIN_DATABASE_URL="$(url $admin)" "$bin" serve >"$tmp/$name.log" 2>&1 &
  pid=$!
  pids+=("$pid")
  for _ in $(seq 1 1000); do
    if curl -fsS -o /dev/null "http://127.0.0.1:$port/healthz" 2>/dev/null; then
      local took=$(($(now) - s))
      rss=$(awk '/VmRSS/ { printf "%.0f MB", $2 / 1024 }' "/proc/$pid/status")
      kill "$pid"
      wait "$pid" 2>/dev/null || true
      echo "${took}ms $rss"
      return 0
    fi
    sleep 0.01
  done
  kill "$pid" 2>/dev/null || true
  tail -20 "$tmp/$name.log" >&2
  return 1
}

say "### Build (CGO_ENABLED=0 -trimpath -ldflags='-s -w', as the Dockerfiles)"
say ""
# The edit adds code to the app's services package: it recompiles, and
# everything above it recompiles or relinks.
rows=()
for b in server:./apps/web/cmd/server:apps/web/internal/services/users.go admin:./apps/admin/cmd/admin:apps/admin/internal/services/users.go; do
  IFS=: read -r name pkg edited <<<"$b"
  cp "$edited" "$tmp/edited.orig"
  c=-
  if [ -n "$cold" ]; then c=$(cold t build "$tmp/$name" "$pkg"); fi
  build "$tmp/$name" "$pkg"
  w=$(t build "$tmp/$name" "$pkg")
  printf '\nvar BenchEdit = %s\n' "$(now)" >>"$edited" # new code: recompile + relink
  e=$(t build "$tmp/$name" "$pkg")
  cp "$tmp/edited.orig" "$edited"
  build "$tmp/$name" "$pkg"
  edited=""
  size=$(awk -v s="$(stat -c %s "$tmp/$name")" 'BEGIN { printf "%.1f MB", s / 1048576 }')
  rows+=("$name|$c|$w|$e|$size")
done
say "| binary | cold | warm | after a one-line edit | size | cold start to /healthz | RSS after start |"
say "|---|--:|--:|--:|--:|--:|--:|"
for r in "${rows[@]}"; do
  IFS='|' read -r name c w e size <<<"$r"
  start=- rss=-
  if [ -n "${TEST_DATABASE_URL:-}" ]; then
    read -r start rss < <(coldstart "$name" "$tmp/$name" || echo "- -") || true
  fi
  say "| $name | $c | $w | $e | $size | $start | $rss |"
done
say ""
fi

# ---- generate ----
if want generate; then
say "### Generators (\`make generate\` steps)"
say ""
say "| step | cold | warm |"
say "|---|--:|--:|"
gen_sqlc() { toolrun sqlc generate -f packages/database/sqlc.yaml; }
gen_go() { "$GO" generate ./...; }
gen_templ() { for ui in apps/web/ui apps/admin/ui; do [ ! -d $ui ] || "$GO" tool templ generate -path $ui; done; }
for g in sqlc:gen_sqlc "go generate (mockgen)":gen_go templ:gen_templ "make generate":"make --no-print-directory generate"; do
  name=${g%%:*} cmd=${g#*:}
  c=-
  if [ -n "$cold" ]; then c=$(cold t $cmd); fi
  $cmd >/dev/null 2>&1
  say "| $name | $c | $(t $cmd) |"
done
if ! git diff --quiet -- '*.go'; then say ""; say "**generated code is stale** (git diff after generate)"; fi
say ""
fi

# ---- lint ----
if want lint; then
say "### golangci-lint"
say ""
lint=${GOLANGCI_LINT:-$("$GO" tool -modfile=tools/go.mod -n golangci-lint)}
say "| run | cold | warm |"
say "|---|--:|--:|"
c=-
if [ -n "$cold" ]; then c=$(cold t "$lint" run); fi
"$lint" run >/dev/null 2>&1 || true
say "| golangci-lint run ./... | $c | $(t "$lint" run) |"
say "| gofmt -l . | - | $(t gofmt -l .) |"
say ""
if [ -n "$cold" ]; then
  # Each linter alone on an empty lint cache (warm Go build cache): its time
  # includes loading and type-checking the packages, the first row alone.
  say "Per linter, alone, empty lint cache (includes loading packages):"
  say ""
  say "| linter | time |"
  say "|---|--:|"
  mapfile -t linters < <("$lint" linters 2>/dev/null | sed -n '/^Enabled by your/,/^$/p' | sed -nE 's/^([a-z0-9]+):.*/\1/p')
  for l in "${linters[@]}"; do
    rm -rf "$tmp/lintonly" && mkdir -p "$tmp/lintonly"
    say "| $l | $(GOLANGCI_LINT_CACHE=$tmp/lintonly t "$lint" run --enable-only "$l") |"
  done
  say ""
fi
fi

# ---- probes ----
if want probes; then
say "### Probes (prove the lint and test rules still bite)"
say ""
say "| script | warm |"
say "|---|--:|"
say "| check-test-boundaries.sh | $(t ./scripts/check-test-boundaries.sh) |"
say "| check-layering.sh | $(GOLANGCI_LINT=$lint t ./scripts/check-layering.sh) |"
say "| check-test-probes.sh | $(t ./scripts/check-test-probes.sh) |"
say ""
fi

# ---- tests ----
if want tests; then
say "### Tests (go test ./..., -count=1)"
say ""
[ -n "${TEST_DATABASE_URL:-}" ] || say "TEST_DATABASE_URL is not set: database tests skip."
"$GO" build -o "$tmp/testbudget" ./scripts/testbudget
# gotest <name> <flags...>: one run; prints its wall time, keeps the -json.
gotest() {
  local name=$1 s
  shift
  s=$(now)
  "$GO" test -count=1 -json "$@" ./... >"$tmp/$name.json" 2>"$log" || {
    "$tmp/testbudget" -pkg 0 -test 0 -top 0 <"$tmp/$name.json" >&2 || true
    cat "$log" >&2
    return 1
  }
  secs $(($(now) - s))
}
say "| run | cold (compile + run) | warm (run only) |"
say "|---|--:|--:|"
c=-
if [ -n "$cold" ]; then c=$(cold gotest cold-race -race -coverprofile="$tmp/cover.out"); fi
say "| -race -coverprofile (CI) | $c | $(gotest race -race -coverprofile="$tmp/cover.out") |"
c=-
if [ -n "$cold" ]; then c=$(cold gotest cold-plain); fi
say "| plain | $c | $(gotest plain) |"
say ""
say "Per package (test binary wall time, warm):"
say ""
"$tmp/testbudget" table "-race -cover=$tmp/race.json" "plain=$tmp/plain.json" | tee -a "$report"
say ""
fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then cat "$report" >>"$GITHUB_STEP_SUMMARY"; fi
