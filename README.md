# ai-patterns-go

A Go + Datastar monorepo template: one static binary serving server-rendered `templ` pages, a JSON API, and Better Auth–style auth on Postgres. It succeeds [ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo) and keeps the same layered patterns.

## Quick start

Needs Go (version in `mise.toml`; `mise install`) and Docker.

```sh
make db-up      # Postgres 18 + pgweb on 127.0.0.1, ports per checkout (creates .env)
make migrate    # auth migrations, then app migrations
make dev        # http://<this-machine>:3000, reachable over LAN/Tailscale
```

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

## Deploy to Railway

One file decides the whole Railway project: [`deploy/platform.yaml`](deploy/platform.yaml) (admin yes/no, which environments, previews, region, sizes). `make railway-apply` makes Railway match it using Railway's [Infrastructure as Code](https://docs.railway.com/infrastructure-as-code) (Go authoring, beta). Re-running converges, and editing the file (add `qa`, turn on `admin`) updates the project.

Needs the [Railway CLI](https://docs.railway.com/cli) 5.71.0 or newer, logged in, and Go. From clone to live:

```sh
railway login
$EDITOR deploy/platform.yaml   # project name, repo, environments, admin
make railway-apply             # creates the project, environments, services, databases, domains, secrets
```

`make railway-plan` shows what would change without touching anything. `make railway-verify` checks `/healthz` on every public URL.

**Code source.** With `repo: owner/name`, Railway builds on every push to `branch`. Install the [Railway GitHub app](https://github.com/apps/railway-app) on that repo first, or the first apply fails with "You don't have access to the GitHub repo". With `repo: ""`, nothing deploys on push: `make railway-up` uploads your checkout (`PLATFORM_ENV=staging` for one environment).

### Profiles

| `environments:` | For | Services per environment |
|---|---|---|
| `production` (default) | trying it, solo projects | `web` + `postgres` |
| `staging,production` | teams: recommended | same, twice |
| `staging,qa,production` | a real QA sign-off step | same, three times |

`admin: true` adds `admin` (the dashboard, `apps/admin`) and its own `admin-postgres` to every environment; admins never live in the app database. New environments are created empty: no copied data or secrets.

Each service gets:
- its Dockerfile built from the repo root;
- `/server migrate` (or `/admin migrate`) as the pre-deploy command;
- `/healthz` gating each deploy;
- `DATABASE_URL` as a reference to its Postgres;
- a generated `*.up.railway.app` domain behind `PUBLIC_URL`;
- watch paths, so a change to `apps/admin/**` never rebuilds `web` (and `packages/**` rebuilds both).

Removing something from `platform.yaml` (an environment, `admin`) never deletes it unless you run `CONFIRM_DESTRUCTIVE=1 make railway-apply`. Environments you drop are left alone and listed.

### Cost

Railway bills what containers actually use: about $10 per GB of memory per month, $20 per vCPU per month, and $0.15 per GB of volume. The plan minimum includes that much usage: Hobby $5/month, Pro $20/month. Idle footprint, measured on Railway on 2026-10-22:
- web: about 75 MB, under 0.001 vCPU.
- Postgres: 60–80 MB, under 0.001 vCPU, and about 0.9 GB of volume.
- admin: about 70 MB, the same as web in the same local `docker stats` run.

| Profile | Services | Idle compute/month | Fits in |
|---|---|---:|---|
| `production` | 2 | ≈ $1.55 | Hobby ($5) |
| `staging,production` | 4 | ≈ $3.10 | Hobby |
| `staging,qa,production` | 6 | ≈ $4.65 | Hobby |
| `production` + admin | 4 | ≈ $3.00 | Hobby |
| `staging,production` + admin | 8 | ≈ $6.00 | Hobby (≈ $6 bill) / Pro ($20) |
| `staging,qa,production` + admin | 12 | ≈ $9.00 | Hobby (≈ $9 bill) / Pro ($20) |

Real traffic, egress ($0.05/GB) and backups come on top. Each Postgres also gets Railway's public TCP proxy (`DATABASE_PUBLIC_URL`); IaC cannot opt out of it yet. The caps in `resources:` only bound a runaway process.

### Secrets and Doppler

Setup provisions per service and environment, and never overwrites a value that exists:
- `AUTH_SECRET` / `ADMIN_AUTH_SECRET`: generated and sealed (nobody can read them back, not even you).
- `SECRETS_PROVIDER`
- a `DOPPLER_TOKEN` placeholder.

IaC never touches these variables (`variables.ignore`), so `railway config plan` won't show or delete them.

With `secrets.provider: doppler` (the default), each service reads `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` from its own Doppler config, and they rotate without a redeploy (see [Secrets](#secrets)). Until a service has a real token it runs with `SECRETS_PROVIDER=env` on the Railway values, so the project works on day one. Production still needs `RESEND_API_KEY` there.

To switch a service to Doppler:
1. Fill its Doppler config: `DATABASE_URL` (the Postgres service's private URL), `AUTH_SECRET` and `RESEND_API_KEY`. The admin config also needs `ADMIN_DATABASE_URL` and `ADMIN_AUTH_SECRET`.
2. Create a read-only service token per service and environment.
3. Run:

```sh
make railway-secrets   # asks for each token (input hidden), seals it, switches that service to doppler
# or non-interactively:
DOPPLER_TOKEN_WEB_PRODUCTION=dp.st.xxx DOPPLER_TOKEN_WEB_STAGING=dp.st.yyy make railway-secrets
```

Without Doppler, set `secrets.provider: env` and give `RESEND_API_KEY` the same way (`RESEND_API_KEY_PRODUCTION=re_xxx make railway-secrets`). Your local `.env` is never sent to Railway.

Railway currently gives a database added to a second environment the literal text of its password generator as its password, a public string ([issue draft 0](docs/railway-go-iac-issues.md)). `make railway-apply` detects this and sets a random password inside the database over `railway ssh`. That needs an SSH key registered with Railway (`railway ssh keys`).

### Preview environments

Off by default. `previews: true` (needs `staging`) turns on Railway PR environments: each pull request gets a temporary copy of staging, deleted when the PR closes. Each preview runs its own web + Postgres while open, at roughly a staging environment's cost. Sealed values are not copied into previews, so give previews their own `AUTH_SECRET` (and Doppler token) as shared variables if you rely on them.

### Under the hood

- `deploy/` is its own Go module, so the Railway SDK is never in the app's `go.mod` or binary.
- `deploy/platform` turns the YAML into the IaC graph.
- `deploy/.railway/railway.go` is what `railway config plan|apply --file deploy/.railway/railway.go --environment <env>` evaluates.
- `deploy/cmd/railway-setup` does what IaC cannot: create the project and environments, seal secrets, generate domains and toggle previews.
- The root `railway.json` (Config as Code) is gone; Railway stops reading those files on 2026-12-01. Services created by IaC never read `apps/admin/railway.json` either.
- Railway bugs found on the way: [docs/railway-go-iac-issues.md](docs/railway-go-iac-issues.md).

`/healthz` is only the deploy gate: Railway checks it until a new deployment goes live, never afterwards. It answers 503 when Doppler secrets go stale (`SECRETS_MAX_STALENESS`), so never wire it up as a liveness or restart probe, or a Doppler outage turns into a restart loop. Services restart only when the process exits (`ON_FAILURE`, 5 retries).

Every service runs with `APP_ENV=production` (email verification on, `RESEND_API_KEY` required), `CLIENT_IP_HEADER=X-Real-IP` and `PORT=3000`. The server refuses to boot with a missing or invalid value and lists every problem.

## Secrets

Secrets come from one place, `SECRETS_PROVIDER`, through [`packages/secrets`](packages/secrets): a manager that fetches the whole set for this service and environment, validates it, and hands each consumer its value. With `doppler`, **every secret rotates without a redeploy except `DOPPLER_TOKEN`** itself. Non-secret settings stay plain env vars read once at boot.

| `SECRETS_PROVIDER` | What it is | Live rotation |
|---|---|---|
| `env` (default) | values from the process env (`.env`, Railway variables), read once at boot | no: changing a value means a redeploy |
| `doppler` | polls one [Doppler](https://www.doppler.com) config with a read-only service token, over plain `net/http` | yes, within `SECRETS_REFRESH_INTERVAL` |

**What rotates live.**

| Secret | With `doppler`, a change in Doppler… |
|---|---|
| `DATABASE_URL` | opens a new pool, pings it and checks it is migrated at least as far as this binary, then switches atomically; queries already running finish on the old pool, which closes once they are done (at most 30 s). If the new URL fails, the old pool stays (and `/healthz` only goes red if the old database fails too). Migrations stay a deploy step (`server migrate`). |
| `AUTH_SECRET` + `AUTH_SECRET_PREVIOUS` | updates the auth keyring: the new secret signs; previous ones (comma-separated) still verify for `AUTH_SECRET_GRACE`, so no session, email link or trusted device breaks. Cookies signed with an old secret are re-signed on their next request. 2FA secrets are protected by data keys the auth secret only *wraps*, so they are rewrapped, never lost. |
| `RESEND_API_KEY` | rebuilds the mailer's Resend client. |
| `DOPPLER_TOKEN` | — it is the credential that reaches Doppler: update the Railway variable (redeploy), then revoke the old token. |

With `env`, all of these are read at boot and change with a redeploy. The admin service does the same with its own Doppler config: `DATABASE_URL` and `ADMIN_DATABASE_URL` each go through a `database.Pool`, the `ADMIN_AUTH_SECRET` keyring (`_PREVIOUS`, `_REVOKED`, grace `ADMIN_AUTH_SECRET_GRACE`) through the admin auth instance's `SetSecrets`, and `RESEND_API_KEY` rebuilds its mailer. Its `/healthz` also reports stale secrets.

**Doppler setup for a team.**

1. Create a Doppler workspace and a project (e.g. `myapp`). Each environment (`dev`, `stg`, `prd`) is a config; add `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` to each (`AUTH_SECRET_PREVIOUS` only while rotating). The Developer plan is free for three people and service tokens are free.
2. Per Railway environment, create a **read-only service token** for that environment's config (project → config → Access → Generate). One token per service per environment, so staging can't read production and a future admin service gets its own config.
3. `make railway-secrets` (see "Deploy to Railway") stores each token sealed on its service and environment and sets `SECRETS_PROVIDER=doppler`, which redeploys it. The Railway copies of `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` are then ignored. The pre-deploy `server migrate` reads `DATABASE_URL` from Doppler too, so put the Postgres service's private `DATABASE_URL` in the Doppler config first.
4. Deploy. The log shows `secrets: loaded provider=doppler`. From now on a change in Doppler reaches every replica within one interval (60 s ± 10 %): `secrets: rotated changed=[…]`, then the consumer's line (`database: DATABASE_URL rotated`, `auth: AUTH_SECRET rotated`, `mailer: resend client built`) with fingerprints. Revoke an old credential upstream once every replica logged the new fingerprint.
5. Leave Doppler's Railway sync off for live keys: it writes Railway variables and redeploys, which is the `env` path.

**Runbook: rotate the database password (Railway Postgres, zero downtime).** Postgres has one password per role, so changing the app's own password in place fails new connections until the URL catches up. Use two login roles that act as one owner and alternate between them.

One-time setup, in `railway connect Postgres` (psql as the superuser; `railway` is Railway's default database name):

```sql
CREATE ROLE app_owner NOLOGIN;
CREATE ROLE app_blue  LOGIN PASSWORD '<random>' IN ROLE app_owner;
CREATE ROLE app_green LOGIN PASSWORD '<random>' IN ROLE app_owner;
ALTER ROLE app_blue  SET role = 'app_owner';   -- sessions act as the owner, so migrations create owner-owned tables
ALTER ROLE app_green SET role = 'app_owner';
ALTER DATABASE railway OWNER TO app_owner;
ALTER SCHEMA public OWNER TO app_owner;
DO $$ DECLARE r record; BEGIN               -- tables created before, by the superuser
  FOR r IN SELECT format('ALTER TABLE public.%I OWNER TO app_owner', tablename) AS q FROM pg_tables WHERE schemaname = 'public'
  LOOP EXECUTE r.q; END LOOP; END $$;
```

Set Doppler's `DATABASE_URL` to `postgresql://app_blue:<password>@<host>:<port>/railway` (host and port from the Postgres service's private `DATABASE_URL`). Don't touch the Postgres service's own `POSTGRES_PASSWORD`/`PGPASSWORD`: they belong to the superuser and redeploy the database. Each rotation:

1. Give the idle role a new password: `ALTER ROLE app_green PASSWORD '<new random>';`
2. In Doppler, point `DATABASE_URL` at `app_green` with that password. Within one interval each replica logs `database: switched to a new pool … to=app_green@… from=app_blue@…`, then `old pool drained and closed`.
3. Check nothing still uses the old role: `SELECT usename, count(*) FROM pg_stat_activity WHERE datname = current_database() GROUP BY 1;`
4. Kill the old credential: `ALTER ROLE app_blue PASSWORD '<random>';` Next time, go the other way.

A URL that can't connect or points at an unmigrated database is refused (`database: probe …`) and the current pool keeps serving; fix the value in Doppler and the next fetch retries.

**Runbook: rotate `AUTH_SECRET`.** In one Doppler save, set `AUTH_SECRET=<new>` (`openssl rand -base64 32`) and `AUTH_SECRET_PREVIOUS=<old>`. Every replica logs `auth: AUTH_SECRET rotated`; new sessions are signed with the new secret, existing ones keep working and are re-signed when used, emailed links stay valid, 2FA is untouched. The old secret is accepted until `AUTH_SECRET_GRACE` after the first replica saw the rotation (the time is stored in the database, so every replica ends the window together, and an expired secret can't be revived by listing it again). Remove `AUTH_SECRET_PREVIOUS` afterwards.
- **Several replicas:** a replica that hasn't polled yet doesn't know the new secret for up to one interval. To avoid that, first add the new value alone to `AUTH_SECRET_PREVIOUS`, wait one interval, then do the save above.
- **Leaked secret:** in one save, set the new `AUTH_SECRET`, leave `AUTH_SECRET_PREVIOUS` empty and put the leaked value in `AUTH_SECRET_REVOKED`. Everything it signed is rejected at once (everyone signs in again; emailed links stop working) and its row is marked expired, so it can never verify again, even if someone lists it as previous later. 2FA enrolments survive: the revoked value is only used to unwrap the data keys, which are then rewrapped with the new secret. Keep it in `AUTH_SECRET_REVOKED` until every replica logged the rotation (with the `env` provider, until the redeploy is done), then remove it. Rotating with the old value listed nowhere also revokes it, but a replica that restarts before a running one has rewrapped can't load the 2FA keys and refuses to boot until you add it to `AUTH_SECRET_REVOKED`.
- **Rolling back** to an older secret is a rotation too: a secret whose window has ended signs again but stays expired, so rotate forward to a fresh value instead.
- **Default grace** is the longest lifetime of anything the secret signs: the session lifetime (7 days), or the trust-device lifetime (30 days) when 2FA is on. A shorter `AUTH_SECRET_GRACE` signs out users idle for longer than it.

**When the provider misbehaves.** No valid first snapshot means the server does not boot (the deploy fails; the previous one keeps serving). Once running, a failed or invalid fetch (malformed body, missing `RESEND_API_KEY` in production) never replaces the last good values; the manager retries with backoff (5 s doubling, capped at min(5 min, max staleness ÷ 3), honouring `Retry-After`). After `SECRETS_MAX_STALENESS` without a good fetch, `/healthz` answers 503 `secrets stale` and every failed poll logs an error. Clients already built keep their last good key. `/healthz` is a readiness check: don't wire it to a liveness probe that restarts the process, because a fresh process can't load secrets during the outage either.

**Logs never contain values.** A secret prints as `[redacted]` everywhere (fmt, slog, JSON), and so does every credential in config (`DATABASE_URL`, `AUTH_SECRET`, `DOPPLER_TOKEN`, the admin app's too); only the binaries' `cmd` packages reveal them (`reveal.Value`), to build clients. Logs name changed keys and show a fingerprint (an HMAC under a random per-process key: it shows a change within one process, nothing more). Both binaries also redact every log record: password, token, API key, cookie and DSN fields, `Authorization`/`Cookie`/`Set-Cookie` headers, `Bearer` tokens, URL passwords, token/code query values and the token in emailed links. In development the dev mailer's links stay readable so you can copy them; in production they are redacted too.

**Local development.** Keep `SECRETS_PROVIDER=env` and `.env`, or run `doppler run -- make dev` to inject a dev config as env at startup. To try live rotation locally, put a dev-config service token in `.env` with `SECRETS_PROVIDER=doppler`.

| Variable | Default | |
|---|---|---|
| `SECRETS_PROVIDER` | `env` | `env` or `doppler` |
| `DOPPLER_TOKEN` | | required with `doppler`: read-only service token for this service's config |
| `DOPPLER_API_URL` | `https://api.doppler.com` | https in production; point it at a fake for tests |
| `SECRETS_REFRESH_INTERVAL` | `60s` | at least `10s` (Doppler Developer allows 120 reads/min per workspace) |
| `SECRETS_MAX_STALENESS` | `15m` | at least twice the interval |
| `AUTH_SECRET_GRACE` | session / trust-device lifetime | how long a retired `AUTH_SECRET` still verifies (not a secret: plain env) |

Secrets read from `SECRETS_PROVIDER`: `DATABASE_URL`, `AUTH_SECRET`, `AUTH_SECRET_PREVIOUS` and `AUTH_SECRET_REVOKED` (comma-separated, each at least 32 characters), `RESEND_API_KEY`. Every fetch is validated before it replaces the last good set.

Infisical and 1Password adapters are stubs with an implementation plan (`packages/secrets/infisical.go`, `onepassword.go`, outside the default build).

## Security

The browser only ever talks to the UI: server-rendered pages, Datastar actions that answer with HTML fragments over SSE, and the embedded static assets. Nothing it receives names the JSON API.

- **The JSON API is not for browsers.** `/api/auth/*`, `/api/core/*` and `/healthz` answer any request that carries browser headers (`Sec-Fetch-*`, `Origin`, `Referer`, or `Accept: text/html`) with the same bare 404 as an unknown path, and never send CORS headers. A wrong method is a 404 too (no `405` + `Allow`). The mobile app (native fetch) and servers send none of these. The one exception: a top-level navigation to auth's two link endpoints when an emailed link's callback is a deep link or another origin (mobile); they only redirect, and an error they would answer as JSON is the same 404. Emailed links (verify, reset, invite) open UI pages directly, and opening one spends nothing: mail scanners and link previews fetch links, so each page asks for a click and only that POST spends the token (the page carries it only as a hidden form field, with `no-store` and, behind https, `no-referrer`). The mobile app's verification links use the same page, which then hands over to the app's deep link.
- **The admin app has no JSON API on the network.** Its UI calls the services and its auth instance in-process; `/api/admin/v1` exists as code (`apps/admin/internal/api`, tested) but `cmd/admin` mounts none of it and no admin auth endpoint. To let scripts call it, pass its routers as `server.Deps.API` (browsers still get a 404).
- **Headers on every UI response:** a nonce-based CSP (`script-src 'self' 'nonce-…'`, `style-src 'self'`, `frame-ancestors 'none'`, no `unsafe-*`: Datastar compiles expressions into nonced scripts from `<html data-nonce>`), `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy: strict-origin`, `Permissions-Policy`, COOP/CORP, `Cache-Control: no-store` on pages and SSE, and HSTS behind https. Templates never use inline `style=` or `on*=` attributes (show/hide with `class="hidden"` + `data-class:hidden`). The CSP keeps out foreign scripts, styles, frames and form targets, but it is not the XSS defence: Datastar runs every `data-*` expression it finds with the page's nonce, so injected markup would run. templ's escaping is the defence; nothing may bypass it (`templ.Raw`, `templ.Unsafe*`, `templ.SafeScript*`, ...), and redirect targets from the query go through `safeNext`.
- **Errors are generic.** Panics answer a plain 500 (the envelope on the API); unknown service errors show "Something went wrong" and are logged; validation messages name JSON types, never Go types.
- **Cookies** are `HttpOnly`, `SameSite=Lax` and `Secure` behind https; the admin session uses the `__Host-` prefix (the web app keeps Better Auth's `__Secure-` names for the mobile client).
- **Secrets** are `secrets.Secret` values from config to the client that uses them (see [Secrets](#secrets)); logs are redacted.

Enforced by tests and lint, not review: each UI has an exposure crawl (`apps/*/ui/crawl_test.go`) that requests every registered route in every state, with markup and off-site redirect probes in user data and the query, and fails on `/api/` references, canary secrets, internal error text, Go type names, JSON, inline script/style, unescaped probes, off-site navigation, unsafe cookies or a missing header, and fails when a new route is not crawled; `TestNoUnescapedOutput` rejects templ's unescaped-output helpers in UI sources; `server_test.go` proves browsers get 404s from the API; depguard keeps UIs from importing their JSON API or `packages/secrets`, and keeps `packages/secrets/reveal` (`reveal.Value`, the only way to read a secret: `Secret` has no method that returns it) in `cmd` (`make lint-layering` proves both bite, and that reading a secret around `reveal.Value` does not compile).

## Admin dashboard (optional)

`apps/admin` is a separate binary and Railway service for the people who run the app: browse, search and inspect users (sessions, verification, role, ban state), ban (reason + expiry) and unban, revoke sessions, set roles, manage admins, and read the audit log of every admin action. Admin changes fail closed: if the audit log cannot record the attempt, nothing is changed, and an attempt whose outcome was never recorded shows as "outcome unknown" until a reconciler checks the current state. Sign-ins, sign-outs and refused 2FA codes are audited best-effort after the fact (they never block authentication), so the log can miss one if it was down at that moment. An analytics page is a placeholder behind a port.

- **Isolated.** Admins are users of the admin app's own auth instance in its own database (`ADMIN_DATABASE_URL`, cookie `admin.session_token`, secret `ADMIN_AUTH_SECRET`), never rows in the app's `user` table. It reads and administers the app's users through `DATABASE_URL`. depguard keeps `apps/web` and `apps/admin` from importing each other and keeps the higher-privilege `packages/database/adminrepo` out of the web app.
- **Invite only.** There is no sign-up. `admin migrate` seeds `ADMIN_BOOTSTRAP_EMAIL` without a password; `admin invite <email>` (or `make admin-invite EMAIL=...`, or the Admins page) emails a one-time set-password link. With `ADMIN_REQUIRE_2FA=true` (the default in production) setting the password continues into TOTP enrolment with backup codes before the first session.
- **Banning** revokes every session of the user at once; the web app refuses their sign-in until the ban is lifted or expires.

```sh
make db-up                         # also creates the "admin" database on the dev Postgres
make migrate                       # the app database (web)
make admin-dev                     # migrates the admin database, seeds ADMIN_BOOTSTRAP_EMAIL, serves 0.0.0.0:3001
make admin-invite EMAIL=admin@example.com   # dev: the set-password link is printed in the log
```

Locally the admin database is a second database on the same per-checkout Postgres: no extra container, and worktrees stay isolated. Hosted, give it its own small Postgres so its credentials and backups are separate from the app's.

**Deploy:** set `admin: true` in `deploy/platform.yaml` and run `make railway-apply` (see "Deploy to Railway"). It adds the `admin` service (builds `apps/admin/Dockerfile`, runs `/admin migrate` before deploy, health-checks `/healthz`, which pings both databases) and its own `admin-postgres` to every environment. It also wires `ADMIN_DATABASE_URL`, `DATABASE_URL` (a reference to the app's Postgres) and `ADMIN_PUBLIC_URL`, and generates a sealed `ADMIN_AUTH_SECRET`. Set `admin_bootstrap_email` there for the first admin. To rotate the admin secrets live, give the admin service its own Doppler config and token (see "Secrets"). The database-password runbook applies to the admin Postgres too. Not using it? Delete `apps/admin`, `packages/database/adminrepo` and the `admin-*` make targets.

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, test boundaries, how to add a service, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
