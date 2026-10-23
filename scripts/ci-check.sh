#!/usr/bin/env bash
# The CI checks, run as parallel tasks inside one job (.github/actions/check):
# lint, migration order, generated code, tests, the release smoke, the deploy/
# module and the mobile app each run in the background with their own log, then the probes
# (which plant files in the tree, so nothing else may be reading it) run once
# all of them are done.
# Each task's log is printed as one collapsed group, failures expanded.
#
# Env (from the check action; empty or false skips a task):
#   MODE      all | db | other   (db: lint + DB tests, other: the rest; see ci.yml)
#   DIRS      affected package dirs for golangci-lint
#   TESTS     import paths to test (scripts/ci-shard.sh: tested packages only)
#   DB        true: some of TESTS need Postgres (started by the action)
#   FULL      true: shared inputs changed, also run the probes
#   SQLC TEMPL GENERATE   generators to rerun (scripts/affected.sh)
#   MOBILE DEPLOY         true: check apps/mobile-app / the deploy/ module
#   SMOKE                 true: the release smoke (scripts/release-smoke.sh)
#   EVENT TARGET          the GitHub event and the branch it targets (the PR's
#                         base, else the pushed or dispatched ref): migration order
set -uo pipefail
cd "$(dirname "$0")/.."

# The job's Postgres (started by the check action) and how long to wait for
# it: 2x its measured boot (12-22 s), at most 45 s.
readonly pg_server=postgres://postgres:postgres@127.0.0.1:5432
readonly pg_wait_seconds=0.25
readonly pg_wait_attempts=180
# Every task but the tests runs at the lowest CPU priority, so the timed test
# run gets the CPUs whenever it wants them and the others use what is left
# (see test_go).
readonly background_niceness=19

tmp=${RUNNER_TEMP:-$(mktemp -d)}/ci-check
rm -rf "$tmp" && mkdir -p "$tmp" .cache
lint=false
[ "$MODE" != other ] && [ -n "${DIRS:-}" ] && lint=true

names=() pids=() starts=()
start() { # start <name> <function>: run it in the background, log to $tmp/<name>.log
  names+=("$1") starts+=("$(date +%s.%N)")
  # The inner subshell is the task (its set -e exits it, not the bookkeeping);
  # its children inherit the outer subshell's priority.
  (
    [ "$1" = test ] || renice -n "$background_niceness" -p "$BASHPID" >/dev/null
    ("$2") >"$tmp/$1.log" 2>&1
    echo $? >"$tmp/$1.rc"
    date +%s.%N >"$tmp/$1.end"
  ) &
  pids+=($!)
}
failed=0
report() { # print every started task's log, in start order
  local i n rc secs
  for i in "${!names[@]}"; do
    n=${names[$i]} rc=$(cat "$tmp/$n.rc" 2>/dev/null || echo unfinished)
    secs=$(awk -v a="${starts[$i]}" -v b="$(cat "$tmp/$n.end" 2>/dev/null || date +%s.%N)" 'BEGIN { printf "%.1f", b - a }')
    if [ "$rc" = 0 ]; then
      echo "::group::ok    ${secs}s  $n"; cat "$tmp/$n.log"; echo "::endgroup::"
    else
      echo "::error::$n failed (${secs}s, exit $rc)"; cat "$tmp/$n.log"; failed=1
    fi
  done
}
# A step timeout or cancel still shows what each task got to.
trap 'report; exit 1' INT TERM

# ---- tasks ----
lint_go() {
  set -e
  unformatted=$(gofmt -l .)
  [ -z "$unformatted" ] || { echo "$unformatted"; echo 'gofmt: run gofmt -w .'; exit 1; }
  # word splitting intended: DIRS is a list. --allow-parallel-runners: the
  # deploy task lints at the same time (same cache, safe).
  golangci-lint run --allow-parallel-runners $DIRS
  ./scripts/check-test-boundaries.sh
  ./scripts/check-ui-api-paths.sh
  if [ "$FULL" = true ]; then ./scripts/check-coverage.sh --exist; fi
}

# In a separate worktree of HEAD, so regenerating never rewrites a file that
# lint or the tests are reading.
generated() {
  set -e
  gen=$tmp/gen
  git worktree add -q --detach "$gen" HEAD
  cd "$gen"
  if [ "$SQLC" = true ]; then sqlc generate -f packages/database/sqlc.yaml; fi
  for p in $GENERATE; do go generate "$p"; done
  if [ "$TEMPL" = true ]; then
    for ui in packages/uikit apps/web/ui apps/admin/ui; do if [ -d "$ui" ]; then go tool templ generate -path "$ui"; fi; done
  fi
  status=$(git status --porcelain)
  [ -z "$status" ] || { git diff; echo "$status"; echo 'generated code is stale: run make generate and commit'; exit 1; }
}

# DB-backed test binaries wait for Postgres through an -exec wrapper, so
# compiling overlaps its boot. Readiness is checked inside the container over
# TCP: docker's port proxy accepts connections before Postgres listens, and
# the image's first-boot init server answers on the unix socket only.
test_go() {
  set -e
  exec_flag=()
  if [ "$DB" = true ]; then
    cat >"$tmp/wait-db" <<'SH'
#!/usr/bin/env bash
if grep -lq 'TEST_DATABASE_URL' ./*_test.go 2>/dev/null; then
  # 2x the measured Postgres boot (12-22 s): never waits longer than 45 s.
  for _ in $(seq 1 180); do
    docker exec pg pg_isready -q -h 127.0.0.1 -U postgres 2>/dev/null && exec "$@"
    sleep 0.25
  done
  echo "postgres did not start:" >&2; cat "$RUNNER_TEMP/pg.log" >&2; docker logs pg >&2; exit 1
fi
exec "$@"
SH
    chmod +x "$tmp/wait-db"
    exec_flag=(-exec "$tmp/wait-db")
  fi
  # The time budgets (AGENTS.md "Fast tests") must measure tests, not
  # compilation on a 2-vCPU runner. So: build and link every test binary
  # first, untimed, with the timed run's -race -cover instrumentation (each
  # "runs" as `true`, so no test, TestMain or database is touched); the timed
  # run then only links and runs. The other tasks (lint, generators, smoke,
  # deploy/, mobile) run at background_niceness: they use the CPU time the
  # tests leave instead of slowing them down.
  local build_start=$SECONDS
  go test -race -cover -exec true $TESTS >"$tmp/test-build.log" 2>&1 || { cat "$tmp/test-build.log"; return 1; }
  echo "test binaries built in $((SECONDS - build_start))s"
  # testbudget prints plain go test output, the slowest tests, and fails a
  # package or test over its time budget.
  go test -race -json "${exec_flag[@]}" -coverprofile=.cache/coverage.out $TESTS | go run ./scripts/testbudget
  TESTED="$TESTS" ./scripts/check-coverage.sh .cache/coverage.out
}

# A new migration must sit above every migration the deployed branches have
# (scripts/check-migration-order.sh): staging (dev) and production (main).
# Bases: HEAD^1 on PRs, the merge queue and pushes (the target branch: the
# merge commit's base, or the branch's previous commit, which catches two PRs
# that raced for one number), plus the tip of each deployed branch other than
# the target itself. A deployed branch the repo does not have is skipped.
migration_order() {
  set -e
  . scripts/branches.sh
  bases=() refspecs=()
  case $EVENT in pull_request | merge_group | push) bases+=(HEAD^1) ;; esac
  for branch in $(git ls-remote --heads origin "${DEPLOY_BRANCHES[@]}" | sed 's|.*refs/heads/||'); do
    [ "$branch" = "$TARGET" ] && continue
    refspecs+=("+refs/heads/$branch:refs/remotes/origin/$branch") bases+=("origin/$branch")
  done
  [ "${#refspecs[@]}" -eq 0 ] || git fetch -q --no-tags --depth=1 origin "${refspecs[@]}"
  if [ "${#bases[@]}" -eq 0 ]; then echo "no base to compare with ($EVENT on $TARGET)"; return; fi
  echo "bases: ${bases[*]}"
  ./scripts/check-migration-order.sh "${bases[@]}"
}

# Builds every Dockerfile's binary as its image does, migrates fresh
# databases on this job's Postgres and boots web + admin in production mode.
# Waits for Postgres like the DB tests do (it boots while the build runs).
smoke() {
  set -e
  local attempt
  for attempt in $(seq 1 "$pg_wait_attempts"); do
    docker exec pg pg_isready -q -h 127.0.0.1 -U postgres 2>/dev/null && break
    sleep "$pg_wait_seconds"
  done
  echo "postgres ready after $attempt checks"
  local database
  for database in smoke_app smoke_admin; do docker exec pg createdb -U postgres "$database"; done
  SMOKE_DATABASE_URL="$pg_server/smoke_app?sslmode=disable" \
    SMOKE_ADMIN_DATABASE_URL="$pg_server/smoke_admin?sslmode=disable" \
    ./scripts/release-smoke.sh
}

deploy_mod() {
  set -e
  cd deploy
  go vet ./...
  (cd .railway && go vet railway.go)
  golangci-lint run --allow-parallel-runners -c ../.golangci.yml ./...
}

# node_modules comes from the cache keyed by the lockfile (see the action);
# installed only on a miss.
mobile() {
  set -e
  cd apps/mobile-app
  [ -d node_modules ] || pnpm install --frozen-lockfile
  pnpm exec tsc --noEmit
  pnpm exec expo lint
  pnpm exec expo install --check
  pnpm test
}

[ "$lint" = true ] && start lint lint_go
[ "$MODE" != other ] && [ -n "${EVENT:-}" ] && start migrations migration_order
[ "$MODE" != other ] && { [ "${SQLC:-}" = true ] || [ "${TEMPL:-}" = true ] || [ -n "${GENERATE:-}" ]; } && start generated generated
[ -n "${TESTS:-}" ] && start test test_go
[ "$MODE" != db ] && [ "${SMOKE:-}" = true ] && start smoke smoke
[ "${DEPLOY:-}" = true ] && start deploy deploy_mod
[ "${MOBILE:-}" = true ] && start mobile mobile
wait "${pids[@]}" 2>/dev/null
report
[ -d "$tmp/gen" ] && git worktree remove --force "$tmp/gen"

# ---- probes, after everything above: they plant files in the tree ----
# They prove the lint and test rules still bite; their inputs (scripts/,
# .golangci.yml) always force a full run. The coverage probes reuse the test
# run's profile (a full run tests every probed package).
probes() {
  set -e
  if [ "$lint" = true ]; then GOLANGCI_LINT=golangci-lint ./scripts/check-layering.sh; fi
  if [ "$MODE" != db ]; then PROFILE=.cache/coverage.out ./scripts/check-test-probes.sh; fi
}
if [ "$failed" = 0 ] && [ "$FULL" = true ]; then
  names=() pids=() starts=()
  start probes probes
  wait "${pids[@]}"
  report
fi
exit "$failed"
