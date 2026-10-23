# Secrets

Secrets come from one place, `SECRETS_PROVIDER`, through [`packages/secrets`](../packages/secrets): a manager that fetches the whole set for this service and environment, validates it, and hands each consumer its value. With `doppler`, **every secret rotates without a redeploy except `DOPPLER_TOKEN`** itself. Non-secret settings stay plain env vars read once at boot.

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
3. `make railway-secrets` (see [Deploy to Railway](deploy.md)) stores each token sealed on its service and environment and sets `SECRETS_PROVIDER=doppler`, which redeploys it. The Railway copies of `DATABASE_URL`, `AUTH_SECRET` and `RESEND_API_KEY` are then ignored. The pre-deploy `server migrate` reads `DATABASE_URL` from Doppler too, so put the Postgres service's private `DATABASE_URL` in the Doppler config first.
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

## Secrets and Doppler

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
2. Creates a read-only service token named `railway <project> <service>` (unless that config already has one), seals it in Railway as `DOPPLER_TOKEN`, and sets `SECRETS_PROVIDER=doppler`.
3. Once the new deployment is healthy on Doppler, removes the Railway copies (`AUTH_SECRET` and its `_PREVIOUS`/`_REVOKED`, `ADMIN_AUTH_SECRET` likewise, `RESEND_API_KEY`) without another deploy. "Healthy" means the newest deployment Railway built (not one it skipped for a push outside the watch paths) is `SUCCESS`. If the switch deploy fails, the old deployment keeps running on the Railway copies and setup keeps them. Fix the value in Doppler and re-run: setup redeploys and removes them once that deploy succeeds. The IaC `DATABASE_URL` reference stays and is ignored.

**Staged environments.** `per_environment.<env>.secrets: staged` (in `deploy/platform.yaml`) does steps 1 and 2 without switching: Doppler is filled in and `DOPPLER_TOKEN` is sealed with no deploy, while the services keep `SECRETS_PROVIDER=env` and their Railway secrets. Use it for production until a release should move it. Set it to `doppler` (or remove the line) and re-run `make railway-apply` to switch. `make railway-plan` lists every Doppler step it would take, `make railway-doctor` reports staged services and any secret still on Railway for a service on Doppler, and `make deploy-vars` fails when `unmanaged_variables` or the graph would keep a Doppler-served secret as a plain Railway variable.

Re-running changes nothing. Local dev: setup points this checkout at the `dev` config (`doppler setup`) and gives it an `AUTH_SECRET`; `USE_DOPPLER=1 make dev` runs with it, and plain `make dev` still uses `.env`. Tokens are never printed; setup reads them straight from the Doppler CLI into Railway.

To set one by hand instead (another provider account, say), `make railway-secrets` asks for each token (input hidden) and seals it: `DOPPLER_TOKEN_WEB_PRODUCTION=dp.st.xxx make railway-secrets`.

Without Doppler, set `secrets.provider: env` and give `RESEND_API_KEY` the same way (`RESEND_API_KEY_PRODUCTION=re_xxx make railway-secrets`). Your local `.env` is never sent to Railway.

Railway currently gives a database added to a second environment the literal text of its password generator as its password, a public string ([issue draft 0](railway-go-iac-issues.md)). `make railway-apply` detects this and sets a random password inside the database over `railway ssh`. That needs an SSH key registered with Railway (`railway ssh keys`).

## Where secrets live, and rotating after an incident

Keep app secrets in **Doppler**, not in Railway variables. A Railway variable changes only with a redeploy and sits with whoever runs your hosting. Doppler values rotate live and stay readable to you, which a rotation needs.

| Where | What |
|---|---|
| Doppler, one config per service and environment | `DATABASE_URL`, `AUTH_SECRET` (+ `AUTH_SECRET_PREVIOUS` / `_REVOKED` while rotating), `RESEND_API_KEY`, and the sign-in provider credentials you use (`GOOGLE_CLIENT_SECRET`, `APPLE_PRIVATE_KEY`, `APPLE_KEY_ID`; add them yourself, [sign-in](sign-in.md)); admin also `ADMIN_DATABASE_URL`, `ADMIN_AUTH_SECRET` |
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
5. **Backups** (see [Backups and restore](backups.md)):
   - **Encryption key:** `BACKUP_ENCRYPTION_KEY` lives on Railway, so treat it as leaked. Move it to `BACKUP_ENCRYPTION_KEY_PREVIOUS` and set a new one in one deploy:
     ```sh
     old=$(railway variable list --service backup --environment production --json | jq -r .BACKUP_ENCRYPTION_KEY)
     printf %s "$old" | railway variable set BACKUP_ENCRYPTION_KEY_PREVIOUS --stdin --service backup --environment production --skip-deploys
     openssl rand -base64 48 | tr -d '\n' | railway variable set BACKUP_ENCRYPTION_KEY --stdin --service backup --environment production
     ```
     New backups use the new key; older ones still restore with the previous one. Store the new key in your password manager. Once the last backup made with the old key has aged out of retention (about 4 weeks with the defaults: `make railway-backups` shows the dates), delete `BACKUP_ENCRYPTION_KEY_PREVIOUS`.
   - **Bucket credentials:** Railway rotated storage-bucket credentials in its 2026-10-21/22 incident. The backup service reads them as references, which Railway resolves at deploy time, so redeploy it to pick up the new ones: `railway redeploy --from-source --service backup --environment production` (plain `redeploy` can bring back an old build: [docs/deploy.md](deploy.md#redeploying)). Then take a backup to prove it works: `make railway-backup PLATFORM_ENV=production`. With `storage: external`, rotate the access key at your provider instead and set the new `BACKUP_S3_ACCESS_KEY_ID` / `BACKUP_S3_SECRET_ACCESS_KEY`.
   - Backups taken before the rotation still restore: a dump holds no database password. It does hold the `authKey` rows, wrapped by the `AUTH_SECRET` of that time. After restoring such a backup, keep the old secret in `AUTH_SECRET_REVOKED` until the app has started once (it rewraps the 2FA keys with the current secret and never accepts the old one), then remove it.

If the incident is at Doppler instead, rotate the same values in Doppler and revoke and reissue its tokens (step 1); the Railway side is untouched.
