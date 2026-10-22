# AGENTS.md: instructions for AI assistants

This is **Go Fast Stack**, a Go + Datastar monorepo template: one Go module, one static binary, server-rendered `templ` pages with Datastar, a JSON API for the mobile app, Postgres through `pgx` + `sqlc` + `goose`, and Go-native auth modelled on Better Auth. It replaces the Next.js template `jayleaton/ai-patterns-monorepo` and keeps the same patterns.

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
- **Templates are CSP-clean.** `ui.Handler` sends a nonce CSP with no `unsafe-*`: no inline `style=`/`on*=` attributes or inline scripts (Datastar `data-*` expressions are fine; hide with `class="hidden"` + `data-class:hidden`). Never bypass templ's escaping (`templ.Raw`, `templ.Unsafe*`): Datastar runs any injected `data-*` expression with the page nonce, so escaping is the XSS defence, not the CSP. Redirect targets from a request go through `safeNext`. A GET never changes state or spends a token (mail scanners prefetch emailed links): a GET handler calls no state-changing port, no `data-init` posts on load, and a query token is rendered only in its hidden form field.

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

- **`make check-fast` before every push (mandatory).** It runs gofmt, `go vet`, golangci-lint, the test-location check and plain `go test` (no `-race`) on only the packages your changes can affect, plus the generators whose inputs changed and the `deploy/` module when it changed. Changes mean everything that differs from the merge base with origin/main: commits, staged, unstaged and untracked files, mapped to packages by `scripts/affected.sh` exactly as CI does, so a shared input (go.mod, tooling, lint config, migrations) checks everything. It takes seconds (a docs-only change: under a second; a full run: under 10 s warm), so it fits machines where a full `make ci` is not allowed. Database tests skip unless `TEST_DATABASE_URL` is set; CI still runs `-race`, Postgres and the probes. `make hooks` installs a pre-push hook that runs it from every worktree of the clone (`make hooks-remove` undoes it; `git push --no-verify` skips it once, for emergencies only). Anything that runs git or go from a hook drops git's repository env first (`git rev-parse --local-env-vars`), as the hook and `scripts/check-fast.sh` do.
- `make lint test` before every commit when you can run the full suite. `make ci` runs exactly what CI runs: build, generated-code diff (sqlc, mocks, templ), gofmt + golangci-lint, the test-location check, `go test -race` against a throwaway Postgres with per-boundary coverage floors, and the probes that prove the layering and test rules still bite (`make lint-layering lint-tests`).
- CI on a PR checks only what the change can affect: `scripts/affected.sh` maps changed files to Go packages and adds every package whose code or tests import them (`go list -test -deps`), then builds, lints, tests and coverage-checks just those, and reruns only the generators whose inputs or outputs changed. Changes to go.mod/go.sum, tools/, scripts/, workflows, `.golangci.yml`, the Makefile, migrations or sqlc config run everything, as do pushes to main and the merge queue. `make affected` shows what your branch would run. A new shared input (a file that can change many packages' results without being in their package graph) belongs in the script's full-run list.
- CI layout: jobs bill rounded up to whole minutes, so CI is ONE job (`ci`): affected-only generators, gofmt, golangci-lint and race tests (all packages on pushes to main, the merge queue and manual runs), plus the mobile app and `deploy/` module checks when those change. Docs-only PRs post `ci-ok` from that job with nothing to check; drafts start no jobs. Sharding is an opt-in fallback only: label a PR `ci:fanout` (takes effect on its next push) or run the workflow manually with `fanout`, which splits into `lint + test (db)` and `test (other)` for faster wall time at more billed minutes. Timeouts are ~2x measured averages (see the workflow comments). Shared steps live in `.github/actions/check`. Avoid adding jobs: fold new checks into the existing one. A push to main whose tree is identical to a PR merge commit that already passed CI is not tested again: `ci-ok` is posted from that result (`scripts/ci-reuse.sh`; PR results record the tested tree in their `ci-ok` description). Caches: modules keyed by go.sum; one build/lint cache keyed by commit, saved by main pushes that run and by a scheduled run every 4 hours (skipped when main has not moved), restored by prefix elsewhere.
- Run the narrowest thing first: `go test ./apps/web/internal/services/...`.
- Behaviour changes ship with tests at the boundaries below, and only there.
- A test that needs a sleep or timeout to pass is wrong. See "Fast tests".
- Test memory: `auth.New` hashes passwords with cheap argon2 settings inside test binaries (`testing.Testing()`), so suites that sign users up stay small under `-race`; only `TestPasswordHashDefaults` exercises production cost. A full cold `make test` peaks around 2 GB, almost all of it compiling, so don't lower `-p`. If a test or compile dies with `signal: killed`, check the cgroup's `memory.events`: `oom_kill 0` means something outside the repo sent the kill (another job on a shared machine), not a memory limit.
- User-visible UI changes get one pass in a real browser, over the LAN or Tailscale URL when origins matter.

## Test boundaries (mandatory)

The business logic is the testing layer. Tests live only in these five directories; everything else (UI, config, server wiring, repositories, `packages/database`, `packages/secrets`, `deploy/`) has no tests and must not get any. The database is assumed working: migrations are checked against real state before they ship, and services see repositories only through mocked ports. All of it is enforced by `make lint` / CI, not by review: `scripts/check-test-boundaries.sh` rejects a `_test.go` anywhere else (there is no allowlist; do not add one), depguard rules in `.golangci.yml` reject the wrong imports in test files, and `scripts/check-coverage.sh` holds a coverage floor per boundary.

| Boundary | Directory (not subdirectories) | What a test does | It may use | Floor |
|---|---|---|---|---|
| service | `apps/web/internal/services`, `apps/admin/internal/services` | unit-tests every method: happy path, 403 for another user, each rule, every port error mapped | generated mocks of the ports (`services/mocks`) | 90 % |
| route handler | `apps/web/internal/api`, `apps/admin/internal/api` | per route: no session → 401, other user → 403, invalid input → 400, happy path → 2xx + envelope; validators are covered here | generated mock of the service (`api/mocks`), `apitest.AsUser(u)` / `apitest.AsAdmin(a)` / `apitest.Anonymous`, the real router and route wrapper | 85 % |
| backend auth | `packages/auth` | its own suite: sessions, passwords, rate limits, rotation | real Postgres | 80 % |

If behaviour outside these directories matters, move the logic into a service and test it there. Service tests may not import pgx, `packages/database` (services re-export `User`, `ErrRecordNotFound`, ...), `packages/auth` or `api`; handler tests may not import pgx, `packages/database`, `packages/auth` or `services/mocks` (mock the service, never what is underneath it); production code may not import mocks or `apitest`.

Mocks are generated, never written: `//go:generate go tool mockgen -source=<file>.go -destination=mocks/<file>.go -package=mocks` next to the interfaces, `make generate`, commit the output (CI fails on a stale mock). `mockgen` is a `tool` in the root `go.mod` so it always matches the `go.uber.org/mock` version the mocks import.

## Fast tests

The suite runs on every change it can affect, under `-race` on 2-vCPU CI runners, so its speed is a feature that has to survive growth. Lint and `make test` enforce these; `make lint-tests` proves the budgets trip.

- **Parallel.** Every test starts with `t.Parallel()` (`paralleltest`). Slow table tests make their cases parallel subtests too; `tparallel` keeps a test and its subtests consistent. Build expensive fixtures once per group and share them with the parallel subtests: CI has 2 vCPUs, so a split that doubles CPU time gains nothing, and a long subtest trips the budget. Judge CPU cost with `GOMAXPROCS=2`; on a big machine idle parallel workers inflate it. Never share a gomock controller, `httptest.Server` or recorder across parallel subtests; build them per case. A test that must touch process-wide state stays serial with `//nolint:paralleltest // <why>`; prefer removing the global instead.
- **No sleeps.** `time.Sleep` in a test fails lint (forbidigo). Wait on a channel, a condition or `synctest.Wait`; for intervals, expiry and backoff use `testing/synctest` or an injected clock (`auth`'s `testClock`).
- **One database per test, cloned.** `packages/auth` (the only suite on Postgres) clones a database migrated once per server in its `TestMain` (`newDatabase`), so tests never migrate; tests on its shared database use unique data (`uniqueEmail`).
- **Cheap crypto.** `auth.New` uses cheap argon2 inside test binaries; only `TestPasswordHashDefaults` pays production cost.
- **Table tests.** One test with a case table, not a copy per case.
- **Budgets.** `make test` and CI pipe `go test -json` through `scripts/testbudget`: plain `go test` output, then the slowest tests, then a failure if a package's test binary runs over 60 s or a single test (a leaf test or subtest) over 10 s, measured on CI under `-race` (about twice today's slowest, so a doubling fails). Over budget means split or speed up the test. Raise a budget (constants in `scripts/testbudget`) only with the reason in the commit.
- **Measure, don't guess.** `make bench-devloop` prints the timing tables (build, generate, lint per linter, probes, tests per package with and without `-race`); `BENCH_COLD=1` adds cold caches, `BENCH_PKGS` narrows the packages, and `BENCH_BASE=<ref>` adds an A/B of the CI test run against that ref on the same machine (medians of alternating rounds). Separate CI runs land on different machines and vary by tens of percent, so compare a perf change with the A/B: push it to a `bench/<name>` branch (the "Bench dev loop" workflow compares it with main) and delete the branch afterwards.

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

- **Typed.** A credential in `config.Config` (or anywhere) is a `secrets.Secret`, never a `string` (`DatabaseURL`, `AuthSecret`, `DOPPLER_TOKEN`...): it prints, logs and marshals as `[redacted]`. Validate with `Len`, `IsZero`, `Equal`.
- **Revealed only in `cmd`.** Neither `Secret` nor `Keyring` has a method that returns a value, so no interface can reach one (`TestNoMethodReturnsTheValue` fails if one is added). The only way to read them is `reveal.Value(s)` / `reveal.Keyring(k)` (`packages/secrets/reveal`), called in `apps/*/cmd` where the client is built (`database.Open`, `auth.Config`, `NewResendMailer`); depguard rejects importing `reveal` anywhere else, and it reads the value through `packages/secrets/internal/access`, which Go's internal rule keeps private. Inside `packages/secrets` code uses the field directly. UIs and JSON APIs never import `packages/secrets`, and a UI never imports its app's JSON API (depguard; `make lint-layering` probes each rule and proves the bypasses do not compile).
- **Redacting logger.** Both binaries log through `secrets.NewRedactingHandler`: values of credential-named keys (password, secret, token, api_key, authorization, cookie, dsn, database_url, otp, backup codes...) and, in any string, `Bearer` tokens, URL passwords, token/code/key query values, emailed-link tokens and Cookie headers become `[redacted]`. Outside production it keeps the dev mailer's `url` readable. It is a net, not a licence: log `Fingerprint()`.

1. **It rotates live.** Every secret does except `DOPPLER_TOKEN`; pick the mechanism by consumer: an outbound API key rebuilds its client (`auth.SwapMailer`), a database URL goes through a `database.Pool` (`Swap`: probe, atomic switch, drain), a signing/encryption key goes through a keyring with a grace period (`secrets.Keyring` + `auth.Auth.SetSecrets`; data at rest under a wrapped data key, never directly under the secret). Non-secret settings stay plain env vars in `config.Load`.
2. **Live secret.** Add the name to `liveSecrets` in the app's `internal/config/secrets.go` (a keyring adds `secrets.KeyringNames(name)`), its checks to `validateLive` (they run on the env at boot and on every fetch before it is published; use `Len`/`IsZero`/`Equal`, never `Reveal`), and a typed accessor next to `OnDatabaseURL` / `OnAuthSecrets`.
3. **Consumer.** In `cmd/server`, register the hook: build the new client and swap it in atomically (see `auth.SwapMailer`); on a zero value fall back or keep the old client; return an error to keep the previous client (the manager retries it after each later fetch). A hook must not call back into the manager (`OnChange`, `Refresh`, `Stop`). Log `v.Fingerprint()`, never the value. Never cache the value elsewhere.
4. **Tests and docs.** Cover the swap at the consumer's boundary (like `TestSwapMailerUsesTheLatestMailer`), add the name to `.env.example` and README "Secrets", and add it to every Doppler config.

## Deploy to Railway (for agents)

Only when the developer asks: it creates billed resources in their Railway account.

1. **Preflight.** `bin/railway-setup` is built by any `make railway-*` target. Run `make railway-doctor ARGS=--json` and fix every check with `"status": "fail"`; each check carries a `fix`. Typical fixes: `railway login` (the developer's browser), `--workspace` when they belong to several, and `railway ssh keys add` when there is more than one environment.
2. **Setup.** `make railway-setup ARGS="--yes --json --environments staging,production"`. Add `--admin true`, `--region`, `--project` or `--workspace` as asked. Every flag has a `PLATFORM_*` env equivalent (`PLATFORM_YES=1`, `PLATFORM_JSON=1`, `PLATFORM_ENVIRONMENTS`, ...). The flags are written into `deploy/platform.yaml`: commit that file. Without `--yes`, and with no terminal to ask on, creating a project stops with exit 3.
3. **Read the summary.** stdout is one JSON object: `ok`, `project.{id,url}`, `source` (`github` or `upload`), each environment's services with `url`, `deployment` and `health`, and `nextSteps`. Logs go to stderr. Relay `nextSteps` to the developer verbatim. They are things only a person can do, such as installing the GitHub app, Doppler tokens or a real `RESEND_API_KEY`. Never invent or fill in secrets yourself.
4. **Doppler** (`secrets.provider: doppler`): when the developer has run `doppler login`, setup also moves the app secrets into Doppler (README "Secrets and Doppler"); otherwise `nextSteps` says so, and `make doppler-setup ARGS="--json"` does it later. Its output names configs, secret names and fingerprints only. Keep it that way: never print, log or paste a token or secret value.
5. **Re-run anytime.** `make railway-setup` is idempotent: no changes, no redeploys when healthy. Removing an environment or `admin` never deletes anything without `CONFIRM_DESTRUCTIVE=1`; ask before passing it.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | error: read `error` |
| 2 | plan: changes pending |
| 3 | needs `--yes` |
| 4 | doctor: a required check failed |
| 5 | applied, but a public service is unhealthy |

`make` itself turns any failure into exit 2, so read `exitCode` from the JSON, or run `bin/railway-setup <command> -config deploy/platform.yaml -iac deploy/.railway/railway.go` directly.

## Backups

- The backup job (`apps/backup` + `packages/backup`) is deletable like the admin app: it imports no app, no app imports it, and it reaches Postgres only through `pg_dump`/`pg_restore` (depguard, probed). It runs in its own `postgres:18-alpine` image so the tools match the server.
- A new database (a third service with its own Postgres) gets backed up by passing its URL to the backup service and adding it to the job's database list; keep `<db>/<time>.dump.age` ids and the restore guards (`--yes`, `--force` over a non-empty database, an explicit target).
- Never weaken the restore guards or log a value: logs carry ids, sizes and fingerprints. The encryption key is a keyring (`BACKUP_ENCRYPTION_KEY`, `_PREVIOUS`), validated with `Len` and revealed only in `apps/backup/cmd`.
- Retention is the job's (Railway buckets have no lifecycle rules). Before changing `Retention.Expired`, prove the change with a local prune run: plant old backup ids in a local store and check what `backup prune` keeps.

## Pull requests

- Never open a PR unless the developer asks.
- Conventional commit titles in plain language: `fix(api): PATCH /users no longer drops the image`.
- Body: the problem in a sentence or two, then the fix. UI changes need before/after images; motion needs a short video.
- One concern per PR. If the description says "also", split it.
- `ci-ok` is the single required check. It is a commit status posted by the CI jobs on the commit they tested (pending, then success/failure), not a job, so skipped or cancelled jobs and drafts can never satisfy it. Require it in branch protection as a status check (source: GitHub Actions). When babysitting CI, verify each finding against the source, fix real ones, and dismiss false positives with a reason.

## Where code lives

- `apps/web/cmd/server`: the binary (`server serve`, `server migrate`).
- `apps/web/internal/config`: env config, `AppRoutes`/`APIRoutes`, `Features`.
- `apps/web/internal/server`: middleware (`apiOnly` hides the API and `/healthz` from browsers), `/healthz`, the mounts (auth at `/api/auth/`, API, UI at `/`), and the adapters from `packages/auth` to service ports (`CallerFrom`, `AccountCredentials`).
- `apps/web/internal/api`: JSON transport `/api/core/v1`: envelope, route wrapper, handlers, the service ports they call; `api/mocks` (generated) and `api/apitest` (mock session) for handler tests.
- `apps/web/internal/services`: business rules and the ports they need; `services/mocks` (generated) for service tests. `apps/web/internal/validate`: input types.
- `apps/web/ui`: templ pages, Datastar handlers, i18n, static assets, and the browser security headers (`security.go`).
- `packages/uikit`: the templ components and design tokens both UIs share (`theme.css` is the one file to re-theme). It renders translated strings and values only; depguard keeps it from importing `apps/` or the data, auth and secrets packages.
- `apps/backup`, `packages/backup`: the nightly encrypted Postgres backup and restore (README "Backups and restore").
- `packages/database`: pool (`Pool`: swaps itself when `DATABASE_URL` rotates), `migrations/` (goose, embedded), `queries/` (sqlc input), `sqlc/` (generated, never edit), repositories, `model/`.
- `packages/secrets`: live secrets: `Manager` (polling, validation, last-good cache, staleness, change hooks) and providers (`Env`, `Doppler`; Infisical/1Password stubs behind build tags), `Secret`, and the redacting log handler. Self-contained; never reads the env.
- `packages/auth`: Better Auth–style auth: its own migrations, queries, endpoints (Better Auth paths and response shapes, not the envelope), sessions, mailers. Self-contained: it never imports `apps/` or `packages/database` (depguard).
- `apps/mobile-app`: the Expo app, which talks to the JSON API with bearer tokens.
- `apps/admin`: the optional admin dashboard (see "Admin app"); `packages/database/adminrepo`: its data layer (audit log, stats; migrations, queries and sqlc output under it).
- `deploy/`: the Railway setup, its own Go module (the Railway IaC SDK never enters the app's `go.mod`). `platform.yaml` is what a team edits; `platform/` turns it into the IaC graph (pure, tested); `.railway/railway.go` is the shim the Railway CLI evaluates; `cmd/railway-setup` does what IaC cannot (project, environments, domains, sealed secrets, previews). Run it through `make railway-plan` / `make railway-apply`, never by hand-editing Railway. Railway bugs we work around are in `docs/railway-go-iac-issues.md`.
- Root: `Makefile`, `docker-compose.yml`, `Dockerfile`, `.golangci.yml`, `tools/` (pinned dev tools, run via `go tool -modfile=tools/go.mod`).

## Taste

- Complexity belongs at the layer boundary. Repositories hide SQL, services hide business rules, the route wrapper hides auth and envelopes, and templates stay dumb.
- Small interfaces, defined by the consumer (`services.UserRepository`, `api.UserService`). Concrete types out, interfaces in.
- Typed values over `any` and `map[string]any`. Generics only where they remove real repetition.
- Errors are values: wrap with context, compare with `errors.Is`/`errors.As`, and never log and return the same error.
- Comments say how a thing is used and move with the code. Document functions, not every line.
- If a rule here fights the task in front of you, say so loudly and get a human sign-off before breaking it.
