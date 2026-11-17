# AGENTS.md: instructions for AI assistants

This is **Go Fast Stack**, a Go + Datastar monorepo template: one Go module, static binaries, server-rendered `templ` pages, a JSON API for the mobile app, Postgres through `pgx` + `sqlc` + `goose`, and Go-native auth modelled on Better Auth.

Shared vocabulary and where code lives are in [CONTEXT.md](CONTEXT.md). Use those words and propose additions for naming and formatting.

## Ground rules

1. **Template first.** People clone this and diverge right away. Keep it generic and minimal; the example flows (the users API, the dashboard) are documentation as code. If a change only makes sense for one project, it does not belong here.
2. **Code gives context.** Do not write comments for everything. Code should be named and shaped well enough for agents to discover what it needs; if it needs a comment to be understood, refactor it. Keep comments and documentation to an absolute minimum.
3. **Performance without compromise.** Render on the server, keep client JavaScript to what Datastar needs, and treat binary size, memory and verification time as features. Prefer the standard library; every dependency needs a reason.
4. **Network-ready dev servers.** The server binds `0.0.0.0:3000`, so it works the same on localhost, the LAN and Tailscale. Never bake `localhost` into code, emails or auth callbacks: origins come from `PUBLIC_URL`, `INTERNAL_URL` and `TRUSTED_ORIGINS`.
5. **Monorepo-aware.** `packages/database`, `packages/auth`, `apps/web`, `apps/admin` and `apps/mobile-app` move together. Every change gets a decision per workspace, even if it is "no change needed".

## Architecture (mandatory)

- The template's value is its layers. Do not bypass them. `packages/database` (migrations → sqlc queries → repository) → `apps/web/internal/services` → `apps/web/internal/api` (the JSON API: access gate, validation, envelope) → `apps/web/ui` (HTML/Datastar), which reaches data only through the API's client.
- Each layer talks only to the layer below. **The UI calls the API, never the services**: pages and actions use `apiclient.Client` (`apps/web/internal/apiclient`, the only app package `ui/**` may import for data), whose implementation `api.Client` runs each `api.Endpoint` in-process.
- **Browser interactions go through UI server actions, never to the API.** A form post or Datastar action targets a UI action route (`action(policy, ...)`: CSRF-protected, in `config.AppRoutes` / `ui/routes`), which calls the API client and answers with HTML.
- The browser never calls `/api/...` directly.
- **Sign-in flows are API endpoints too.** Sign-up, sign-in, sign-out, verification, password reset and 2FA are `api.Endpoint`s (`api.Sessions`) over an `api.Authenticator` port that `server.Authenticator` adapts from `packages/auth`.
- The UI never imports `services`, `validate`, `access`, `api`, `packages/auth`, repositories, `packages/database` or sqlc output, and `server` never imports its app's `ui` (no adapters for UI-owned ports); transports never import `pgx`, sqlc output or repositories.

## Hit every layer

The most common defect is a change that works on the path you tested and is missing everywhere else. Before calling work done, walk this list and say which entries applied:

- **Entry points.** A new page needs its handler, its route in `config.AppRoutes`, a Datastar loading indicator and translations in every locale; a new endpoint needs its route in `config.APIRoutes`.
- **Contracts.** Migrations and sqlc queries are the contract: `make generate`, then follow every consumer (repository, service, validator, both transports, the mobile app).
- **Reverse states.** Delete needs restore, disable needs enable, set needs clear.
- **Placeholders.** Replacing a template example (the welcome pages behind `config.Features.TemplateWelcome`, the dashboard's example cards, the admin Analytics page, the Infisical and 1Password stubs) deletes it in the same PR. A stub you add answers `501 Not Implemented`.

## Verifying

- Verify code changes both ways and walk the tree for failures and regressions. How to verify and attest a PR: the [github-prs](.claude/skills/github-prs/SKILL.md) skill.
- Merged migrations are append-only; a new one takes the next number above the highest on origin/main (`make check-migrations`).

## Test boundaries (mandatory)

The business logic is the testing layer. Tests live only in these directories; everything else (UI, config, server wiring, repositories, `packages/database`, `packages/secrets`, `deploy/`) gets none. Services see repositories only through mocked ports. All of it is enforced by `make lint`, not by review: `scripts/check-test-boundaries.sh` rejects a `_test.go` anywhere else and `scripts/check-coverage.sh` holds a floor per boundary. Never remove these restrictions and never write tests that do not test business logic.

| Boundary | Directory (not subdirectories) | What a test does |
|---|---|---|
| service | `apps/web/internal/services`, `apps/admin/internal/services` | unit-tests the business rules, with generated mocks of the ports |
| route handler | `apps/web/internal/api`, `apps/admin/internal/api` | per route: no session → 401, other user → 403, invalid input → 400, happy path → 2xx + envelope |
| backend auth | `packages/auth` | its own suite on real Postgres: sessions, passwords, rate limits, rotation |
| local verification | `scripts/cilocal/verify` | the rules `make ci-local` and its GitHub checks enforce; standard library only |

If behaviour outside these directories matters, move the logic into a service and test it there.

Mocks are generated, never written: `//go:generate go tool mockgen -source=<file>.go -destination=mocks/<file>.go -package=mocks` next to the interfaces, `make generate`, commit the output (`make check-generated` fails on a stale mock). `mockgen` is a `tool` in the root `go.mod` so it always matches the `go.uber.org/mock` version the mocks import.

## Fast tests

The suite runs on every change it can affect, under `-race` on machines as small as a 2-vCPU box, so its speed is a feature that has to survive growth. Lint and `make test` enforce these; `make lint-tests` proves the budgets trip.

## Admin app

`apps/admin` is optional and copies the web app's layers exactly: `packages/database/adminrepo` (sqlc) and adapters over `packages/auth` → `apps/admin/internal/services` → `apps/admin/internal/api` (`/api/admin/v1`, not mounted on the network by `cmd/admin`) → `apps/admin/ui`, which calls that API in-process through its own `apps/admin/internal/apiclient` (same rules as the web app), wired in `apps/admin/internal/server` and `apps/admin/cmd/admin` (`admin serve | migrate | invite <email>`). Config, routes and validators are its own (`apps/admin/internal/{config,validate}`).

- **Two databases.** The admin database (`ADMIN_DATABASE_URL`) holds a second `packages/auth` instance (admins: `goose_auth`, cookie prefix `admin`) and `admin_audit_log` (`goose_admin`). The app database (`DATABASE_URL`) holds the users it administers, reached through `auth.UserAdmin` (packages/auth owns those tables, ban rules and session revocation) and read-only `adminrepo.Stats`.
- **Isolation is enforced.** `apps/web/**` never imports `apps/admin` or `adminrepo`; `apps/admin/**` never imports `apps/web` (copy patterns, not code, so either app can be deleted); admin transports never import `packages/auth` either: the admin API declares its ports (`api.Authenticator`) and `server` adapts auth to them. `make lint-layering` proves every rule both ways.
- **Every mutation fails closed** through `services.mutate`: intent row, change, outcome row. Never route an admin mutation through the best-effort auth events.

## Deploying

The platform runs on Railway: environments are declared in [deploy/platform.yaml](deploy/platform.yaml); the release rules are in [docs/RELEASES.md](docs/RELEASES.md).

- **Do not create, change or delete Railway resources unless the developer explicitly asks**: it creates billed resources in their account. Infrastructure comes from [deploy/platform.yaml](deploy/platform.yaml) through `make railway-setup` / `make railway-plan` / `make railway-apply` (`ARGS=--json`; `make railway-doctor` prints each fix), never from the dashboard. Never hand-edit Railway or `deploy/.railway/railway.go`.
- **Relay `nextSteps` verbatim.** They are what only a person can do. Never invent a sender address, a token or a secret, and never print, log or paste a secret value.
- **Hand-set variables.** Never set a Railway variable by hand without listing it in `unmanaged_variables` or as `manual` in `per_environment`: the next apply deletes it (`make deploy-vars` must pass).

## Environments: staging is always current

- **Staging always runs the latest `main`.** Every merge of a head `make ci-local` attested deploys to staging automatically (`Deploy staging`, `.github/workflows/deploy-staging.yml`), and the hourly "Staging catch-up" deploys `main` whenever staging is behind ([docs/RELEASES.md](docs/RELEASES.md)).
- **Production may lag staging, never lead it.** Production only ever runs a commit that staging has already run and passed its readiness check on. No hotfixes straight to production, no production deploys of branches, and never a production commit that staging has not run.
- **A failed staging deploy, or an unattested `main` that staging refuses, is the top priority.** After a merge, check its deploy once with `make pr-merge ARGS="--check-deploy <squash sha>"` (one read, not a polling loop); "Post-merge report" also comments on the merged PR when Deploy staging fails on `main`. If it failed, fix it, or attest and dispatch `main` ([github-prs](.claude/skills/github-prs/SKILL.md)), before merging anything else. Never leave staging behind `main`.
- **Production ships only as a GitHub release** ([docs/RELEASES.md](docs/RELEASES.md)): publishing a `vX.Y.Z` release starts "Deploy production", whose gate proves staging ran that commit, still runs it or a later `main` commit and is healthy, and that the publisher is in `PRODUCTION_RELEASERS`. An agent publishes or dispatches one only when the developer explicitly says so.

## Branches and releases

`main` is the only long-lived branch, named once in `scripts/branches.sh`. Agents push feature branches and open PRs that target `main`; CI runs locally and every PR is attested before it merges.

## Pull requests

- Conventional commit titles in plain language: `fix(api): PATCH /users no longer drops the image`.
- Body: the problem in a sentence or two, then the fix. UI changes need before/after images; motion needs a short video.
- Follow the repo PR skill: the [github-prs](.claude/skills/github-prs/SKILL.md) skill.
- Open it from [.github/pull_request_template.md](.github/pull_request_template.md), ready (not draft), then run what it needs and `make ci-local RAN="<what you ran>"` on its pushed head; it fills "Local verification". A new push or rebase needs a new attestation.
- Merge with `make pr-merge PR="<number>..."` only. When babysitting, verify each finding against the source, fix real ones, and dismiss false positives with a reason.

## Taste

- Complexity belongs at the layer boundary. Repositories hide SQL, services hide business rules, the API's endpoints hide access, validation and envelopes for both ways in, and templates stay dumb.
- Small interfaces, defined by the consumer (`services.UserRepository`, `api.UserService`). Concrete types out, interfaces in.
- Typed values over `any` and `map[string]any`. Generics only where they remove real repetition.
- Errors are values: wrap with context, compare with `errors.Is`/`errors.As`, and never log and return the same error.
- Comments are used sparingly: the code should tell the story.
- No magic numbers: a literal that carries meaning is a named const (`const maxRequestBody = 1 << 20 // 1 MiB cap on JSON bodies`); 0 and 1 as plain counters and indexes are fine. `mnd` in golangci-lint enforces it outside tests.
- No single-character names, anywhere, tests included. Name a thing for what it holds (`func (service *Service)`, `writer http.ResponseWriter`, `request *http.Request`, `test *testing.T`); two letters are fine where they are the word (`ctx`, `id`, `ok`, `tx`). `scripts/namelint` enforces it in `make lint` and `make check-fast`.
- If a rule here fights the task in front of you, say so loudly and get a human sign-off before breaking it.
