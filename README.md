# ai-patterns-go

A Go + Datastar monorepo template: one static binary serving server-rendered `templ` pages, a JSON API, and Better Auth–style auth on Postgres. It succeeds [ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo) and keeps the same layered patterns.

## Quick start

Needs Go (version in `mise.toml`; `mise install`) and Docker.

```sh
make db-up      # Postgres 18 + pgweb on 127.0.0.1 (creates .env from .env.example)
make migrate    # auth migrations, then app migrations
make dev        # http://<this-machine>:3000, reachable over LAN/Tailscale
```

Try the API (in development every request is signed in as a seeded dev user until auth is wired):

```sh
curl localhost:3000/healthz
curl localhost:3000/api/core/v1/users
curl -X PATCH -d '{"name":"Ada"}' localhost:3000/api/core/v1/users
```

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

`railway.json` builds the `Dockerfile`, runs `/server migrate` as the pre-deploy command, and health-checks `/healthz`. Set `APP_ENV=production`, `PUBLIC_URL`, `DATABASE_URL`, `AUTH_SECRET` (32+ chars), `RESEND_API_KEY` and `FROM_EMAIL`. The server refuses to boot with a missing or invalid value and lists every problem.

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, verification, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
