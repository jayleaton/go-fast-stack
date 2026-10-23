# Development

## Try the API

Try the API (it is for the mobile app and servers: a browser gets a 404, see [Security](#security)). In development email verification is off and verification/reset links are logged by the server instead of emailed:

```sh
curl localhost:3000/healthz
# Sign up (Better Auth endpoint and response shape); the body carries the session token.
curl -H 'Content-Type: application/json' \
  -d '{"name":"Ada","email":"ada@example.com","password":"password123"}' \
  localhost:3000/api/auth/sign-up/email
# Clients send that token as a bearer token.
curl -H "Authorization: Bearer <token>" localhost:3000/api/core/v1/users/me
curl -H "Authorization: Bearer <token>" -X PATCH -d '{"name":"Ada Lovelace"}' localhost:3000/api/core/v1/users/me
curl -H "Authorization: Bearer <token>" -X POST localhost:3000/api/auth/sign-out
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
| `make test` | `go test -race ./...` (services, route handlers, and `packages/auth` against this checkout's throwaway tmpfs Postgres or `TEST_DATABASE_URL`), then per-package and per-test time budgets and per-boundary coverage floors |
| `make lint` | gofmt + golangci-lint (layering and test-import rules) + test file locations |
| `make lint-layering` / `make lint-tests` | prove the layering, test-location, coverage and test-time rules still reject planted violations |
| `make check-fast` | before every push: gofmt, vet, golangci-lint and plain tests for only what your changes vs origin/main affect (seconds) |
| `make hooks` / `make hooks-remove` | install / remove a pre-push hook that runs `make check-fast` (covers every worktree) |
| `make ci` | everything CI runs |
| `make generate` | sqlc, mocks (`go generate`) and templ code generation |
| `make css` | rebuild both apps' `app.css` (pinned Tailwind standalone CLI) |
| `make bench-devloop` | timing tables for the dev loop: build per binary (with size and cold start), generators, lint per linter, probes, tests per package with and without `-race`; `BENCH_COLD=1` adds cold caches, `BENCH_BASE=<ref>` an A/B of the test run against that ref on the same machine |
| `make build` / `make docker` | static binary / production image |
| `make db-up` / `make db-down` | start / stop the local databases |
| `make db-backup` / `make db-backups` / `make db-restore ID=… YES=1` | encrypted backup of this checkout's Postgres, list, restore (see "Backups and restore") |
| `make railway-backup` / `railway-backups` / `railway-restore` | the same against a Railway environment (`PLATFORM_ENV=production`) |
| `make env` | print this checkout's compose project, ports and `DATABASE_URL` |

## Look and feel

Both apps render through one component set, [packages/uikit](packages/uikit/README.md) (app shell with a responsive
nav, buttons, fields, cards, alerts, badges, tables, confirm dialogs, empty and loading states), styled by design
tokens with a light and a dark value each. **Re-theme by editing [packages/uikit/theme.css](packages/uikit/theme.css)
and running `make css`.** Pages follow the system theme until someone uses the toggle (a `theme` cookie the server
renders as `<html class>`, so there is no flash). The product name in the header and titles is `APP_NAME` (default
"My App"); the admin app's is `ADMIN_APP_NAME` (default "Admin").

## Stack

Go stdlib `net/http` · `templ` + Datastar + Tailwind (standalone CLI) · `pgx` + `sqlc` + `goose` · Postgres 18 · Resend · Railway (distroless image, about 5 MB).

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, test boundaries, how to add a service, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
