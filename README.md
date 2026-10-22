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

Secrets that can change while the server runs come from one place, `SECRETS_PROVIDER`, through [`packages/secrets`](packages/secrets): a manager that fetches the whole set for this service and environment, validates it, and hands each consumer its value. Everything else stays a plain env var read once at boot.

| `SECRETS_PROVIDER` | What it is | Live rotation |
|---|---|---|
| `env` (default) | values from the process env (`.env`, Railway variables), read once at boot | no: changing a value means a redeploy |
| `doppler` | polls one [Doppler](https://www.doppler.com) config with a read-only service token, over plain `net/http` | yes, within `SECRETS_REFRESH_INTERVAL` |

**What rotates live (v1).**

| Secret | Source | Rotates without redeploy | Why not (yet) |
|---|---|---|---|
| `RESEND_API_KEY` | `SECRETS_PROVIDER` | yes, with `doppler`: the mailer rebuilds its Resend client on change | |
| `DATABASE_URL` | env | no | a new password needs a new `pgx` pool that authenticates and passes a probe before an atomic swap and a drain of the old pool; Railway Postgres credentials change through a redeploy anyway |
| `AUTH_SECRET` | env | no | it signs session cookies and verification/reset tokens: replacing it in place signs everyone out and breaks links already sent. Rotation needs a keyring (new key signs, old keys still verify during an overlap) |
| `DOPPLER_TOKEN` | env (sealed) | no | it is the credential that reaches the provider; update the Railway variable (redeploy), then revoke the old token |

**Doppler setup for a team.**

1. Create a Doppler workspace and a project (e.g. `myapp`). Each environment (`dev`, `stg`, `prd`) is a config; add `RESEND_API_KEY` to each. The Developer plan is free for three people and service tokens are free.
2. Per Railway environment, create a **read-only service token** for that environment's config (project → config → Access → Generate). One token per service per environment, so staging can't read production and a future admin service gets its own config.
3. In Railway, on the web service of each environment, set `SECRETS_PROVIDER=doppler` and `DOPPLER_TOKEN=<that token>` (sealed). Keep `DATABASE_URL`, `AUTH_SECRET` and the rest as Railway variables. Drop `RESEND_API_KEY` from Railway: with `doppler` the env copy is ignored.
4. Deploy. The log shows `secrets: loaded provider=doppler`. From now on, changing `RESEND_API_KEY` in Doppler reaches every replica within one interval (60 s ± 10 %): `secrets: rotated changed=[RESEND_API_KEY]`, then `mailer: resend client built resend_api_key=fp:…`. Revoke the old Resend key once every replica logged the new fingerprint.
5. Leave Doppler's Railway sync off for live keys: it writes Railway variables and redeploys, which is the `env` path.

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

**Deploy:** a second Railway service from this repo with config file `apps/admin/railway.json` (builds `apps/admin/Dockerfile`, runs `/admin migrate` before deploy, health-checks `/healthz`, which pings both databases). Set `APP_ENV=production`, `ADMIN_PUBLIC_URL`, `ADMIN_DATABASE_URL` (the admin Postgres), `DATABASE_URL` (a reference to the app's Postgres), `ADMIN_AUTH_SECRET`, `ADMIN_BOOTSTRAP_EMAIL`, `RESEND_API_KEY`, `FROM_EMAIL` and `CLIENT_IP_HEADER=X-Real-IP`. Deploy the web service first: it migrates the app database the admin app reads. Not using it? Delete `apps/admin`, `packages/database/adminrepo` and the `admin-*` make targets.

## Working on it

Read [AGENTS.md](AGENTS.md) (architecture, test boundaries, how to add a service, PR rules) and [CONTEXT.md](CONTEXT.md) (shared vocabulary).
