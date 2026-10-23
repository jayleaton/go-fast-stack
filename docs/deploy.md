# Deploy to Railway

One file decides the whole Railway project: [`deploy/platform.yaml`](../deploy/platform.yaml) (admin yes/no, which environments, previews, region, sizes). From a fresh clone, one command creates everything and deploys it: the project, the environments, the services and databases, the domains, the secrets (generated, or placeholders), and a first healthy deploy. It runs on Railway's [Infrastructure as Code](https://docs.railway.com/infrastructure-as-code) (Go authoring, beta). Re-running converges, and editing the file (add `qa`, turn on `admin`) updates the project.

Needs the [Railway CLI](https://docs.railway.com/cli) 5.71.0 or newer and Go. No dashboard or GitHub step first:

```sh
railway login
make railway-doctor   # login, workspace, SSH key, GitHub access, Doppler: prints fixes
make railway-setup    # asks once before creating the project; ends with every /healthz checked
```

**A real sender is required.** Every service runs with `APP_ENV=production`, which refuses to boot when `FROM_EMAIL` is unset or on an example or test domain (`example.com`, `example.net`, `example.org`, `.example`, `.test`, `.invalid`, `localhost`). So `make railway-plan`, `make railway-setup` and `make railway-apply` refuse first and touch nothing until `email.from` in `deploy/platform.yaml` is a sender on a domain your mail provider has verified. Per environment, `per_environment.<env>.from_email` overrides it (`manual`: you set `FROM_EMAIL` in Railway yourself). Setup on a terminal asks for the sender and writes it into `platform.yaml`. Otherwise pass `--email-from noreply@yourdomain.com` or `PLATFORM_EMAIL_FROM`, which are written into the file the same way. `make railway-doctor` reports it as a failed check.

The defaults come from the checkout: the project is named after your repo, and production is the only environment. Choose on the first run: `make railway-setup ARGS="--environments staging,production --admin true --workspace acme"`. These flags (and `--project`, `--region`, `--repo`; or `PLATFORM_*` env vars) are written into `platform.yaml`, so commit it afterwards.

`make railway-plan` shows what would change without touching anything (exit 2 when something would). `make railway-verify` checks `/healthz` on every public URL. `make railway-rotate-db` gives every Postgres a new superuser password (see below). Agents: see AGENTS.md "Deploy to Railway (for agents)" for `--yes`, `--json` and exit codes.

**Code source.** With `repo: auto` (the default: this checkout's `origin`), Railway builds each environment from its own branch once its GitHub app can read the repo: production from `branch` (`main`), and staging and qa from `dev` (or `branches.<env>`), so a merge to `dev` ships to staging and a `dev` → `main` merge releases to production. A production-only project deploys `main`. Setup prints which environment deploys which branch, and if a branch does not exist on GitHub yet it says so and lists `git push origin main:dev` as a next step (Railway's trigger is set anyway and fires on the first push). Until then, setup still creates everything and deploys your checkout by upload (`railway up`), then prints the [Railway GitHub app](https://github.com/apps/railway-app) install link for your account. Install it, pick the repo, and re-run `make railway-setup` to switch every service to deploy-on-push (no duplicate services). With `repo: ""`, setup never connects GitHub: deploy new code with `make railway-up` (`PLATFORM_ENV=staging` for one environment).

## Profiles

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

## Hand-set variables

The project's variables are managed: `make railway-apply` deletes any variable that IaC neither sets nor ignores. So list the ones you set yourself in `unmanaged_variables` (the template lists the optional settings its apps read: `TRUSTED_ORIGINS`, `APP_NAME`, `SECRETS_REFRESH_INTERVAL`, ...). Per environment, `per_environment.<env>` sets `public_url` / `admin_public_url` (`railway`, an https origin for a custom domain, or `manual`) and `from_email` (an address or `manual`). `manual` means you set the value in Railway yourself, and apply never sets, resets or deletes it; plan and apply print it as `left as it is`. `make deploy-vars` proves this offline. It builds every environment's graph and fails when a name in `.env.example` would be deleted, when a name is both set and ignored, or when a name in `DEPLOY_KEEP` (your own hand-set names) is set by IaC. `make deploy-check` runs it.

## Redeploying

`railway redeploy` without flags redeploys the service's **latest deployment as it was built**: the same image, whatever commit that was. When the newest entry is one Railway skipped (a push outside the watch paths) or removed, that can bring back an old build. To run the current code of the environment's branch, use `railway redeploy --from-source --service <svc> --environment <env> --yes`, or `--commit <sha>` for one specific commit. The plain form is right only when you want the running build again with new variables. Setup's own redeploys (`make railway-rotate-db`, retrying a Doppler switch) use `--from-source` when the project deploys from GitHub. In upload mode (`repo: ""`) there is no source to pull: redeploy, or `make railway-up`.

## Cost

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

## Preview environments

Off by default. `previews: true` (needs `staging`) turns on Railway PR environments: each pull request gets a temporary copy of staging, deleted when the PR closes. Each preview runs its own web + Postgres while open, at roughly a staging environment's cost. Sealed values are not copied into previews, so give previews their own `AUTH_SECRET` (and Doppler token) as shared variables if you rely on them.

## Under the hood

- `deploy/` is its own Go module, so the Railway SDK is never in the app's `go.mod` or binary.
- `deploy/platform` turns the YAML into the IaC graph.
- `deploy/.railway/railway.go` is what `railway config plan|apply --file deploy/.railway/railway.go --environment <env>` evaluates.
- `deploy/cmd/railway-setup` does what IaC cannot: create the project and environments, seal secrets, generate domains and toggle previews.
- There is no `railway.json` (Config as Code) any more, for web or admin: Railway stops reading those files on 2026-12-01.
- Railway bugs found on the way: [docs/railway-go-iac-issues.md](railway-go-iac-issues.md).

`/healthz` is only the deploy gate: Railway checks it until a new deployment goes live, never afterwards. It answers 503 when Doppler secrets go stale (`SECRETS_MAX_STALENESS`), so never wire it up as a liveness or restart probe, or a Doppler outage turns into a restart loop. Services restart only when the process exits (`ON_FAILURE`, 5 retries).

Every service runs with `APP_ENV=production` (email verification on, `RESEND_API_KEY` required), `CLIENT_IP_HEADER=X-Real-IP` and `PORT=3000`. The server refuses to boot with a missing or invalid value and lists every problem.
