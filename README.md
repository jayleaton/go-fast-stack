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

`railway.json` builds the `Dockerfile`, runs `/server migrate` as the pre-deploy command, and health-checks `/healthz`. Set `APP_ENV=production`, `PUBLIC_URL`, `DATABASE_URL`, `AUTH_SECRET` (32+ chars), `RESEND_API_KEY` and `FROM_EMAIL`, plus `CLIENT_IP_HEADER=X-Real-IP` so auth rate limits see the client instead of Railway's proxy. Production turns on email verification. The server refuses to boot with a missing or invalid value and lists every problem.

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, test boundaries, how to add a service, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
