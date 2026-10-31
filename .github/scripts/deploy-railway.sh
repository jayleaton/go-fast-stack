#!/bin/sh
# Deploys the checked-out commit to one Railway environment and proves it
# healthy (docs/RELEASES.md). Used by "Deploy staging" and "Deploy production";
# the workflow checks out the commit and holds the token.
#
#   sh .github/scripts/deploy-railway.sh <environment> <sha>
#
# The services and their order come from deploy/platform.yaml through
# deploy/cmd/deploy-services: web, then admin when admin is on, then the
# backup cron when backups are on. Each is uploaded with `railway up` (Railway
# builds its Dockerfile from this checkout; the deployment message names the
# commit) and waited for before the next one starts. There is no separate
# migrate step: each service's Railway pre-deploy command migrates its
# database (/server migrate for web, /admin migrate for admin), and /healthz
# gates the deployment, so a failed migration fails that service's deployment
# and stops the rollout. Once every service is up, the public /healthz
# endpoints must answer 200 (retried for a few minutes).
#
#   RAILWAY_TOKEN    Railway project token scoped to <environment>. A project
#                    token already names its project and environment, so no
#                    --environment flag is passed (with one, the CLI cannot
#                    look environments up and answers "Environment not
#                    found"). Without a token (a person signed in with
#                    `railway login`, in a linked checkout) --environment
#                    <environment> is added, and --project when
#                    RAILWAY_PROJECT_ID is set.
#   WEB_URL          public web origin; <WEB_URL>/healthz must answer 200
#   ADMIN_URL        optional public admin origin; checked when set
#   DEPLOY_SERVICES  optional override of the deploy-services list
#   DEPLOY_TIMEOUT   seconds to wait for each deployment (default 1800)
#   READY_ATTEMPTS, READY_INTERVAL  readiness retries (default 6 x 30s)
#   DRY_RUN=1        print every railway and curl command instead of running
#                    it: what the workflows do until RAILWAY_DEPLOY is true
#
# Never pass a variable named RAILWAY_ENV: it switches the Railway CLI to
# Railway's internal API (docs/railway-go-iac-issues.md).
set -eu

usage="usage: sh .github/scripts/deploy-railway.sh <environment> <sha>"
environment=${1:?$usage}
sha=${2:?$usage}
dry=false
[ "${DRY_RUN:-}" = 1 ] && dry=true

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

if [ "$dry" = true ]; then
  echo "DRY RUN: printing the railway and curl commands instead of running them."
fi

# The upload is this checkout, so it must be the commit being deployed.
head=$(git rev-parse HEAD)
if [ "$head" != "$sha" ]; then
  if [ "$dry" = true ]; then
    echo "::warning::the checkout is $head, not $sha; a real deploy refuses this"
  else
    echo "::error::the checkout is $head, not $sha; nothing was deployed"
    exit 1
  fi
fi

if [ -z "${RAILWAY_TOKEN:-}" ] && [ -n "${GITHUB_ACTIONS:-}" ] && [ "$dry" = false ]; then
  upper=$(echo "$environment" | tr '[:lower:]' '[:upper:]')
  echo "::error::RAILWAY_TOKEN is empty. Add a Railway project token scoped to $environment as the RAILWAY_${upper}_TOKEN secret of the $environment GitHub environment (docs/RELEASES.md); nothing was deployed"
  exit 1
fi

target=""
if [ -z "${RAILWAY_TOKEN:-}" ] && [ "$dry" = false ]; then
  target="--environment $environment"
  [ -n "${RAILWAY_PROJECT_ID:-}" ] && target="$target --project $RAILWAY_PROJECT_ID"
fi

services=${DEPLOY_SERVICES:-$(cd deploy && go run ./cmd/deploy-services -config platform.yaml)}
if [ -z "$services" ]; then
  echo "::error::deploy-services listed no services for $environment"
  exit 1
fi

timeout=${DEPLOY_TIMEOUT:-1800}
poll=15

# show CMD...: the command as it would run.
show() {
  printf '+'
  for arg in "$@"; do
    case $arg in
      *[!A-Za-z0-9_./:=@-]* | '') printf " '%s'" "$arg" ;;
      *) printf ' %s' "$arg" ;;
    esac
  done
  printf '\n'
}

# newest_id SERVICE: the id of the service's newest deployment ("" when none
# or unreadable).
newest_id() {
  # shellcheck disable=SC2086
  railway deployment list $target --service "$1" --json 2>/dev/null | jq -r '.[0].id // ""' 2>/dev/null || true
}

# upload SERVICE: starts the deployment and prints the CLI's output. An
# upload can fail transiently at Railway's edge, so one answering a 5xx is
# retried twice with a pause; any other failure stops at once.
upload() {
  attempt=1
  while :; do
    # shellcheck disable=SC2086
    if out=$(railway up $target --service "$1" --detach --verbose -m "deploy $sha" 2>&1); then
      echo "$out"
      return 0
    fi
    echo "$out" >&2
    if [ "$attempt" -lt 3 ] && echo "$out" | grep -q 'status code 5[0-9][0-9]'; then
      echo "Upload for $1 failed, retrying in $((attempt * 20))s" >&2
      sleep $((attempt * 20))
      attempt=$((attempt + 1))
      continue
    fi
    echo "::error::upload for $1 failed" >&2
    return 1
  done
}

# wait_for SERVICE ID: polls the deployment until it settles.
wait_for() {
  svc=$1
  id=$2
  waited=0
  while [ "$waited" -lt "$timeout" ]; do
    # A read that fails or is not JSON says nothing about the deployment, so
    # the next poll tries again.
    # shellcheck disable=SC2086
    status=$(railway deployment list $target --service "$svc" --json 2>/dev/null |
      jq -r --arg id "$id" '(map(select(.id == $id))[0].status) // "UNKNOWN"' 2>/dev/null) || status=UNREAD
    case $status in
      SUCCESS)
        echo "$svc $id SUCCESS"
        return 0
        ;;
      SLEEPING)
        # The backup cron sleeps between runs once deployed.
        if [ "$svc" = backup ]; then
          echo "$svc $id SLEEPING (a cron between runs)"
          return 0
        fi
        ;;
      FAILED | CRASHED | REMOVED | SKIPPED)
        echo "::error::$svc deployment $id in $environment ended $status (railway logs --service $svc --deployment $id)"
        return 1
        ;;
      UNREAD) echo "::warning::could not read $svc's deployment status; retrying" ;;
    esac
    sleep "$poll"
    waited=$((waited + poll))
  done
  echo "::error::$svc deployment $id in $environment did not finish within ${timeout}s"
  return 1
}

# deploy SERVICE: upload, find the new deployment's id, wait for it.
deploy() {
  svc=$1
  case $svc in
    web) echo "== $svc: Railway runs /server migrate as its pre-deploy command; a failed migration fails this deployment" ;;
    admin) echo "== $svc: Railway runs /admin migrate as its pre-deploy command; a failed migration fails this deployment" ;;
    *) echo "== $svc" ;;
  esac
  if [ "$dry" = true ]; then
    # shellcheck disable=SC2086
    show railway up $target --service "$svc" --detach --verbose -m "deploy $sha"
    # shellcheck disable=SC2086
    show railway deployment list $target --service "$svc" --json
    echo "  (polled every ${poll}s until the new deployment is SUCCESS$([ "$svc" = backup ] && echo ' or SLEEPING'), for up to ${timeout}s)"
    return 0
  fi
  before=$(newest_id "$svc")
  out=$(upload "$svc") || return 1
  # The CLI names the deployment in its output (id=<uuid>, or "id" in JSON).
  id=$(echo "$out" | grep -oE 'id=[0-9a-f-]{36}' | head -1 | cut -d= -f2) || true
  [ -n "$id" ] || id=$(echo "$out" | grep -oE '"(deployment)?[iI]d" *: *"[0-9a-f-]{36}"' | head -1 | grep -oE '[0-9a-f-]{36}') || true
  if [ -z "$id" ]; then
    # Otherwise the deployment is the newest one that was not there before.
    tries=0
    while [ "$tries" -lt 20 ]; do
      id=$(newest_id "$svc")
      [ -n "$id" ] && [ "$id" != "$before" ] && break
      id=""
      tries=$((tries + 1))
      sleep 3
    done
  fi
  if [ -z "$id" ]; then
    echo "$out"
    echo "::error::cannot find the deployment railway up started for $svc"
    return 1
  fi
  echo "$svc started $id"
  wait_for "$svc" "$id"
}

# ready URL: GET URL until it answers 200, retried because readiness can lag
# a successful deployment.
ready() {
  if [ "$dry" = true ]; then
    show curl -s -o /dev/null -m 10 -w '%{http_code}' "$1"
    echo "  (retried up to ${READY_ATTEMPTS:-6} times, ${READY_INTERVAL:-30}s apart, until 200)"
    return 0
  fi
  attempts=${READY_ATTEMPTS:-6}
  interval=${READY_INTERVAL:-30}
  attempt=1
  while :; do
    code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$1") || true
    if [ "$code" = 200 ]; then
      echo "$1 200"
      return 0
    fi
    if [ "$attempt" -ge "$attempts" ]; then
      echo "::error::$1 answered $code after $attempts attempts"
      return 1
    fi
    echo "$1 answered $code; retrying in ${interval}s ($attempt/$attempts)"
    attempt=$((attempt + 1))
    sleep "$interval"
  done
}

upper=$(echo "$environment" | tr '[:lower:]' '[:upper:]')
echo "Deploying $(git log --oneline -1 "$sha" 2>/dev/null || echo "$sha") to Railway $environment"
echo "Services, in order: $services"
if [ "$dry" = false ]; then
  echo "Railway CLI: $(railway --version)"
fi

for svc in $services; do
  deploy "$svc" || {
    echo "::error::$environment deploy stopped at $svc; the services after it were not deployed"
    exit 1
  }
done

echo "== readiness"
if [ -n "${WEB_URL:-}" ]; then
  ready "${WEB_URL%/}/healthz"
elif [ "$dry" = true ]; then
  echo "::warning::WEB_URL is empty (the ${upper}_WEB_URL repository variable); a real deploy fails here"
  ready "<${upper}_WEB_URL>/healthz"
else
  echo "::error::WEB_URL is empty: set the ${upper}_WEB_URL repository variable to $environment's web origin"
  exit 1
fi
if [ -n "${ADMIN_URL:-}" ]; then
  ready "${ADMIN_URL%/}/healthz"
else
  case " $services " in
    *" admin "*) echo "::warning::ADMIN_URL is empty (the ${upper}_ADMIN_URL repository variable); admin's /healthz not checked" ;;
  esac
fi
echo "$environment runs $sha"
