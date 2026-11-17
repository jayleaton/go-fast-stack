# CONTEXT.md: shared language

Use these words in code, commits, review and agent output. The **Code** column
gives the canonical identifier or place to use; where a word and the code
disagree, the code is what ships. [AGENTS.md](AGENTS.md) holds the rules; this
file holds the names.

## Who is who

| Term | Means | Code |
|---|---|---|
| you, agent | an AI assistant changing the repo, on a branch, never on `main` | AGENTS.md |
| we, maintainers | the people building this template | |
| developer | the person who cloned the template and directs agents | |
| user | a person signed in to the app | `"user"`, `model.User` |
| admin | a person who can sign in to the admin app; never a row in the app's `"user"` | admin `"user"`, `apps/admin/internal/access` |

## Engineering vocabulary

| Term | Means | Code |
|---|---|---|
| app | a product surface: **web** (`apps/web`), **admin** (`apps/admin`), **backup** (`apps/backup`) or **mobile** (`apps/mobile-app`) | `apps/` |
| package | shared Go code under `packages/` | `packages/` |
| migration | a goose SQL file; three sets: `goose_auth`, `goose_app` (web), and `goose_auth` + `goose_admin` in the admin database | `packages/*/migrations/` |
| query | a sqlc-annotated statement; `make generate` turns it into Go | `packages/database/queries/` |
| repository | data access over sqlc implementing a service's port; returns `model` types and `model.ErrNotFound` | `packages/database/*.go` |
| model | plain data types repositories return; services re-export them | `packages/database/model/` |
| service | business rules; returns `*services.Error` with a `Kind` | `apps/web/internal/services/` |
| validator | an input type with `Validate()`, named by CRUD verb; shape only | `apps/web/internal/validate/` |
| transport | code that speaks HTTP: the **JSON API** or the **UI** (UI → API → services) | `apps/web/internal/api/`, `apps/web/ui/` |
| endpoint | `api.Endpoint`: one API operation declared once (method, path, policy, handler), served over HTTP and called in-process | `apps/web/internal/api/` |
| API client | `apiclient.Client`: the API as the UI calls it; `api.Client` implements it in-process | `apps/web/internal/apiclient/` |
| server action | a UI action route a form or Datastar action posts to (CSRF-protected); answers with HTML | `apps/web/ui/` (`action(...)`) |
| sign-in endpoints | `api.Sessions`: sign-up, sign-in, sign-out, verification, password reset (and 2FA in admin) over `api.Authenticator`; the UI sets the cookies they return | `apps/*/internal/api/auth.go`, `apps/*/ui/cookies.go` |
| envelope | every JSON response: `{"data": T\|null, "error": string\|null}` | `api.WriteData`, `api.WriteError` |
| route handler | the one handler per transport every route goes through | `api.Route`, the UI's `page`/`action` |
| route access | the coarse gate: who may reach a route, declared per route as a policy | `apps/web/internal/access/` |
| access policy | `access.Public`, `access.SignedIn`, or a team's (`access.Role("admin")`) | `apps/web/internal/access/` |
| gate | `access.Gate`: resolves the caller and enforces a route's policy | `apps/web/internal/access/` |
| resource authorization | may this caller act on this record: a rule in the service, never a policy | `apps/web/internal/services/` |
| caller / actor | the request's user and session; the actor is how a service sees them | `access.Caller`, `services.Actor` |
| port / adapter | an interface a layer declares for what it needs; a thin type that satisfies it | next to its consumer, `apps/*/internal/server` |
| service error | a named `*services.Error` matched with `errors.Is`, plus kind sentinels | `apps/web/internal/services/` |
| mock / mock session | generated mockgen output; `apitest.AsUser` / `apitest.Anonymous` for handler tests | `services/mocks`, `api/mocks`, `api/apitest` |
| test boundary | one of the only places tests may live, each with allowed imports and a coverage floor | AGENTS.md "Test boundaries" |
| local verification | run what the change needs on the pushed head, then `make ci-local RAN="…"` attests it | `scripts/cilocal/`, the local-ci skill |
| attested / claimed / failed head | a head whose attestation holds / that `make ci-local` has touched / that failed and stays failed | `scripts/cilocal/verify` |
| release | a published `vX.Y.Z` on a `main` commit staging already ran; the only path to production | `Deploy production`, `docs/RELEASES.md` |
| routes config | `AppRoutes` (pages) and `APIRoutes` (endpoints) | `apps/web/internal/config/routes.go` |
| feature toggle | `config.Features`: email verification, template welcome, Expo dev origins | `apps/web/internal/config/features.go` |
| session | a `"session"` row behind the signed cookie (web) or bearer token (mobile) | `packages/auth/` |
| dev mailer | logs auth email links instead of sending them | `packages/auth/mailer.go` |
| live secret | a secret served by the provider that rotates without a redeploy | `apps/web/internal/config/secrets.go` |
| secrets provider | where live secrets come from (`SECRETS_PROVIDER`): `env` or `doppler` | `packages/secrets/` |
| keyring / data key | current and previous auth secrets; a data key wrapped by them protects data at rest | `packages/auth/keyring.go` |
| pool swap | `database.Pool.Swap`: a rotated `DATABASE_URL` probes, switches and drains | `packages/database/pool.go` |
| backup | an encrypted `pg_dump -Fc` stored as `<db>/<time>.dump.age` | `packages/backup/`, `apps/backup` |
| sandbox database | the dev Postgres from `make db-up`; the only database you may break | `docker-compose.yml` |
| uikit / theme | the shared templ components and design tokens; `theme.css` holds every colour | `packages/uikit/` |
| Datastar | the SSE hypermedia library the UI uses (core only) | `apps/web/ui/` |
| audit log | the append-only record of admin mutations (intent, then outcome) | `admin_audit_log`, `admin_audit_outcome` |
| ban | blocks sign-in and revokes every session | `auth.UserAdmin.BanUser` |

## Where code lives

- `apps/web/cmd/server`: the binary (`server serve`, `server migrate`).
- `apps/web/internal/config`: env config, `AppRoutes`/`APIRoutes`, `Features`.
- `apps/web/internal/server`: middleware (`apiOnly` hides the API and `/healthz` from browsers), `/healthz`, the mounts (auth at `/api/auth/`, API, UI at `/`), and the adapters from `packages/auth` to service ports (`AccountCredentials`) and to the access gate (`Callers`, the `access.Resolver`).
- `apps/web/internal/access`: route access: `Caller`, `Policy` (`Public`, `SignedIn`, yours), `Gate`. Both transports register routes with a policy; the gate decides.
- `apps/web/internal/apiclient`: the API as the UI calls it: `Client`, and aliases of the types, inputs, errors and access policies it uses. A contract only (no `net/http`).
- `apps/web/internal/api`: JSON API `/api/core/v1`: envelope, `Route` and `Endpoint` (served over HTTP, called in-process by `api.Client`), handlers, the service ports they call; `api/mocks` (generated) and `api/apitest` (mock session) for handler tests.
- `apps/web/internal/services`: business rules and the ports they need; `services/mocks` (generated) for service tests. `apps/web/internal/validate`: input types.
- `apps/web/ui`: templ pages, Datastar handlers (server actions that call `apiclient.Client`), i18n, static assets, and the browser security headers (`security.go`).
- `packages/uikit`: the templ components and design tokens both UIs share (`theme.css` is the one file to re-theme). It renders translated strings and values only; depguard keeps it from importing `apps/` or the data, auth and secrets packages.
- `packages/storage`: the optional S3 client, `safefetch` (SSRF-safe fetch of user URLs, rules injected from `services.PublicAddr`/`FetchURLAllowed`) and `imaging` (pure-Go decode/normalise to WebP; build with `-tags nodynamic`). Services reach it only through ports (`ObjectStore`, `ImageFetcher`, `ImageCodec`; depguard).
- `apps/backup`, `packages/backup`: the nightly encrypted Postgres backup and restore.
- `packages/database`: pool (`Pool`: swaps itself when `DATABASE_URL` rotates), `migrations/` (goose, embedded), `queries/` (sqlc input), `sqlc/` (generated, never edit), repositories, `model/`.
- `packages/secrets`: live secrets: `Manager` (polling, validation, last-good cache, staleness, change hooks) and providers (`Env`, `Doppler`; Infisical/1Password stubs behind build tags), `Secret`, and the redacting log handler. Self-contained; never reads the env.
- `packages/auth`: Better Auth–style auth: its own migrations, queries, endpoints (Better Auth paths and response shapes, not the envelope), sessions, mailers. Self-contained: it never imports `apps/` or `packages/database` (depguard).
- `apps/mobile-app`: the Expo app, which talks to the JSON API with bearer tokens.
- `apps/admin`: the optional admin dashboard (see "Admin app" in AGENTS.md); `packages/database/adminrepo`: its data layer (audit log, stats; migrations, queries and sqlc output under it).
- `deploy/`: the Railway setup, its own Go module (the Railway IaC SDK never enters the app's `go.mod`). `platform.yaml` is what a team edits; `platform/` turns it into the IaC graph; `.railway/railway.go` is the shim the Railway CLI evaluates; `cmd/railway-setup` does what IaC cannot (project, environments, domains, sealed secrets, previews). Run it through `make railway-plan` / `make railway-apply`, never by hand-editing Railway. Railway bugs we work around are commented where the workaround lives.
- Root: `Makefile`, `docker-compose.yml`, `Dockerfile`, `.golangci.yml`, `tools/` (pinned dev tools, run via `go tool -modfile=tools/go.mod`).
