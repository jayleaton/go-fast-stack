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

## Deploy (Railway)

`railway.json` builds the `Dockerfile`, runs `/server migrate` as the pre-deploy command, and health-checks `/healthz`. Set `APP_ENV=production`, `PUBLIC_URL`, `DATABASE_URL`, `AUTH_SECRET` (32+ chars), `RESEND_API_KEY` and `FROM_EMAIL`, plus `CLIENT_IP_HEADER=X-Real-IP` so auth rate limits see the client instead of Railway's proxy. Production turns on email verification. The server refuses to boot with a missing or invalid value and lists every problem. To rotate `RESEND_API_KEY` without a redeploy, serve it from Doppler instead (next section).

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
3. In Railway, on the web service of each environment, set `SECRETS_PROVIDER=doppler` and `DOPPLER_TOKEN=<that token>` (sealed). Remove `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` from the service: with `doppler` env copies are ignored. The pre-deploy `server migrate` reads `DATABASE_URL` from Doppler too.
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

**Logs never contain values.** A secret prints as `[redacted]` everywhere (fmt, slog, JSON), and so does every credential in config (`DATABASE_URL`, `AUTH_SECRET`, `DOPPLER_TOKEN`, the admin app's too); only the binaries' `cmd` packages reveal them, to build clients. Logs name changed keys and show a fingerprint (an HMAC under a random per-process key: it shows a change within one process, nothing more). Both binaries also redact every log record: password, token, API key, cookie and DSN fields, `Authorization`/`Cookie`/`Set-Cookie` headers, `Bearer` tokens, URL passwords, token/code query values and the token in emailed links. In development the dev mailer's links stay readable so you can copy them; in production they are redacted too.

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

- **The JSON API is not for browsers.** `/api/auth/*`, `/api/core/*` and `/healthz` answer any request that carries browser headers (`Sec-Fetch-*`, `Origin`, `Referer`, or `Accept: text/html`) with the same bare 404 as an unknown path, and never send CORS headers. A wrong method is a 404 too (no `405` + `Allow`). The mobile app (native fetch) and servers send none of these. The one exception: a top-level navigation to auth's two link endpoints when an emailed link's callback is a deep link or another origin (mobile); they only redirect, and an error they would answer as JSON is the same 404. Emailed links for the web UI open its pages directly.
- **The admin app has no JSON API on the network.** Its UI calls the services and its auth instance in-process; `/api/admin/v1` exists as code (`apps/admin/internal/api`, tested) but `cmd/admin` mounts none of it and no admin auth endpoint. To let scripts call it, pass its routers as `server.Deps.API` (browsers still get a 404).
- **Headers on every UI response:** a nonce-based CSP (`script-src 'self' 'nonce-…'`, `style-src 'self'`, `frame-ancestors 'none'`, no `unsafe-*`: Datastar compiles expressions into nonced scripts from `<html data-nonce>`), `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy: strict-origin`, `Permissions-Policy`, COOP/CORP, `Cache-Control: no-store` on pages and SSE, and HSTS behind https. Templates never use inline `style=` or `on*=` attributes (show/hide with `class="hidden"` + `data-class:hidden`). The CSP keeps out foreign scripts, styles, frames and form targets, but it is not the XSS defence: Datastar runs every `data-*` expression it finds with the page's nonce, so injected markup would run. templ's escaping is the defence; nothing may bypass it (`templ.Raw`, `templ.Unsafe*`, `templ.SafeScript*`, ...), and redirect targets from the query go through `safeNext`.
- **Errors are generic.** Panics answer a plain 500 (the envelope on the API); unknown service errors show "Something went wrong" and are logged; validation messages name JSON types, never Go types.
- **Cookies** are `HttpOnly`, `SameSite=Lax` and `Secure` behind https; the admin session uses the `__Host-` prefix (the web app keeps Better Auth's `__Secure-` names for the mobile client).
- **Secrets** are `secrets.Secret` values from config to the client that uses them (see [Secrets](#secrets)); logs are redacted.

Enforced by tests and lint, not review: each UI has an exposure crawl (`apps/*/ui/crawl_test.go`) that requests every registered route in every state, with markup and off-site redirect probes in user data and the query, and fails on `/api/` references, canary secrets, internal error text, Go type names, JSON, inline script/style, unescaped probes, off-site navigation, unsafe cookies or a missing header, and fails when a new route is not crawled; `TestNoUnescapedOutput` rejects templ's unescaped-output helpers in UI sources; `server_test.go` proves browsers get 404s from the API; depguard keeps UIs from importing their JSON API or `packages/secrets`, and forbidigo keeps `Secret.Reveal` in `cmd` (`make lint-layering` proves both bite).

## Admin dashboard (optional)

`apps/admin` is a separate binary and Railway service for the people who run the app: browse, search and inspect users (sessions, verification, role, ban state), ban (reason + expiry) and unban, revoke sessions, set roles, manage admins, and read the audit log of every admin action. An analytics page is a placeholder behind a port.

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

**Deploy:** a second Railway service from this repo with config file `apps/admin/railway.json` (builds `apps/admin/Dockerfile`, runs `/admin migrate` before deploy, health-checks `/healthz`, which pings both databases). Set `APP_ENV=production`, `ADMIN_PUBLIC_URL`, `ADMIN_DATABASE_URL` (the admin Postgres), `DATABASE_URL` (a reference to the app's Postgres), `ADMIN_AUTH_SECRET`, `ADMIN_BOOTSTRAP_EMAIL`, `RESEND_API_KEY`, `FROM_EMAIL` and `CLIENT_IP_HEADER=X-Real-IP`; to rotate them live, put the four secrets in the admin service's own Doppler config instead and set `SECRETS_PROVIDER=doppler` + `DOPPLER_TOKEN` (see "Secrets"). The database-password runbook applies to the admin Postgres too. Deploy the web service first: it migrates the app database the admin app reads. Not using it? Delete `apps/admin`, `packages/database/adminrepo` and the `admin-*` make targets.

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, test boundaries, how to add a service, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
