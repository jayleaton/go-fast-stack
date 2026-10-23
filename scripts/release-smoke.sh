#!/usr/bin/env bash
# Release smoke: what a deploy hits and the tests cannot see.
#
#   1. build   every binary that has a Dockerfile, with that Dockerfile's own
#              build-stage ENV and RUN line (scripts/dockerfile-builds.sh), and
#              fail if one is dynamically linked: it would not start on
#              distroless/static ("missing dynamic library?").
#   2. migrate the web app's database, then the admin app's (when apps/admin
#              exists), with the binaries' own `migrate`, as pre-deploy does.
#   3. boot    both with APP_ENV=production and only the config production
#              requires (no Doppler, no optional integrations; a dummy
#              RESEND_API_KEY, which production requires) and wait for
#              /healthz 200.
#
# Env: SMOKE_DATABASE_URL, SMOKE_ADMIN_DATABASE_URL   empty databases to use
#      (`make release-smoke` and CI create them); SMOKE_BOOT_SECONDS,
#      SMOKE_WEB_PORT, SMOKE_ADMIN_PORT override the defaults below.
# Needs readelf (binutils) and curl.
set -euo pipefail
cd "$(dirname "$0")/.."

readonly default_boot_seconds=30 # per app; a healthy boot takes about 1 s
readonly default_web_port=3390   # away from the dev servers' 3000/3001
readonly default_admin_port=3391
readonly poll_seconds=0.2 # between /healthz attempts
readonly http_ok=200
readonly log_tail_lines=40 # of a failed app's log

app_db=${SMOKE_DATABASE_URL:?SMOKE_DATABASE_URL: an empty database for the web app}
admin_db=${SMOKE_ADMIN_DATABASE_URL:-}
boot_seconds=${SMOKE_BOOT_SECONDS:-$default_boot_seconds}
web_port=${SMOKE_WEB_PORT:-$default_web_port}
admin_port=${SMOKE_ADMIN_PORT:-$default_admin_port}
work=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
step() { printf '== %s (%ss)\n' "$1" "$SECONDS"; }
fail() {
  echo "release smoke: $*" >&2
  exit 1
}

# ---- 1. build as the Dockerfiles do ----
# Only the Dockerfile's ENV and RUN line reach the build: no GOFLAGS,
# CGO_ENABLED or tags from the caller, so a Dockerfile that drops one fails
# here the way its image would. /out/ becomes the work dir.
step build
keep=(PATH="$PATH" HOME="$HOME" GOPATH="$(go env GOPATH)" GOCACHE="$(go env GOCACHE)"
  GOMODCACHE="$(go env GOMODCACHE)" GOTOOLCHAIN="$(go env GOTOOLCHAIN)")
[ -z "${GOTMPDIR:-}" ] || keep+=(GOTMPDIR="$GOTMPDIR")
mapfile -t builds < <(./scripts/dockerfile-builds.sh)
[ "${#builds[@]}" -gt 0 ] || fail "no Dockerfile with a 'RUN ... go build' line"
for build in "${builds[@]}"; do
  IFS=$'\x1f' read -r dockerfile build_env run output _ <<<"$build"
  command=${run//\/out\//$work\/}
  binary=$work/${output#/out/}
  echo "$dockerfile: ${build_env:+$build_env }$command"
  # build_env is the Dockerfile's ENV text: word splitting intended.
  # shellcheck disable=SC2086
  env -i "${keep[@]}" $build_env bash -c "$command" || fail "$dockerfile: the build failed"
  [ -x "$binary" ] || fail "$dockerfile: the build did not write $output"
  if readelf -lW "$binary" | grep -q 'program interpreter'; then
    readelf -dW "$binary" | grep NEEDED >&2 || true
    fail "${binary##*/} is dynamically linked: it would not start on distroless/static ($dockerfile). A dependency needs cgo or loads libraries at run time (e.g. purego): build with the tag that keeps it static, in the Dockerfile's ENV GOFLAGS"
  fi
  echo "${binary##*/}: statically linked"
done

# ---- 2. migrate like pre-deploy ----
admin=false
[ -x "$work/admin" ] && [ -n "$admin_db" ] && admin=true
web_env=(APP_ENV=production PORT="$web_port" PUBLIC_URL="http://127.0.0.1:$web_port"
  DATABASE_URL="$app_db" AUTH_SECRET=release-smoke-web-auth-secret-0123456789 RESEND_API_KEY=re_release_smoke_unused)
admin_env=(APP_ENV=production ADMIN_PORT="$admin_port" ADMIN_PUBLIC_URL="http://127.0.0.1:$admin_port"
  DATABASE_URL="$app_db" ADMIN_DATABASE_URL="$admin_db" ADMIN_AUTH_SECRET=release-smoke-admin-auth-secret-0123456789
  RESEND_API_KEY=re_release_smoke_unused)
run() { env -i PATH="$PATH" HOME="$HOME" "$@"; }
step migrate
run "${web_env[@]}" "$work/server" migrate >"$work/web-migrate.log" 2>&1 ||
  { cat "$work/web-migrate.log" >&2; fail "web migrate failed"; }
echo "web: migrated"
if [ "$admin" = true ]; then
  run "${admin_env[@]}" "$work/admin" migrate >"$work/admin-migrate.log" 2>&1 ||
    { cat "$work/admin-migrate.log" >&2; fail "admin migrate failed"; }
  echo "admin: migrated"
fi

# ---- 3. boot like a deployment ----
healthy() { # name port log pid
  local name=$1 port=$2 log=$3 pid=$4
  local deadline=$((SECONDS + boot_seconds)) code=none
  while [ "$SECONDS" -lt "$deadline" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz" || true)
    if [ "$code" = "$http_ok" ]; then
      echo "$name: /healthz $http_ok"
      return 0
    fi
    kill -0 "$pid" 2>/dev/null || break # it exited
    sleep "$poll_seconds"
  done
  tail -n "$log_tail_lines" "$log" >&2
  fail "$name did not answer /healthz $http_ok within ${boot_seconds}s (last: $code)"
}
step boot
run "${web_env[@]}" "$work/server" serve >"$work/web.log" 2>&1 &
pids+=($!)
if [ "$admin" = true ]; then
  run "${admin_env[@]}" "$work/admin" serve >"$work/admin.log" 2>&1 &
  pids+=($!)
fi
healthy web "$web_port" "$work/web.log" "${pids[0]}"
[ "$admin" = false ] || healthy admin "$admin_port" "$work/admin.log" "${pids[1]}"
step "release smoke ok"
