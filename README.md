# ai-patterns-go

A Go + Datastar monorepo template: one static binary serving server-rendered `templ` pages, a JSON API, and Better Auth–style auth on Postgres. It succeeds [ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo) and keeps the same layered patterns.

## Quick start

Needs Go (version in `mise.toml`; `mise install`) and Docker.

```sh
make db-up      # Postgres 18 + pgweb on 127.0.0.1 (creates .env from .env.example)
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
curl -b jar localhost:3000/api/core/v1/users
curl -b jar -H 'Origin: http://localhost:3000' -X PATCH -d '{"name":"Ada Lovelace"}' localhost:3000/api/core/v1/users
# Mobile clients send the token from the sign-in/sign-up body as a bearer token.
curl -H "Authorization: Bearer <token>" localhost:3000/api/core/v1/users
curl -b jar -H 'Origin: http://localhost:3000' -X POST localhost:3000/api/auth/sign-out
```

Auth endpoints live under `/api/auth/` (sign-up, sign-in, sign-out, get-session, verify-email, send-verification-email, request-password-reset, reset-password); see [packages/auth/README.md](packages/auth/README.md).

Ports taken? Set `POSTGRES_PORT`, `PGWEB_PORT`, `TEST_POSTGRES_PORT` and `PORT` in `.env` (and the port in `DATABASE_URL`).

## Commands

| | |
|---|---|
| `make dev` | run the server with `.env` |
| `make test` | `go test -race ./...` against a throwaway tmpfs Postgres |
| `make lint` | gofmt + golangci-lint (includes layering rules) |
| `make ci` | everything CI runs |
| `make generate` | sqlc (and templ) code generation |
| `make build` / `make docker` | static binary / production image |
| `make db-up` / `make db-down` | start / stop the local databases |

## Stack

Go stdlib `net/http` · `templ` + Datastar + Tailwind (standalone CLI) · `pgx` + `sqlc` + `goose` · Postgres 18 · Resend · Railway (distroless image, about 5 MB).

## Deploy (Railway)

`railway.json` builds the `Dockerfile`, runs `/server migrate` as the pre-deploy command, and health-checks `/healthz`. Set `APP_ENV=production`, `PUBLIC_URL`, `DATABASE_URL`, `AUTH_SECRET` (32+ chars), `RESEND_API_KEY` and `FROM_EMAIL`, plus `CLIENT_IP_HEADER=X-Real-IP` so auth rate limits see the client instead of Railway's proxy. Production turns on email verification. The server refuses to boot with a missing or invalid value and lists every problem.

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, verification, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
