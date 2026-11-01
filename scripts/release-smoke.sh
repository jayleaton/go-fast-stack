#!/usr/bin/env bash
# Release smoke: what a deploy hits and the tests cannot see.
#
#   1. build   every binary that has a Dockerfile, with that Dockerfile's own
#              build-stage ENV and RUN line (scripts/dockerfile-builds.sh), for
#              linux/amd64 as Railway's image builder does, and fail if one is
#              dynamically linked: it would not start on distroless/static
#              ("missing dynamic library?"). On any other host it builds the
#              same line again for the host, to run in steps 2 and 3.
#   2. migrate the web app's database, then the admin app's (when apps/admin
#              exists), with the binaries' own `migrate`, as pre-deploy does.
#   3. boot    both with APP_ENV=production and only the config production
#              requires (no Doppler, no optional integrations; a dummy
#              RESEND_API_KEY and a FROM_EMAIL off the example domains, which
#              production requires; nothing is sent) and wait for /healthz 200
#              from the very process it started: it refuses to start when a
#              smoke port already answers (a leftover server would answer for
#              a build that cannot boot), and a 200 counts only while that
#              process is still running.
#
# Env: SMOKE_DATABASE_URL, SMOKE_ADMIN_DATABASE_URL   empty databases to use
#      (`make release-smoke` and CI create them); SMOKE_BOOT_SECONDS,
#      SMOKE_WEB_PORT, SMOKE_ADMIN_PORT override the defaults below.
# Needs file(1), go and curl; readelf too, when the host has it.
set -euo pipefail
cd "$(dirname "$0")/.."

readonly default_boot_seconds=30 # per app; a healthy boot takes about 1 s
readonly default_web_port=3390   # away from the dev servers' 3000/3001
readonly default_admin_port=3391
readonly poll_seconds=0.2 # between /healthz attempts
readonly http_ok=200
readonly log_tail_lines=40 # of a failed app's log
# Production refuses an unset or example.com sender; nothing is ever sent
# (the Resend key is a dummy), so any unreserved domain will do.
readonly smoke_from_email=release-smoke@go-fast-stack.dev

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

# ---- 0. the smoke ports are free ----
# A port something already answers on would hand its 200 to /healthz in step
# 3, so a build that panics at startup would pass: refuse first, before the
# build. The admin port matters when the admin app boots (it has a database).
port_free() { # name port variable
  local name=$1 port=$2 variable=$3
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    fail "port $port ($name) already answers before the smoke started it: a server from an earlier run (or a dev server) would answer /healthz for this build. Stop it (lsof -nP -iTCP:$port -sTCP:LISTEN shows its PID), or set $variable to a free port"
  fi
}
port_free web "$web_port" SMOKE_WEB_PORT
[ -z "$admin_db" ] || port_free admin "$admin_port" SMOKE_ADMIN_PORT

# ---- 1. build as the Dockerfiles do ----
# Only the Dockerfile's ENV and RUN line reach the build: no GOFLAGS,
# CGO_ENABLED or tags from the caller, so a Dockerfile that drops one fails
# here the way its image would. /out/ becomes the work dir.
#
# The static check reads the linux/amd64 build, the binary the image ships:
# a host build proves nothing (a macOS binary always links libSystem, and
# its Mach-O has no ELF interpreter to find). It fails when it cannot tell,
# never passes: a missing checker once made a dynamic binary "static".
step build
readonly image_os=linux image_arch=amd64
command -v file >/dev/null || fail "file(1) is not installed: the static-link check needs it (apt install file / it ships with macOS)"
keep=(PATH="$PATH" HOME="$HOME" GOPATH="$(go env GOPATH)" GOCACHE="$(go env GOCACHE)"
  GOMODCACHE="$(go env GOMODCACHE)" GOTOOLCHAIN="$(go env GOTOOLCHAIN)")
[ -z "${GOTMPDIR:-}" ] || keep+=(GOTMPDIR="$GOTMPDIR")
host_build=true
[ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" != "$image_os/$image_arch" ] || host_build=false
mkdir "$work/image"
# static binary dockerfile: fails unless the image's binary is a statically
# linked linux/amd64 ELF, by file(1), go version -m, and readelf when present.
static() {
  local binary=$1 dockerfile=$2 name=${1##*/} described settings
  described=$(file -b "$binary") || fail "$name: file(1) could not read it"
  case "$described" in
  ELF\ 64-bit*x86-64*) ;;
  *) fail "$name: expected a linux/amd64 ELF to check, file(1) says: $described" ;;
  esac
  case "$described" in
  *"dynamically linked"* | *interpreter*) dynamic "$binary" "$dockerfile" "file(1): $described" ;;
  *"statically linked"*) ;;
  *) fail "$name: cannot tell whether it is statically linked, file(1) says: $described" ;;
  esac
  settings=$(go version -m "$binary") || fail "$name: go version -m could not read it"
  if ! grep -qx $'\tbuild\tGOOS='"$image_os" <<<"$settings" || ! grep -qx $'\tbuild\tGOARCH='"$image_arch" <<<"$settings"; then
    fail "$name: go version -m does not say GOOS=$image_os GOARCH=$image_arch, so the check read the wrong build"
  fi
  if command -v readelf >/dev/null && readelf -lW "$binary" | grep -q 'program interpreter'; then
    dynamic "$binary" "$dockerfile" "readelf: it has a program interpreter"
  fi
}
dynamic() { # binary dockerfile evidence
  ! command -v readelf >/dev/null || readelf -dW "$1" | grep NEEDED >&2 || true
  fail "${1##*/} is dynamically linked ($3): it would not start on distroless/static ($2). A dependency needs cgo or loads libraries at run time (e.g. purego): build with the tag that keeps it static, in the Dockerfile's ENV GOFLAGS or its go build line"
}
mapfile -t builds < <(./scripts/dockerfile-builds.sh)
[ "${#builds[@]}" -gt 0 ] || fail "no Dockerfile with a 'RUN ... go build' line"
for build in "${builds[@]}"; do
  IFS=$'\x1f' read -r dockerfile build_env run output _ <<<"$build"
  name=${output#/out/}
  # build_env is the Dockerfile's ENV text: word splitting intended.
  echo "$dockerfile ($image_os/$image_arch): ${build_env:+$build_env }$run"
  # shellcheck disable=SC2086
  env -i "${keep[@]}" GOOS=$image_os GOARCH=$image_arch $build_env bash -c "${run//\/out\//$work/image/}" ||
    fail "$dockerfile: the $image_os/$image_arch build failed"
  [ -f "$work/image/$name" ] || fail "$dockerfile: the build did not write $output"
  static "$work/image/$name" "$dockerfile"
  echo "$name: statically linked $image_os/$image_arch"
  if [ "$host_build" = true ]; then
    # shellcheck disable=SC2086
    env -i "${keep[@]}" $build_env bash -c "${run//\/out\//$work/}" || fail "$dockerfile: the host build failed"
  else
    cp "$work/image/$name" "$work/$name"
  fi
  [ -x "$work/$name" ] || fail "$dockerfile: the build did not write $output"
done

# ---- 2. migrate like pre-deploy ----
admin=false
[ -x "$work/admin" ] && [ -n "$admin_db" ] && admin=true
web_env=(APP_ENV=production PORT="$web_port" PUBLIC_URL="http://127.0.0.1:$web_port"
  DATABASE_URL="$app_db" AUTH_SECRET=release-smoke-web-auth-secret-0123456789 RESEND_API_KEY=re_release_smoke_unused
  FROM_EMAIL="$smoke_from_email")
admin_env=(APP_ENV=production ADMIN_PORT="$admin_port" ADMIN_PUBLIC_URL="http://127.0.0.1:$admin_port"
  DATABASE_URL="$app_db" ADMIN_DATABASE_URL="$admin_db" ADMIN_AUTH_SECRET=release-smoke-admin-auth-secret-0123456789
  RESEND_API_KEY=re_release_smoke_unused FROM_EMAIL="$smoke_from_email")
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
# healthy accepts a 200 only from `pid`, the server this run started: the
# port answered nobody before it started (port_free), and the process must
# still run once the 200 arrived; one that exited fails, whatever answered.
healthy() { # name port log pid
  local name=$1 port=$2 log=$3 pid=$4
  local deadline=$((SECONDS + boot_seconds)) code=none
  while [ "$SECONDS" -lt "$deadline" ]; do
    kill -0 "$pid" 2>/dev/null || break # it exited
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz" || true)
    if [ "$code" = "$http_ok" ]; then
      kill -0 "$pid" 2>/dev/null || break # it exited: the 200 was not its
      echo "$name: /healthz $http_ok from pid $pid"
      return 0
    fi
    sleep "$poll_seconds"
  done
  tail -n "$log_tail_lines" "$log" >&2
  if ! kill -0 "$pid" 2>/dev/null; then
    fail "$name exited before it answered /healthz $http_ok (last: $code; its log is above)"
  fi
  fail "$name did not answer /healthz $http_ok within ${boot_seconds}s (last: $code)"
}
step boot
# Started straight from this shell, not through run(): a backgrounded function
# is a subshell, so $! would be that subshell's PID. cleanup would then kill
# only the subshell and leave the server running on the port (the leftover
# that answered later runs), and healthy would watch the wrong process. env
# execs the binary, so $! is the server itself.
start() { # log command...
  local log=$1
  shift
  env -i PATH="$PATH" HOME="$HOME" "$@" >"$log" 2>&1 &
  pids+=($!)
}
start "$work/web.log" "${web_env[@]}" "$work/server" serve
[ "$admin" = false ] || start "$work/admin.log" "${admin_env[@]}" "$work/admin" serve
healthy web "$web_port" "$work/web.log" "${pids[0]}"
[ "$admin" = false ] || healthy admin "$admin_port" "$work/admin.log" "${pids[1]}"
step "release smoke ok"
