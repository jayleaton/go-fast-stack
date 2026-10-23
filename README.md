# Go Fast Stack

<p align="center"><a href="https://x.com/jayleaton"><img src="https://img.shields.io/badge/Follow-%40jayleaton-000000?logo=x&logoColor=white" alt="Follow @jayleaton"></a> <a href="https://buymeacoffee.com/jayleaton"><img src="https://img.shields.io/badge/Buy_Me_a_Coffee-Support-FFDD00?logo=buymeacoffee&logoColor=black" alt="Buy Me a Coffee: Support"></a></p>

A Go + Datastar monorepo template: one static binary serving server-rendered `templ` pages, a JSON API, and Better Auth–style auth on Postgres, with the same layered patterns as its predecessor.

Go Fast Stack succeeds [jayleaton/ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo) (this repo was previously named `ai-patterns-go`).

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

## Deploy to Railway

One file decides the whole Railway project: [`deploy/platform.yaml`](deploy/platform.yaml) (admin yes/no, which environments, previews, region, sizes). From a fresh clone, one command creates everything and deploys it: the project, the environments, the services and databases, the domains, the secrets (generated, or placeholders), and a first healthy deploy. It runs on Railway's [Infrastructure as Code](https://docs.railway.com/infrastructure-as-code) (Go authoring, beta). Re-running converges, and editing the file (add `qa`, turn on `admin`) updates the project.

Needs the [Railway CLI](https://docs.railway.com/cli) 5.71.0 or newer and Go. No dashboard or GitHub step first:

```sh
railway login
make railway-doctor   # login, workspace, SSH key, GitHub access, Doppler: prints fixes
make railway-setup    # asks once before creating the project; ends with every /healthz checked
```

The defaults come from the checkout: the project is named after your repo, and production is the only environment. Choose on the first run: `make railway-setup ARGS="--environments staging,production --admin true --workspace acme"`. These flags (and `--project`, `--region`, `--repo`; or `PLATFORM_*` env vars) are written into `platform.yaml`, so commit it afterwards.

`make railway-plan` shows what would change without touching anything (exit 2 when something would). `make railway-verify` checks `/healthz` on every public URL. `make railway-rotate-db` gives every Postgres a new superuser password (see below). Agents: see AGENTS.md "Deploy to Railway (for agents)" for `--yes`, `--json` and exit codes.

**Code source.** With `repo: auto` (the default: this checkout's `origin`), Railway builds on every push to `branch` once its GitHub app can read the repo. Until then, setup still creates everything and deploys your checkout by upload (`railway up`), then prints the [Railway GitHub app](https://github.com/apps/railway-app) install link for your account. Install it, pick the repo, and re-run `make railway-setup` to switch every service to deploy-on-push (no duplicate services). With `repo: ""`, setup never connects GitHub: deploy new code with `make railway-up` (`PLATFORM_ENV=staging` for one environment).

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
- `RESEND_API_KEY`: from `RESEND_API_KEY_<ENV>` in your shell (sealed), else a placeholder, so production boots (emails fail until you set a real key; setup lists it as a next step).
- `AUTH_SECRET` / `ADMIN_AUTH_SECRET`: generated, not sealed. Rotating one needs its old value (`AUTH_SECRET_PREVIOUS` / `_REVOKED`), and a sealed value can never be read back.
- `SECRETS_PROVIDER`
- a `DOPPLER_TOKEN` placeholder.

IaC never touches these variables (`variables.ignore`), so `railway config plan` won't show or delete them.

With `secrets.provider: doppler` (the default), each service reads `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` from Doppler, and they rotate without a redeploy (see [Secrets](#secrets)). The [Doppler CLI](https://docs.doppler.com/docs/cli) is pinned in `mise.toml` (`mise install`). Run `doppler login` once, and `make railway-setup` moves everything over itself (or later: `make doppler-setup`, same flags, `--json` and exit codes). Not logged in? Services run on the Railway values (`SECRETS_PROVIDER=env`) until you are.

What it sets up in the Doppler project (`secrets.project`, default `go-fast-stack`, created if missing; `auto`: the Railway project's name):

| Railway | Doppler environment | web reads | admin reads |
|---|---|---|---|
| production | `prd` | `prd` | `prd_admin`, a branch of `prd` |
| staging | `stg` | `stg` | `stg_admin` |
| qa (only if chosen) | `qa` | `qa` | `qa_admin` |
| your machine | `dev` | `dev` (`USE_DOPPLER=1 make dev`) | |

A branch config inherits its root (`DATABASE_URL`, `RESEND_API_KEY`), and admin-only secrets (`ADMIN_DATABASE_URL`, `ADMIN_AUTH_SECRET`) live in the branch, so a web token can never read them. Per service and environment, setup:
1. Seeds the config from Railway, never overwriting a value already in Doppler. `DATABASE_URL` and `ADMIN_DATABASE_URL` come from Railway. The auth secrets are Railway's values: a new one would stop the app from booting, because it wraps the 2FA keys. `RESEND_API_KEY` is Railway's value or a placeholder.
2. Creates a read-only service token named `railway <project> <service>`, seals it in Railway as `DOPPLER_TOKEN`, and sets `SECRETS_PROVIDER=doppler`.
3. Once the new deployment is healthy on Doppler, removes the Railway copies (`AUTH_SECRET`, `ADMIN_AUTH_SECRET`, `RESEND_API_KEY`) without another deploy. The IaC `DATABASE_URL` reference stays and is ignored.

Re-running changes nothing. Local dev: setup points this checkout at the `dev` config (`doppler setup`) and gives it an `AUTH_SECRET`; `USE_DOPPLER=1 make dev` runs with it, and plain `make dev` still uses `.env`. Tokens are never printed; setup reads them straight from the Doppler CLI into Railway.

To set one by hand instead (another provider account, say), `make railway-secrets` asks for each token (input hidden) and seals it: `DOPPLER_TOKEN_WEB_PRODUCTION=dp.st.xxx make railway-secrets`.

Without Doppler, set `secrets.provider: env` and give `RESEND_API_KEY` the same way (`RESEND_API_KEY_PRODUCTION=re_xxx make railway-secrets`). Your local `.env` is never sent to Railway.

Railway currently gives a database added to a second environment the literal text of its password generator as its password, a public string ([issue draft 0](docs/railway-go-iac-issues.md)). `make railway-apply` detects this and sets a random password inside the database over `railway ssh`. That needs an SSH key registered with Railway (`railway ssh keys`).

### Where secrets live, and rotating after an incident

Keep app secrets in **Doppler**, not in Railway variables. A Railway variable changes only with a redeploy and sits with whoever runs your hosting. Doppler values rotate live and stay readable to you, which a rotation needs.

| Where | What |
|---|---|
| Doppler, one config per service and environment | `DATABASE_URL`, `AUTH_SECRET` (+ `AUTH_SECRET_PREVIOUS` / `_REVOKED` while rotating), `RESEND_API_KEY`; admin also `ADMIN_DATABASE_URL`, `ADMIN_AUTH_SECRET` |
| Railway, sealed | `DOPPLER_TOKEN`, the only secret Railway needs |
| Railway, plain | non-secrets (`APP_ENV`, `PUBLIC_URL`, `SECRETS_PROVIDER`, ...) |

Once a service runs `SECRETS_PROVIDER=doppler` (after `make railway-secrets`), delete its Railway copies, for example `railway variable delete AUTH_SECRET --service web --environment production`. Do the same for `RESEND_API_KEY`, and on admin for `ADMIN_AUTH_SECRET`. Setup no longer recreates them for a service on Doppler. Two values unavoidably stay on Railway:
- each Postgres service's own superuser password;
- the IaC `DATABASE_URL` reference (ignored under Doppler).

That is why the runbook below gives the app its own login roles.

**After a provider incident**, treat every value the provider held as leaked and rotate it. Railway's 2026-10-21 security disclosure is such a case: assume every Railway variable leaked.
1. **Doppler tokens:** create a new read-only service token per service and environment in Doppler. Run `make railway-secrets`, which seals them and redeploys. Then revoke the old tokens in Doppler.
2. **Database passwords:** `make railway-rotate-db` (one environment: `PLATFORM_ENV=production`) sets a new random superuser password inside each Postgres over `railway ssh`, then stores it. Services that read `DATABASE_URL` from Railway are redeployed; for services on Doppler it tells you to update Doppler. If the app uses its own roles, also run the database-password runbook in [Secrets](#secrets).
3. **Auth secrets:** on Doppler, follow the **Leaked secret** step of the `AUTH_SECRET` runbook in [Secrets](#secrets): new `AUTH_SECRET`, old value in `AUTH_SECRET_REVOKED`, both in one save (same for `ADMIN_AUTH_SECRET`). Still on the `env` provider? Do it in Railway in one deploy:
   ```sh
   old=$(railway variable list --service web --environment production --json | jq -r .AUTH_SECRET)
   printf %s "$old" | railway variable set AUTH_SECRET_REVOKED --stdin --service web --environment production --skip-deploys
   openssl rand -base64 48 | tr -d '\n' | railway variable set AUTH_SECRET --stdin --service web --environment production
   ```
   Delete `AUTH_SECRET_REVOKED` once the new deployment is live. Everyone signs in again, and 2FA enrolments survive.
4. **Everything else in Doppler:** roll `RESEND_API_KEY` in Resend, save it in Doppler (live), then revoke the old key.
5. **Backups** (see [Backups and restore](#backups-and-restore)):
   - **Encryption key:** `BACKUP_ENCRYPTION_KEY` lives on Railway, so treat it as leaked. Move it to `BACKUP_ENCRYPTION_KEY_PREVIOUS` and set a new one in one deploy:
     ```sh
     old=$(railway variable list --service backup --environment production --json | jq -r .BACKUP_ENCRYPTION_KEY)
     printf %s "$old" | railway variable set BACKUP_ENCRYPTION_KEY_PREVIOUS --stdin --service backup --environment production --skip-deploys
     openssl rand -base64 48 | tr -d '\n' | railway variable set BACKUP_ENCRYPTION_KEY --stdin --service backup --environment production
     ```
     New backups use the new key; older ones still restore with the previous one. Store the new key in your password manager. Once the last backup made with the old key has aged out of retention (about 4 weeks with the defaults: `make railway-backups` shows the dates), delete `BACKUP_ENCRYPTION_KEY_PREVIOUS`.
   - **Bucket credentials:** Railway rotated storage-bucket credentials in its 2026-10-21/22 incident. The backup service reads them as references, which Railway resolves at deploy time, so redeploy it to pick up the new ones: `railway redeploy --service backup --environment production`. Then take a backup to prove it works: `make railway-backup PLATFORM_ENV=production`. With `storage: external`, rotate the access key at your provider instead and set the new `BACKUP_S3_ACCESS_KEY_ID` / `BACKUP_S3_SECRET_ACCESS_KEY`.
   - Backups taken before the rotation still restore: a dump holds no database password. It does hold the `authKey` rows, wrapped by the `AUTH_SECRET` of that time. After restoring such a backup, keep the old secret in `AUTH_SECRET_REVOKED` until the app has started once (it rewraps the 2FA keys with the current secret and never accepts the old one), then remove it.

If the incident is at Doppler instead, rotate the same values in Doppler and revoke and reissue its tokens (step 1); the Railway side is untouched.

### Preview environments

Off by default. `previews: true` (needs `staging`) turns on Railway PR environments: each pull request gets a temporary copy of staging, deleted when the PR closes. Each preview runs its own web + Postgres while open, at roughly a staging environment's cost. Sealed values are not copied into previews, so give previews their own `AUTH_SECRET` (and Doppler token) as shared variables if you rely on them.

### Under the hood

- `deploy/` is its own Go module, so the Railway SDK is never in the app's `go.mod` or binary.
- `deploy/platform` turns the YAML into the IaC graph.
- `deploy/.railway/railway.go` is what `railway config plan|apply --file deploy/.railway/railway.go --environment <env>` evaluates.
- `deploy/cmd/railway-setup` does what IaC cannot: create the project and environments, seal secrets, generate domains and toggle previews.
- There is no `railway.json` (Config as Code) any more, for web or admin: Railway stops reading those files on 2026-12-01.
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

1. Create a Doppler workspace and a project (e.g. `go-fast-stack`). Each environment (`dev`, `stg`, `prd`) is a config; add `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` to each (`AUTH_SECRET_PREVIOUS` only while rotating). The Developer plan is free for three people and service tokens are free.
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

## Backups and restore

Every project gets a nightly backup of the app database (and the admin one with `admin: true`), on by default (`backups:` in `deploy/platform.yaml`):

- **What:** `pg_dump -Fc` (compressed), encrypted with [age](https://age-encryption.org) under `BACKUP_ENCRYPTION_KEY`, stored as `app/<UTC time>.dump.age` in a [Railway storage bucket](https://docs.railway.com/storage-buckets) (S3-compatible, no other account). Logs show the backup id, size and a SHA-256 fingerprint, never data, URLs or keys.
- **Who:** a small always-on service, `backup` (`apps/backup`, image `postgres:18-alpine`, whose `pg_dump` always matches the server). It sleeps until `backups.schedule` (cron, UTC; default 03:00), backs up, then prunes. A failed night is logged and retried the next night. It stays up so you can `railway ssh` in; restores run inside Railway over the private network, so no database is exposed and no secret leaves Railway.
- **Kept:** the newest backup of each of the last 7 days (`keep_daily`) and of each of the last 4 ISO weeks (`keep_weekly`), plus everything from the last 24 hours (a manual backup before a risky migration) and always the newest. Objects the job did not write are never touched.
- **After a provider incident** (for example Railway's 2026-10-21/22 one, which invalidated tokens and rotated bucket credentials), rotate the encryption key through `_PREVIOUS` and redeploy the backup service: step 5 of "After a provider incident" in [Where secrets live](#where-secrets-live-and-rotating-after-an-incident).
- **Key:** setup generates `BACKUP_ENCRYPTION_KEY` once per environment (`railway variable list --service backup`). **Copy it into your password manager now**: a backup without its key cannot be restored. To rotate it, put the old value in `BACKUP_ENCRYPTION_KEY_PREVIOUS` and a new one in `BACKUP_ENCRYPTION_KEY`; older backups still decrypt. Storage keys and database URLs are read afresh at every run (Railway references, or Doppler with `SECRETS_PROVIDER`), so rotating them needs no redeploy.

**Restore runbook (Railway).** Needs only the Railway CLI with an SSH key (`railway ssh keys`, the same as `make railway-rotate-db`).

```sh
make railway-backups PLATFORM_ENV=production                 # ids, newest first
make railway-backup  PLATFORM_ENV=production                 # take one now (e.g. before a risky migration)
make railway-restore PLATFORM_ENV=production ID=app/20261022T030000Z.dump.age YES=1          # into an empty database
make railway-restore PLATFORM_ENV=production ID=app/20261022T030000Z.dump.age YES=1 FORCE=1  # over the live one
```

A restore refuses without `YES=1` (exactly `1`), and refuses a database that is not empty unless `FORCE=1`, which **replaces everything in it**: every user schema is dropped (tables, views, functions, extensions, including ones newer migrations added) and the backup is replayed, all in one transaction, so a failure leaves the database as it was. It always targets the database the backup came from (`app/…` into the app database, `admin/…` into the admin one); the job refuses a mismatch, because both databases share auth's tables. To rehearse, restore into a scratch database: `railway ssh --service backup`, then `RESTORE_DATABASE_URL=<scratch URL> backup restore <id> --yes`. Writes made during a restore are lost: pause traffic first if that matters, and run `server migrate` afterwards (the web service's pre-deploy does it on the next deploy) if the backup predates the current schema. Practise a restore once when you set the project up. A single backup object is limited to 5 GB (one S3 PUT); bigger databases need multipart upload, not built yet.

**Local.** `make db-backup` backs up this checkout's Postgres into `.backups/` (gitignored; `BACKUP_ADMIN=1` adds the admin database), `make db-backups` lists, `make db-restore ID=... YES=1 [FORCE=1]` restores. They run the job in Docker on the compose network, so no local `pg_dump` is needed. Set `BACKUP_S3_*` in `.env` to use any S3-compatible bucket instead of the directory.

**Off Railway.** The Railway bucket lives in the same project as the database: deleting the project deletes both (after Railway's 48 h grace). For copies that survive that, set `backups.storage: external` and point the backup service's `BACKUP_S3_ENDPOINT`, `BACKUP_S3_BUCKET`, `BACKUP_S3_REGION`, `BACKUP_S3_ACCESS_KEY_ID`, `BACKUP_S3_SECRET_ACCESS_KEY` (and `BACKUP_S3_PATH_STYLE=true` if your provider needs it) at any S3-compatible bucket: Cloudflare R2 (10 GB free, no egress fees), Backblaze B2 ($6/TB/month) or AWS S3. In an emergency a backup restores without this repository: `age -d -o app.dump app/<time>.dump.age` (passphrase: the key), then `pg_restore --no-owner -d "$DATABASE_URL" app.dump`.

**Railway's own backups.** On the Pro plan, Railway can also snapshot the Postgres volume (daily kept 6 days, weekly 27, monthly 89; restore mounts the snapshot as a new volume in the same project). It is configured in the dashboard only (the CLI and IaC cannot set it yet), is not available on Hobby, and wiping the volume deletes its snapshots, so it complements these backups rather than replacing them: turn it on too if you are on Pro.

**Cost.** Railway bills the bucket at $0.015 per GB-month (no request or download fees) and uploads from a service as egress ($0.05/GB). The service idles at a few MB (≈ $0.10/month). For a 1 GB database (a dump of roughly 200 MB): 11 kept backups ≈ 2.2 GB ≈ $0.03/month storage, plus ≈ 6 GB of uploads ≈ $0.30/month. A template-sized database costs fractions of a cent.

## Security

The browser only ever talks to the UI: server-rendered pages, Datastar actions that answer with HTML fragments over SSE, and the embedded static assets. Nothing it receives names the JSON API.

- **The JSON API is not for browsers.** `/api/auth/*`, `/api/core/*` and `/healthz` answer any request that carries browser headers (`Sec-Fetch-*`, `Origin`, `Referer`, or `Accept: text/html`) with the same bare 404 as an unknown path, and never send CORS headers. A wrong method is a 404 too (no `405` + `Allow`). The mobile app (native fetch) and servers send none of these. The one exception: a top-level navigation to auth's two link endpoints when an emailed link's callback is a deep link or another origin (mobile); they only redirect, and an error they would answer as JSON is the same 404. Emailed links (verify, reset, invite) open UI pages directly, and opening one spends nothing: mail scanners and link previews fetch links, so each page asks for a click and only that POST spends the token (the page carries it only as a hidden form field, with `no-store` and, behind https, `no-referrer`). The mobile app's verification links use the same page, which then hands over to the app's deep link.
- **The admin app has no JSON API on the network.** Its UI calls the services and its auth instance in-process; `/api/admin/v1` exists as code (`apps/admin/internal/api`, tested) but `cmd/admin` mounts none of it and no admin auth endpoint. To let scripts call it, pass its routers as `server.Deps.API` (browsers still get a 404).
- **Headers on every UI response:** a nonce-based CSP (`script-src 'self' 'nonce-…'`, `style-src 'self'`, `frame-ancestors 'none'`, no `unsafe-*`: Datastar compiles expressions into nonced scripts from `<html data-nonce>`), `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy: strict-origin`, `Permissions-Policy`, COOP/CORP, `Cache-Control: no-store` on pages and SSE, and HSTS behind https. Templates never use inline `style=` or `on*=` attributes (show/hide with `class="hidden"` + `data-class:hidden`). The CSP keeps out foreign scripts, styles, frames and form targets, but it is not the XSS defence: Datastar runs every `data-*` expression it finds with the page's nonce, so injected markup would run. templ's escaping is the defence; nothing may bypass it (`templ.Raw`, `templ.Unsafe*`, `templ.SafeScript*`, ...), and redirect targets from the query go through `safeNext`.
- **Errors are generic.** Panics answer a plain 500 (the envelope on the API); unknown service errors show "Something went wrong" and are logged; validation messages name JSON types, never Go types.
- **Cookies** are `HttpOnly`, `SameSite=Lax` and `Secure` behind https; the admin session uses the `__Host-` prefix (the web app keeps Better Auth's `__Secure-` names for the mobile client).
- **Secrets** are `secrets.Secret` values from config to the client that uses them (see [Secrets](#secrets)); logs are redacted.

Enforced by lint, not review: depguard keeps UIs from importing their JSON API or `packages/secrets`, and keeps `packages/secrets/reveal` (`reveal.Value`, the only way to read a secret: `Secret` has no method that returns it) in `cmd` (`make lint-layering` proves both bite, and that reading a secret around `reveal.Value` does not compile). Tests live only in the services, route handlers and `packages/auth` (AGENTS.md "Test boundaries").

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

## License

[MIT](LICENSE). Use it for anything, including commercial projects; keep the copyright notice.
