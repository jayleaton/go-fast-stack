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
- Ports are defined by the consumer. A service declares the interfaces it needs (`services.UserRepository`, `services.AccountCredentials`); repositories in `packages/database` and adapters in `apps/web/internal/server` implement them. Transports in turn declare the service they call (`api.UserService`). Authorization (who may act on what) is a business rule: it lives in the service, never in a handler.
- Services return `*services.Error{Kind, Message}`, declared as named values (`services.ErrEmailTaken`) so callers match them with `errors.Is` (the UI picks a translation per error); the kind sentinels `ErrValidation`, `ErrForbidden`, `ErrNotFound`, `ErrConflict` match every error of their kind. The transport maps the kind to a status code; the message is safe to show. Adapters translate their package's errors into these values; transports never see raw `auth` or `pgx` errors.
- JSON responses always use the envelope `{"data": T|null, "error": string|null}` (plus `details` on validation errors). Use `api.Public` / `api.Authenticated`. A handler is about five lines: decode, call the service, return.
- Input validation lives in `apps/web/internal/validate`, one type per input with CRUD names (`UpdateUserSettings`). Decode with `validate.Decode[T]`. Never validate inline.
- Routes and toggles live in `apps/web/internal/config`: `AppRoutes`, `APIRoutes`, and `Features` (email verification off in dev, on in production; `TemplateWelcome`). Never write a path literal or branch on the environment anywhere else.
- Env is read once, in `config.Load`, and validated at boot. Nothing else calls `os.Getenv`. Secrets that must rotate without a redeploy are the one exception to "read once": `config.Load` builds a `packages/secrets` manager from `SECRETS_PROVIDER`, and consumers get those values only through typed accessors on `config.Secrets` (`DatabaseURL`/`OnDatabaseURL`, `AuthSecrets`/`OnAuthSecrets`, `OnResendAPIKey`; the admin app has its own), wired in `cmd/*`. Services and transports never import `packages/secrets` (depguard); they get the client a secret enables through a port.
- `packages/auth` owns the `user`, `session`, `account` and `verification` tables (Better Auth names). App migrations may add tables that reference `"user"(id)`, and repositories may update non-credential user columns (`name`, `image`). Auth migrations run before app migrations (`goose_auth`, then `goose_app`).
- `apps/web/internal/server` and `cmd/server` are the only places that wire packages together.
- **The browser only talks to the UI.** Pages and Datastar actions return HTML (fragments over SSE); the JSON API is for the mobile app and servers, and `server` answers browser requests to it (and `/healthz`) with a bare 404. Never link to, fetch or render an `/api/` path, a JSON payload, a token or internal error text from the UI. Emailed auth links for the UI land on UI pages (`auth.Config.VerifyEmailPath`, relative `redirectTo`).
- **UIs share `packages/uikit`.** Build pages from its components and semantic color utilities (`bg-surface`, `text-muted-foreground`, `border-input`, `bg-primary`, ...), never raw palette classes or `dark:` variants, so a theme stays a one-file change (`packages/uikit/theme.css`). The apps still never import each other; anything both need goes in uikit, which takes already translated strings.
- **Templates are CSP-clean.** `ui.Handler` sends a nonce CSP with no `unsafe-*`: no inline `style=`/`on*=` attributes or inline scripts (Datastar `data-*` expressions are fine; hide with `class="hidden"` + `data-class:hidden`). Never bypass templ's escaping (`templ.Raw`, `templ.Unsafe*`: `TestNoUnescapedOutput` rejects them): Datastar runs any injected `data-*` expression with the page nonce, so escaping is the XSS defence, not the CSP. Redirect targets from a request go through `safeNext`. A GET never changes state or spends a token (mail scanners prefetch emailed links): the crawl fails when a GET calls a state-changing port, a `data-init` posts on load, or a query token is rendered anywhere but its hidden form field. A new UI route must be added to `crawlTargets` in `apps/*/ui/crawl_test.go`, which fails until it is.

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

- `make lint test` before every commit. `make ci` runs exactly what CI runs: build, generated-code diff (sqlc, mocks, templ), gofmt + golangci-lint, the test-location check, `go test -race` against a throwaway Postgres with per-boundary coverage floors, and the probes that prove the layering and test rules still bite (`make lint-layering lint-tests`).
- CI on a PR checks only what the change can affect: `scripts/affected.sh` maps changed files to Go packages and adds every package whose code or tests import them (`go list -test -deps`), then builds, lints, tests and coverage-checks just those, and reruns only the generators whose inputs or outputs changed. Changes to go.mod/go.sum, tools/, scripts/, workflows, `.golangci.yml`, the Makefile, migrations or sqlc config run everything, as do pushes to main and the merge queue. `make affected` shows what your branch would run. A new shared input (a file that can change many packages' results without being in their package graph) belongs in the script's full-run list.
- CI layout: two parallel Go jobs. `lint + test (db)` runs lint while Postgres boots, then the packages whose tests use Postgres (`dbtest` or `TEST_DATABASE_URL` in their tests, see `scripts/ci-shard.sh`); `test (other)` runs the rest. Jobs bill rounded up to whole minutes, so avoid adding short jobs: fold small checks into an existing job. Opening a PR as a draft starts no jobs; pushes to a draft cost one (the draft check). Caches: modules keyed by go.sum; per-shard build/lint caches keyed by commit, saved on every push to main and restored by prefix in PRs. Pushes to main each run on their own (no queueing).
- Run the narrowest thing first: `go test ./apps/web/internal/services/...`.
- Behaviour changes ship with tests at the boundaries below, and only there.
- A test that needs a sleep or timeout to pass is wrong.
- Test memory: `auth.New` hashes passwords with cheap argon2 settings inside test binaries (`testing.Testing()`), so suites that sign users up stay small under `-race`; only `TestPasswordHashDefaults` exercises production cost. A full cold `make test` peaks around 2 GB, almost all of it compiling, so don't lower `-p`. If a test or compile dies with `signal: killed`, check the cgroup's `memory.events`: `oom_kill 0` means something outside the repo sent the kill (another job on a shared machine), not a memory limit.
- User-visible UI changes get one pass in a real browser, over the LAN or Tailscale URL when origins matter.

## Test boundaries (mandatory)

Tests live only at these boundaries, so they pin the architecture instead of working around it. All of it is enforced by `make lint` / CI, not by review: `scripts/check-test-boundaries.sh` rejects a `_test.go` anywhere else, depguard rules in `.golangci.yml` reject the wrong imports in test files, and `scripts/check-coverage.sh` holds a coverage floor per boundary package.

| Boundary | Directory (not subdirectories) | What a test does | It may use | Floor |
|---|---|---|---|---|
| service | `apps/web/internal/services` | unit-tests every method: happy path, 403 for another user, each rule, every port error mapped | generated mocks of the ports (`services/mocks`) | 90 % |
| route handler | `apps/web/internal/api` | per route: no session → 401, other user → 403, invalid input → 400, happy path → 2xx + envelope; validators are covered here | generated mock of the service (`api/mocks`), `apitest.AsUser(u)` / `apitest.Anonymous`, the real router and route wrapper | 85 % |
| ui handler | `apps/web/ui/**` (a tree: also the i18n key guard, render tests and the Datastar hash pin) | same shape as route handlers | generated mocks of the `ui` interfaces + mock session | set when the Account UI lands |
| repository | `packages/database` | integration tests of every repository method | real Postgres via `dbtest.New(t, dbtest.App)` only (each test gets its own cloned database: `t.Parallel()`) | 80 % |
| auth | `packages/auth` | its own suite | real Postgres | 80 % |
| secrets | `packages/secrets` | manager and providers: rotation, outage/staleness, validation, no values in logs | fake providers over `httptest`, `testing/synctest` for the clock (no sleeps), never mocks | 90 % |
| admin service / route handler / ui handler | `apps/admin/internal/services`, `apps/admin/internal/api`, `apps/admin/ui/**` | the same as the web rows above | the same (admin mocks, `apitest.AsAdmin(a)`) | 90 % / 85 % / 80 % |
| admin repository | `packages/database/adminrepo` | integration tests of every repository method | real Postgres: `dbtest.App` (app database) or the admin template (admin database) | 80 % |

The allowlist in `scripts/check-test-boundaries.sh` names the files that have no boundary above them, once per app: `config/config_test.go` (env validation at boot) and `server/{server,auth}_test.go` (wiring: mounts, middleware order, CSRF, and the adapters over real `packages/auth` + Postgres). Do not add to it; if a test does not fit a boundary, test the behaviour through the boundary above it. Service tests may not import pgx, `packages/database` (services re-export `User`, `ErrRecordNotFound`, ...), `packages/auth` or `api`; handler tests may not import pgx, `packages/database`, `packages/auth` or `services/mocks` (mock the service, never what is underneath it); repository tests may not import mocks; production code may not import mocks or `apitest`.

Mocks are generated, never written: `//go:generate go tool mockgen -source=<file>.go -destination=mocks/<file>.go -package=mocks` next to the interfaces, `make generate`, commit the output (CI fails on a stale mock). `mockgen` is a `tool` in the root `go.mod` so it always matches the `go.uber.org/mock` version the mocks import.

## Admin app

`apps/admin` is optional and copies the web app's layers exactly: `packages/database/adminrepo` (sqlc) and adapters over `packages/auth` → `apps/admin/internal/services` → `apps/admin/internal/api` (`/api/admin/v1`, not mounted by `cmd/admin`: the UI calls the services in-process) and `apps/admin/ui`, wired in `apps/admin/internal/server` and `apps/admin/cmd/admin` (`admin serve | migrate | invite <email>`). Config, routes and validators are its own (`apps/admin/internal/{config,validate}`).

- **Two databases.** The admin database (`ADMIN_DATABASE_URL`) holds a second `packages/auth` instance (admins: `goose_auth`, cookie prefix `admin`) and `admin_audit_log` (`goose_admin`). The app database (`DATABASE_URL`) holds the users it administers, reached through `auth.UserAdmin` (packages/auth owns those tables, ban rules and session revocation) and read-only `adminrepo.Stats`.
- **Isolation is enforced.** `apps/web/**` never imports `apps/admin` or `adminrepo`; `apps/admin/**` never imports `apps/web` (copy patterns, not code, so either app can be deleted); admin transports never import `packages/auth` either: they declare UI-owned ports and `server` adapts auth to them. `make lint-layering` proves every rule both ways.
- **Every admin is equal**; `services.Policy` (any admin, 2FA when `ADMIN_REQUIRE_2FA`) is checked first in every service method. Every mutation goes through `services.mutate` and **fails closed**: a pending intent row is appended before the change (if that write fails, the action is refused with `ErrAuditUnavailable`, 503, and nothing changes), then the outcome (`succeeded`, or `failed` with a safe reason) is appended to `admin_audit_outcome`. If the outcome write fails after the change, the caller gets a 500, the row shows "outcome unknown", and `services.AuditReconciler` (startup and every 5 min, `ADMIN_AUDIT_RECONCILE_AFTER`) resolves it from the current state. Both tables are append-only (trigger). Authentication events (sign-in, sign-out, refused 2FA) are different on purpose: packages/auth emits them after the fact (`auth.Config.Events` → `server.NewAuditEvents` → `audit`/`Append`), best-effort, never blocking a sign-in; a failed write is logged and auth counts dropped events. Never route an admin mutation through that path. The CLI acts as `services.SystemActor`.
- New admin feature: same steps as "How to add a service", in `apps/admin`, with the change wrapped in `mutate` in the service, an action constant (`services.Action*`) translated in the UI, and a reconcile verdict in `reconcile.go` when the end state can be read back.

## How to add a service

Copy the user account flow; it is the reference for every service and repository. For a `Widget` owned by a user:

1. **Port.** In `apps/web/internal/services/widgets.go`, declare what the service needs from below: `type WidgetRepository interface { ... }` with model types re-exported (`type Widget = model.Widget`). Anything that is not your database (auth, email, payments) gets its own port and `Err*` values the implementation must return (see `AccountCredentials`). Add the `//go:generate go tool mockgen ...` line.
2. **Repository.** Migration in `packages/database/migrations`, queries in `queries/widgets.sql`, `make generate`, then `packages/database/widgets.go`: a struct whose method set is exactly the port, returning `model.ErrNotFound` for a missing row. A non-database port gets a thin adapter in `apps/web/internal/server` that only converts types and errors.
3. **Service.** `WidgetService` with `NewWidgetService(ports...)`. Every method takes `(ctx, actor Actor, <target id>, in validate.X)`, checks the actor may act on the target (403 otherwise) before touching a port, applies the business rules, and returns named `*services.Error` values (ports report their failures as those values; unknown errors pass through as 500s).
4. **Validators.** One type per input in `apps/web/internal/validate` (`CreateWidget`, `UpdateWidget`) for shape only; rules that need state stay in the service.
5. **Transport.** In `apps/web/internal/api/widgets.go`, declare `type WidgetService interface` (the methods the handlers call, plus `var _ WidgetService = (*services.WidgetService)(nil)`) with its `go:generate` line, routes in `config.APIRoutes`, and handlers of about five lines through `Authenticated`. The UI declares its own interface the same way.
6. **Wire.** `cmd/server/main.go` builds the repository and adapters and passes them to the service, the service to the transports.
7. **Tests**, at the boundaries only: `services/widgets_test.go` (mocks of the ports), `api/widgets_test.go` (mock service + `apitest`, the four cases per route), `packages/database/widgets_test.go` (real Postgres). `make generate lint test` must pass, including the coverage floors.
8. **Every layer.** Walk "Hit every layer" above: the UI, the mobile app, docs.

## Adding a secret

Secrets never reach templates, SSE, JSON or logs:

- **Typed.** A credential in `config.Config` (or anywhere) is a `secrets.Secret`, never a `string` (`DatabaseURL`, `AuthSecret`, `DOPPLER_TOKEN`...): it prints, logs and marshals as `[redacted]`. Validate with `Len`, `IsZero`, `Equal`. Each `config_test.go` proves a loaded `Config` prints nothing secret.
- **Revealed only in `cmd`.** Neither `Secret` nor `Keyring` has a method that returns a value, so no interface can reach one (`TestNoMethodReturnsTheValue` fails if one is added). The only way to read them is `reveal.Value(s)` / `reveal.Keyring(k)` (`packages/secrets/reveal`), called in `apps/*/cmd` where the client is built (`database.Open`, `auth.Config`, `NewResendMailer`); depguard rejects importing `reveal` anywhere else, and it reads the value through `packages/secrets/internal/access`, which Go's internal rule keeps private. Inside `packages/secrets` code uses the field directly. UIs and JSON APIs never import `packages/secrets`, and a UI never imports its app's JSON API (depguard; `make lint-layering` probes each rule and proves the bypasses do not compile).
- **Redacting logger.** Both binaries log through `secrets.NewRedactingHandler`: values of credential-named keys (password, secret, token, api_key, authorization, cookie, dsn, database_url, otp, backup codes...) and, in any string, `Bearer` tokens, URL passwords, token/code/key query values, emailed-link tokens and Cookie headers become `[redacted]`. Outside production it keeps the dev mailer's `url` readable. It is a net, not a licence: log `Fingerprint()`.

1. **It rotates live.** Every secret does except `DOPPLER_TOKEN`; pick the mechanism by consumer: an outbound API key rebuilds its client (`auth.SwapMailer`), a database URL goes through a `database.Pool` (`Swap`: probe, atomic switch, drain), a signing/encryption key goes through a keyring with a grace period (`secrets.Keyring` + `auth.Auth.SetSecrets`; data at rest under a wrapped data key, never directly under the secret). Non-secret settings stay plain env vars in `config.Load`.
2. **Live secret.** Add the name to `liveSecrets` in the app's `internal/config/secrets.go` (a keyring adds `secrets.KeyringNames(name)`), its checks to `validateLive` (they run on the env at boot and on every fetch before it is published; use `Len`/`IsZero`/`Equal`, never `Reveal`), and a typed accessor next to `OnDatabaseURL` / `OnAuthSecrets`.
3. **Consumer.** In `cmd/server`, register the hook: build the new client and swap it in atomically (see `auth.SwapMailer`); on a zero value fall back or keep the old client; return an error to keep the previous client (the manager retries it after each later fetch). A hook must not call back into the manager (`OnChange`, `Refresh`, `Stop`). Log `v.Fingerprint()`, never the value. Never cache the value elsewhere.
4. **Tests and docs.** Cover the swap at the consumer's boundary (like `TestSwapMailerUsesTheLatestMailer`), add the name to `.env.example` and README "Secrets", and add it to every Doppler config.

## Pull requests

- Never open a PR unless the developer asks.
- Conventional commit titles in plain language: `fix(api): PATCH /users no longer drops the image`.
- Body: the problem in a sentence or two, then the fix. UI changes need before/after images; motion needs a short video.
- One concern per PR. If the description says "also", split it.
- `ci-ok` is the single required check. When babysitting CI, verify each finding against the source, fix real ones, and dismiss false positives with a reason.

## Where code lives

- `apps/web/cmd/server`: the binary (`server serve`, `server migrate`).
- `apps/web/internal/config`: env config, `AppRoutes`/`APIRoutes`, `Features`.
- `apps/web/internal/server`: middleware (`apiOnly` hides the API and `/healthz` from browsers), `/healthz`, the mounts (auth at `/api/auth/`, API, UI at `/`), and the adapters from `packages/auth` to service ports (`CallerFrom`, `AccountCredentials`).
- `apps/web/internal/api`: JSON transport `/api/core/v1`: envelope, route wrapper, handlers, the service ports they call; `api/mocks` (generated) and `api/apitest` (mock session) for handler tests.
- `apps/web/internal/services`: business rules and the ports they need; `services/mocks` (generated) for service tests. `apps/web/internal/validate`: input types.
- `apps/web/ui`: templ pages, Datastar handlers, i18n, static assets, and the browser security headers (`security.go`).
- `packages/uikit`: the templ components and design tokens both UIs share (`theme.css` is the one file to re-theme). It renders translated strings and values only; depguard keeps it from importing `apps/` or the data, auth and secrets packages.
- `packages/database`: pool (`Pool`: swaps itself when `DATABASE_URL` rotates), `migrations/` (goose, embedded), `queries/` (sqlc input), `sqlc/` (generated, never edit), repositories, `model/`, `dbtest/`.
- `packages/secrets`: live secrets: `Manager` (polling, validation, last-good cache, staleness, change hooks) and providers (`Env`, `Doppler`; Infisical/1Password stubs behind build tags), `Secret`, and the redacting log handler. Self-contained; never reads the env.
- `packages/auth`: Better Auth–style auth: its own migrations, queries, endpoints (Better Auth paths and response shapes, not the envelope), sessions, mailers. Self-contained: it never imports `apps/` or `packages/database` (depguard).
- `apps/mobile-app`: the Expo app, which talks to the JSON API with bearer tokens.
- `apps/admin`: the optional admin dashboard (see "Admin app"); `packages/database/adminrepo`: its data layer (audit log, stats; migrations, queries and sqlc output under it).
- Root: `Makefile`, `docker-compose.yml`, `Dockerfile`, `railway.json`, `.golangci.yml`, `tools/` (pinned dev tools, run via `go tool -modfile=tools/go.mod`).

## Taste

- Complexity belongs at the layer boundary. Repositories hide SQL, services hide business rules, the route wrapper hides auth and envelopes, and templates stay dumb.
- Small interfaces, defined by the consumer (`services.UserRepository`, `api.UserService`). Concrete types out, interfaces in.
- Typed values over `any` and `map[string]any`. Generics only where they remove real repetition.
- Errors are values: wrap with context, compare with `errors.Is`/`errors.As`, and never log and return the same error.
- Comments say how a thing is used and move with the code. Document functions, not every line.
- If a rule here fights the task in front of you, say so loudly and get a human sign-off before breaking it.
