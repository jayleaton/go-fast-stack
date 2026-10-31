# Releases and production deploys

Staging always runs the latest `main`. Production runs a **release**: a
`vX.Y.Z` tag on a commit staging has already run. The standing rules are in
`AGENTS.md`, "Environments: staging is always current"; this page is how the
pipeline enforces them.

The layout it expects is one Railway project with two environments, `staging`
and `production` (`deploy/platform.yaml`: `environments: staging,production`),
created once with `make railway-setup` ([deploy.md](deploy.md)). Railway's own
GitHub auto-deploy stays off (`repo: ""`): only these workflows ship code.

## The flow

1. **Merge to `main`.** `make ci-local` verified the PR's head
   ([CI.md](CI.md)), then `Deploy staging` checks that attestation and ships
   the commit to Railway staging (see [Railway rollout](#railway-rollout)).
2. **Draft a release.** Actions → **Draft release** → Run workflow (`bump`
   patch, minor or major, or an exact `version`). It drafts a release at the
   commit staging runs now, with notes generated from the pull requests merged
   since the last release. Nothing is tagged yet. Edit the notes or the version
   if you like.
3. **Publish the release.** On the draft, click **Publish release**. That
   creates the tag and starts `Deploy production`. Publishing is the explicit
   yes every production deploy needs, so only people in `PRODUCTION_RELEASERS`
   can do it in a way that deploys. The run's gate checks the release (see
   [What is gated](#what-is-gated)) and refuses anything it cannot prove.
   An agent may do this step only when the developer explicitly tells it to
   publish or deploy (`gh release edit vX.Y.Z --draft=false` with an
   allowlisted account), after confirming staging ran that commit and is
   healthy, and adds "Authorized by <person>, published by <agent>" to the
   release notes.

Required reviewers on an environment exist only for public repositories or on
paid plans. Where they are available, add them to the `production` environment
and the deploy job also waits for an approval; nothing else changes.

Without the helper, step 2 is one command (the commit must be the one staging
runs, shown in the title of the latest successful `Deploy staging` run):

```sh
gh release create v1.4.0 --draft --target <staging commit sha> --generate-notes
```

### Railway rollout

Staging and production run the same script, `.github/scripts/deploy-railway.sh`,
which is the authority for the order. It uploads the checkout with
`railway up --service <name>` (nothing is built on GitHub) and waits for each
deployment before the next:

- `web`, whose pre-deploy command migrates the app and auth databases
  (`/server migrate`); a failed migration or `/healthz` stops the deploy with
  the old deployment still serving.
- `admin` when `deploy/platform.yaml` enables it (`/admin migrate` first).
- `backup` when backups are on (a cron service: it counts as deployed once
  Railway has it `SLEEPING` until its schedule).

The services come from `deploy/cmd/deploy-services`, which reads
`deploy/platform.yaml`. Then the readiness check: `<WEB_URL>/healthz` (and
`<ADMIN_URL>/healthz` when set) must answer 200. `DRY_RUN=1` prints every
command instead of running it.

The token is a Railway project token scoped to one environment, so no
`--project` or `--environment` flag is passed: the token picks both.

### Dry run

Until the repository variable `RAILWAY_DEPLOY` is `true`, `Deploy staging`
skips its deploy job and runs `Dry run (RAILWAY_DEPLOY is not true)`, which
prints the rollout. A fresh copy of the template therefore goes green without
a Railway project. A dry run is never proof for production: the gate reads the
`Deploy main to Railway staging` job, so `Deploy production` refuses every
release until staging has really deployed.

## What is gated

`Deploy staging` refuses a commit `make ci-local` did not attest
(`cilocal attest`: `local-ci` passed on the commit, or on the exact head of the
pull request whose merge made it), and a dispatch from anything but `main`.

`Deploy production` (`.github/scripts/production-gate.sh`) refuses, before any
production step, unless all of these hold. Every check runs and reports, so the
run summary shows everything that is wrong at once. Anything that cannot be
proven counts as a failure.

| Check | Why |
|---|---|
| The ref is a `vX.Y.Z` tag, resolved through `refs/tags/` | No branch or ad-hoc commit deploys |
| It is a published release, not a draft or pre-release | Pre-releases are skipped entirely |
| A dispatch was started from `main` | A dispatch runs the workflow file from its ref; only main's copy carries this gate |
| The person publishing or dispatching is in `PRODUCTION_RELEASERS` | One allowlist, kept in a repository variable |
| The `production` environment exists and admits only `main` and `v*`; `staging` admits only `main` | GitHub creates a missing environment unrestricted on first use; this stops that |
| An active tag ruleset restricts creating, moving and deleting `v*` | A tag runs the workflow file at its own commit, so only the people who may release may make one |
| The tag's commit is on `main` | No hotfixes straight to production |
| A successful `Deploy staging` run deployed that exact commit | Production only runs a commit staging ran and passed readiness on (run title, the run's success, and its `Deploy main to Railway staging` job) |
| Staging still runs that commit or a later `main` commit | Production may lag staging, never lead it |
| `STAGING_WEB_URL/healthz` answers 200 right now | Staging is healthy at release time |
| The tag still points at the same commit when the deploy job starts | A queued deploy starts later than its gate; a moved tag means what was checked would not ship |

Runs never overlap: the workflow uses the `deploy-production` concurrency group
without cancelling. A run started while another is running queues behind it;
GitHub keeps only the newest queued run.

The gate runs read-only from a laptop too (`gh` logged in):
`TAG=v1.0.0 SHA=$(git rev-parse origin/main) sh .github/scripts/production-gate.sh`.

### Deploy credentials

The gate lives inside `deploy-production.yml`, and a workflow always runs the
file at its own ref. So what keeps the production token away from a pushed
workflow is not the gate but these repository settings, which no workflow file
can change:

| Setting | What it admits | Why |
|---|---|---|
| Environment `production` | Branch `main`, tag pattern `v*`. Holds `RAILWAY_PRODUCTION_TOKEN` | A dispatch runs from `main` (reviewed through its ruleset); a release runs from its `v*` tag |
| Environment `staging` | Branch `main` only. Holds `RAILWAY_STAGING_TOKEN` | `Deploy staging` runs from `main` (its push trigger and catch-up's dispatch both do), so no branch workflow can read the token or put branch code on staging |
| Tag ruleset "Release tags" (`refs/tags/v*`) | Creation, update and deletion restricted; bypass: the repository Admin role | The environments admit `v*` tags, so only releasers may make one |

The gate fails closed when an environment or the ruleset is missing or
different. What these settings do not stop: a releaser's own credentials. A
leaked admin token can push a `v*` tag as easily as it can publish a release.

## Staging catch-up

`Staging catch-up` runs hourly (and by hand). When the latest successful
`Deploy staging` run's commit is not `main`'s head (a refused, failed or
cancelled deploy), it dispatches `Deploy staging` on `main`. A `main` that is
not attested is refused again, every hour, until someone attests it
([CI.md](CI.md#required-checks-and-staging)).

## Rollback or redeploy

Actions → **Deploy production** → Run workflow, branch `main`, and enter an
existing release tag (for example the previous one). The same gate applies, and
dispatching is the explicit yes, so only people in `PRODUCTION_RELEASERS` can.
An older release passes the staging checks as long as staging once ran its
commit and still runs that commit or a later one.

Migrations only move forward. Rolling back the code does not undo a migration,
so a release whose migration the older code cannot run against needs a forward
fix instead (merge it, let staging deploy it, release again).

## One-time setup

The pipeline is inert until this is done: staging dry-runs, and every
production run stops at the gate with an error naming what is missing.

1. **Create the project:** `make railway-setup ARGS="--environments staging,production ..."`
   (AGENTS.md, "Deploy to Railway"), with `repo: ""` in `deploy/platform.yaml`.
2. **Environments and the tag ruleset:** `.github/scripts/release-settings.sh`
   (an administrator's `gh` login) creates `staging` (branch `main`) and
   `production` (branch `main`, tag `v*`) and the "Release tags" ruleset,
   idempotently. It never touches secrets. Leave "Allow administrators to
   bypass configured protection rules" off.
3. **Secrets**, each a Railway project token created for that environment of
   the project (the token form's environment picker must say so):
   `gh secret set RAILWAY_STAGING_TOKEN --env staging` and
   `gh secret set RAILWAY_PRODUCTION_TOKEN --env production` (each reads the
   value from stdin). Never as repository secrets, which every branch can read.
4. **Repository variables** (Settings → Secrets and variables → Actions →
   Variables):
   - `PRODUCTION_RELEASERS`: comma-separated GitHub logins who may publish a
     release that deploys, dispatch a redeploy or rollback, and run Draft
     release.
   - `STAGING_WEB_URL`, `PRODUCTION_WEB_URL` (and `STAGING_ADMIN_URL`,
     `PRODUCTION_ADMIN_URL` with the admin app): the public origins
     `deploy-railway.sh` checks `/healthz` on.
   - `RAILWAY_DEPLOY` = `true` last, once the rest is in place.
   - Optional: `CI_RUNNER` and `RELEASE_RUNNER`, a JSON runner label (default
     `"ubuntu-latest"`).

## Where it lives

| File | Role |
|---|---|
| `.github/workflows/deploy-staging.yml` | Attestation, then the staging rollout on every push to `main` (or the dry run) |
| `.github/workflows/staging-catch-up.yml` | Hourly: dispatches `Deploy staging` when staging is behind `main` |
| `.github/workflows/draft-release.yml` | Drafts a release at the commit staging runs |
| `.github/workflows/deploy-production.yml` | Gate, then the production rollout |
| `.github/scripts/deploy-railway.sh` | The services in order, each waited for, then readiness checks |
| `.github/scripts/production-gate.sh` | The production gate |
| `.github/scripts/release-lib.sh` | Allowlist and staging proof shared by the gate and Draft release |
| `.github/scripts/release-settings.sh` | One-time environments and tag ruleset |
| `deploy/cmd/deploy-services` | Which services `deploy/platform.yaml` deploys, in order |

The `Deploy staging` run title (`Deploy staging <sha>`) and its job name
(`Deploy main to Railway staging`) are what the gate reads as proof, so keep
them unchanged.
