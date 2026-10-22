# ai-patterns-go

A Go + Datastar monorepo template: one static binary serving server-rendered `templ` pages, a JSON API, and Better Auth–style auth on Postgres. It succeeds [ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo) and keeps the same layered patterns.

## Quick start

Needs Go (version in `mise.toml`; `mise install`) and Docker.

```sh
make db-up      # Postgres 18 + pgweb on 127.0.0.1, ports per checkout (creates .env)
make migrate    # auth migrations, then app migrations
make dev        # http://<this-machine>:3000, reachable over LAN/Tailscale
```

Try the API. In development email verification is off and verification/reset links are logged by the server instead of emailed:

```sh
curl localhost:3000/healthz
# Sign up (Better Auth endpoint and response shape); keeps the session cookie.
curl -c jar -H 'Content-Type: application/json' \
  -d '{"name":"Ada","email":"ada@example.com","password":"password123"}' \
  localhost:3000/api/auth/sign-up/email
curl -b jar localhost:3000/api/core/v1/users/me
curl -b jar -H 'Origin: http://localhost:3000' -X PATCH -d '{"name":"Ada Lovelace"}' localhost:3000/api/core/v1/users/me
# Mobile clients send the token from the sign-in/sign-up body as a bearer token.
curl -H "Authorization: Bearer <token>" localhost:3000/api/core/v1/users/me
curl -b jar -H 'Origin: http://localhost:3000' -X POST localhost:3000/api/auth/sign-out
```

The signed-in user's account lives under `/api/core/v1/users/me` (all JSON, envelope `{"data", "error"}`):

| Route | |
|---|---|
| `GET /me`, `PATCH /me` | profile (`{"name", "image"}`; `image: null` clears it) |
| `POST /me/email` | `{"newEmail", "callbackURL"?}`: emails a verification link to the new address; it changes when the link is opened |
| `POST /me/password` | `{"currentPassword", "newPassword", "revokeOtherSessions"}` |
| `GET /me/sessions`, `DELETE /me/sessions/{id}`, `DELETE /me/sessions/others` | list sessions (the request's own is `current`), sign one or all others out |
| `GET /me/data`, `DELETE /me` | what is stored, delete the account |

`PATCH /api/core/v1/users/{id}` answers 403 for anyone but the caller.

Auth endpoints live under `/api/auth/` (sign-up, sign-in, sign-out, get-session, verify-email, send-verification-email, request-password-reset, reset-password); see [packages/auth/README.md](packages/auth/README.md).

Each checkout gets its own compose project and database ports, derived from its path, so any number of clones or git worktrees can run `make db-up` and `make test` at once. `make env` prints them (pgweb URL included). To pin them, set `COMPOSE_PROJECT_NAME`, `POSTGRES_PORT`, `PGWEB_PORT`, `TEST_POSTGRES_PORT` or `DATABASE_URL` in `.env`. The app's `PORT` stays 3000 unless you set it.

## Commands

| | |
|---|---|
| `make dev` | run the server with `.env` |
| `make test` | `go test -race ./...` against this checkout's throwaway tmpfs Postgres (or `TEST_DATABASE_URL`), then per-boundary coverage floors |
| `make lint` | gofmt + golangci-lint (layering and test-import rules) + test file locations |
| `make lint-layering` / `make lint-tests` | prove the layering, test-location and coverage rules still reject planted violations |
| `make ci` | everything CI runs |
| `make generate` | sqlc, mocks (`go generate`) and templ code generation |
| `make build` / `make docker` | static binary / production image |
| `make db-up` / `make db-down` | start / stop the local databases |
| `make env` | print this checkout's compose project, ports and `DATABASE_URL` |

## Stack

Go stdlib `net/http` · `templ` + Datastar + Tailwind (standalone CLI) · `pgx` + `sqlc` + `goose` · Postgres 18 · Resend · Railway (distroless image, about 5 MB).

## Deploy (Railway)

`railway.json` builds the `Dockerfile`, runs `/server migrate` as the pre-deploy command, and health-checks `/healthz`. Set `APP_ENV=production`, `PUBLIC_URL`, `DATABASE_URL`, `AUTH_SECRET` (32+ chars), `RESEND_API_KEY` and `FROM_EMAIL`, plus `CLIENT_IP_HEADER=X-Real-IP` so auth rate limits see the client instead of Railway's proxy. Production turns on email verification. The server refuses to boot with a missing or invalid value and lists every problem. To rotate `RESEND_API_KEY` without a redeploy, serve it from Doppler instead (next section).

## Secrets

Secrets that can change while the server runs come from one place, `SECRETS_PROVIDER`, through [`packages/secrets`](packages/secrets): a manager that fetches the whole set for this service and environment, validates it, and hands each consumer its value. Everything else stays a plain env var read once at boot.

| `SECRETS_PROVIDER` | What it is | Live rotation |
|---|---|---|
| `env` (default) | values from the process env (`.env`, Railway variables), read once at boot | no: changing a value means a redeploy |
| `doppler` | polls one [Doppler](https://www.doppler.com) config with a read-only service token, over plain `net/http` | yes, within `SECRETS_REFRESH_INTERVAL` |

**What rotates live (v1).**

| Secret | Source | Rotates without redeploy | Why not (yet) |
|---|---|---|---|
| `RESEND_API_KEY` | `SECRETS_PROVIDER` | yes, with `doppler`: the mailer rebuilds its Resend client on change | |
| `DATABASE_URL` | env | no | a new password needs a new `pgx` pool that authenticates and passes a probe before an atomic swap and a drain of the old pool; Railway Postgres credentials change through a redeploy anyway |
| `AUTH_SECRET` | env | no | it signs session cookies and verification/reset tokens: replacing it in place signs everyone out and breaks links already sent. Rotation needs a keyring (new key signs, old keys still verify during an overlap) |
| `DOPPLER_TOKEN` | env (sealed) | no | it is the credential that reaches the provider; update the Railway variable (redeploy), then revoke the old token |

**Doppler setup for a team.**

1. Create a Doppler workspace and a project (e.g. `myapp`). Each environment (`dev`, `stg`, `prd`) is a config; add `RESEND_API_KEY` to each. The Developer plan is free for three people and service tokens are free.
2. Per Railway environment, create a **read-only service token** for that environment's config (project → config → Access → Generate). One token per service per environment, so staging can't read production and a future admin service gets its own config.
3. In Railway, on the web service of each environment, set `SECRETS_PROVIDER=doppler` and `DOPPLER_TOKEN=<that token>` (sealed). Keep `DATABASE_URL`, `AUTH_SECRET` and the rest as Railway variables. Drop `RESEND_API_KEY` from Railway: with `doppler` the env copy is ignored.
4. Deploy. The log shows `secrets: loaded provider=doppler`. From now on, changing `RESEND_API_KEY` in Doppler reaches every replica within one interval (60 s ± 10 %): `secrets: rotated changed=[RESEND_API_KEY]`, then `mailer: resend client built resend_api_key=fp:…`. Revoke the old Resend key once every replica logged the new fingerprint.
5. Leave Doppler's Railway sync off for live keys: it writes Railway variables and redeploys, which is the `env` path.

**When the provider misbehaves.** No valid first snapshot means the server does not boot (the deploy fails; the previous one keeps serving). Once running, a failed or invalid fetch (malformed body, missing `RESEND_API_KEY` in production) never replaces the last good values; the manager retries with backoff (5 s doubling, capped at min(5 min, max staleness ÷ 3), honouring `Retry-After`). After `SECRETS_MAX_STALENESS` without a good fetch, `/healthz` answers 503 `secrets stale` and every failed poll logs an error. Clients already built keep their last good key. `/healthz` is a readiness check: don't wire it to a liveness probe that restarts the process, because a fresh process can't load secrets during the outage either.

**Logs never contain values.** A secret prints as `[redacted]` everywhere (fmt, slog, JSON). Logs name changed keys and show a fingerprint (an HMAC under a random per-process key: it shows a change within one process, nothing more).

**Local development.** Keep `SECRETS_PROVIDER=env` and `.env`, or run `doppler run -- make dev` to inject a dev config as env at startup. To try live rotation locally, put a dev-config service token in `.env` with `SECRETS_PROVIDER=doppler`.

| Variable | Default | |
|---|---|---|
| `SECRETS_PROVIDER` | `env` | `env` or `doppler` |
| `DOPPLER_TOKEN` | | required with `doppler`: read-only service token for this service's config |
| `DOPPLER_API_URL` | `https://api.doppler.com` | https in production; point it at a fake for tests |
| `SECRETS_REFRESH_INTERVAL` | `60s` | at least `10s` (Doppler Developer allows 120 reads/min per workspace) |
| `SECRETS_MAX_STALENESS` | `15m` | at least twice the interval |

Infisical and 1Password adapters are stubs with an implementation plan (`packages/secrets/infisical.go`, `onepassword.go`, outside the default build).

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, test boundaries, how to add a service, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
