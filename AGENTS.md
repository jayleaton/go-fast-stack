# AGENTS.md: instructions for AI assistants

This is a **Go + Datastar monorepo template**: one Go module, one static binary, server-rendered `templ` pages with Datastar, a JSON API for the mobile app, Postgres through `pgx` + `sqlc` + `goose`, and Go-native auth modelled on Better Auth. It replaces the Next.js template `jayleaton/ai-patterns-monorepo` and keeps the same patterns.

Shared vocabulary lives in [CONTEXT.md](CONTEXT.md). Use those words.

## What makes this template good

1. **Template first.** People clone this and diverge right away. Keep it generic and minimal. The example flows (the users API, the dashboard) are documentation as code: keep them canonical and small. If a change only makes sense for one project, it does not belong here.
2. **Performance without compromise.** Render on the server, keep client JavaScript to what Datastar needs, and treat binary size, memory and CI time as features. Prefer the standard library; every dependency needs a reason.
3. **Network-ready dev servers.** The server binds `0.0.0.0:3000`, so it works the same on localhost, the LAN, and Tailscale (`http://<machine-hostname>:3000`). Never bake `localhost` into code, emails or auth callbacks.
4. **Monorepo-aware.** `packages/database`, `packages/auth`, `apps/web` and `apps/mobile-app` move together. Every change gets a decision per workspace, even if it is "no change needed".

These are good defaults, not hard rules. The developer's preferences override anything here.

## Architecture (mandatory)

The template's value is its layers. Do not route around them.

`packages/database` (migrations → sqlc queries → repository) → `apps/web/internal/services` → transports: `apps/web/internal/api` (JSON) and `apps/web/ui` (HTML/Datastar).

- Each layer talks only to the layer below. Both transports call services **in-process**; nothing calls its own API over HTTP.
- Transports never import `pgx`, sqlc output or repositories. Services never import `net/http`, templ or Datastar. `packages/database` never imports `apps/`. `depguard` in `.golangci.yml` enforces this, and `make lint-layering` proves the rules still bite.
- Services return `*services.Error{Kind, Message}`. The transport maps the kind to a status code; the message is safe to show.
- JSON responses always use the envelope `{"data": T|null, "error": string|null}` (plus `details` on validation errors). Use `api.Public` / `api.Authenticated`. A handler is about five lines: decode, call the service, return.
- Input validation lives in `apps/web/internal/validate`, one type per input with CRUD names (`UpdateUserSettings`). Decode with `validate.Decode[T]`. Never validate inline.
- Routes and toggles live in `apps/web/internal/config`: `AppRoutes`, `APIRoutes`, and `Features` (email verification off in dev, on in production; `TemplateWelcome`). Never write a path literal or branch on the environment anywhere else.
- Env is read once, in `config.Load`, and validated at boot. Nothing else calls `os.Getenv`.
- `packages/auth` owns the `user`, `session`, `account` and `verification` tables (Better Auth names). App migrations may add tables that reference `"user"(id)`, and repositories may update non-credential user columns (`name`, `image`). Auth migrations run before app migrations (`goose_auth`, then `goose_app`).
- `apps/web/internal/server` and `cmd/server` are the only places that wire packages together.

## Hit every layer

The most common defect is a change that works on the path you tested and is missing everywhere else. Before calling work done, walk this list and say which entries applied:

- **Entry points.** A new page needs its handler, its route in `config.AppRoutes`, a Datastar loading indicator, and translations in every locale. A new endpoint needs its route in `config.APIRoutes`.
- **Workspaces.** A feature usually spans `packages/database` (migration, query, repository) plus each surface that shows it: `apps/web/ui`, `apps/web/internal/api`, and `apps/mobile-app`. Doing one half is doing half the feature.
- **Contracts.** Migrations and sqlc queries are the contract. Change them, run `make generate`, and follow every consumer: repository, service, validator, both transports.
- **Reverse states.** If you add a way in, add the way out and a way to see it. Delete needs restore, disable needs enable, set needs clear.
- **Environments.** Dev (Docker Postgres, email verification off, email links logged) and production (Resend, verification on) differ only through `config.Features` and env. Both paths must work.
- **Docs.** Behaviour a template user would notice goes in `README.md`; patterns go here.

## The three ways to hurt yourself

**Killing by pattern.** Never `pkill -f`, `pgrep | kill`, or kill a PID found by matching a name, path or port. Many servers run on this machine and yours can match. Kill only the PID you captured at spawn.

**Trusting the wrong database.** The Docker Postgres from `make db-up` is the sandbox: seed it, reset it, break it. A hosted database (Railway, Neon, production) is never a playground: don't point dev at one, migrate one, or copy real data in or out unless explicitly asked. `make db-down` keeps the volume; `docker compose down -v` deletes it.

**Baking in origins.** Never hardcode `http://localhost:3000` in code, emails or auth callbacks. Origins come from `PUBLIC_URL`, `INTERNAL_URL` and `TRUSTED_ORIGINS` through `config.Config`, or every non-local client breaks.

## Dev loop

- `make db-up && make migrate && make dev` starts Postgres + pgweb (bound to 127.0.0.1), runs migrations, and serves on `0.0.0.0:3000`. `.env` is created from `.env.example` on first run.
- Each checkout (clone or git worktree) gets its own compose project and dev database ports, derived from its path; the test Postgres takes a free port on every start. Parallel worktrees never share or collide. `make env` prints the values; override `COMPOSE_PROJECT_NAME`, `POSTGRES_PORT`, `PGWEB_PORT`, `TEST_POSTGRES_PORT` or `DATABASE_URL` in `.env` only when you need fixed ones. Always go through `make`: bare `docker compose` uses the shared default project. `PORT` (the app) is not derived; set it in `.env` when two worktrees run `make dev` at once.
- Auth is real in every environment. In development email verification is off and the dev mailer logs verification and reset links to the server log, so sign up through `/api/auth/sign-up/email` (or the UI) and copy links from the log. `packages/auth/README.md` has the endpoints, the security model and how to port more Better Auth features.
- A running dev server is fine to leave up. Stop only what you started, by PID.
- Inspect data with pgweb (URL from `make env`). Seed realistic data before verifying a user-facing flow; an empty database is a bad test. Secrets go in `.env` only when the flow needs them, and never get committed.

## Verifying

- `make lint test` before every commit. `make ci` runs exactly what CI runs: build, generated-code diff, gofmt + golangci-lint, the layering proof, and `go test -race` against a throwaway Postgres.
- Run the narrowest thing first: `go test ./apps/web/internal/services/...`.
- Behaviour changes ship with focused tests. Services and handlers test against `services/servicetest` fakes; repositories test against real Postgres with `dbtest.New(t, dbtest.App)` (each test gets its own cloned database, so use `t.Parallel()`).
- A test that needs a sleep or timeout to pass is wrong.
- User-visible UI changes get one pass in a real browser, over the LAN or Tailscale URL when origins matter.

## Pull requests

- Never open a PR unless the developer asks.
- Conventional commit titles in plain language: `fix(api): PATCH /users no longer drops the image`.
- Body: the problem in a sentence or two, then the fix. UI changes need before/after images; motion needs a short video.
- One concern per PR. If the description says "also", split it.
- `ci-ok` is the single required check. When babysitting CI, verify each finding against the source, fix real ones, and dismiss false positives with a reason.

## Where code lives

- `apps/web/cmd/server`: the binary (`server serve`, `server migrate`).
- `apps/web/internal/config`: env config, `AppRoutes`/`APIRoutes`, `Features`.
- `apps/web/internal/server`: middleware, `/healthz`, and the mounts (auth at `/api/auth/`, API, UI at `/`).
- `apps/web/internal/api`: JSON transport `/api/core/v1`: envelope, route wrapper, handlers.
- `apps/web/internal/services`: business rules. `apps/web/internal/validate`: input types.
- `apps/web/ui`: templ pages, Datastar handlers, i18n, static assets.
- `packages/database`: pool, `migrations/` (goose, embedded), `queries/` (sqlc input), `sqlc/` (generated, never edit), repositories, `model/`, `dbtest/`.
- `packages/auth`: Better Auth–style auth: its own migrations, queries, endpoints (Better Auth paths and response shapes, not the envelope), sessions, mailers. Self-contained: it never imports `apps/` or `packages/database` (depguard).
- `apps/mobile-app`: the Expo app, which talks to the JSON API with bearer tokens.
- Root: `Makefile`, `docker-compose.yml`, `Dockerfile`, `railway.json`, `.golangci.yml`, `tools/` (pinned dev tools, run via `go tool -modfile=tools/go.mod`).

## Taste

- Complexity belongs at the layer boundary. Repositories hide SQL, services hide business rules, the route wrapper hides auth and envelopes, and templates stay dumb.
- Small interfaces, defined by the consumer (`services.UserRepository`). Concrete types out, interfaces in.
- Typed values over `any` and `map[string]any`. Generics only where they remove real repetition.
- Errors are values: wrap with context, compare with `errors.Is`/`errors.As`, and never log and return the same error.
- Comments say how a thing is used and move with the code. Document functions, not every line.
- If a rule here fights the task in front of you, say so loudly and get a human sign-off before breaking it.
