# CI

The checks a change needs run on the agent's machine before it is attested ([Local verification](#local-verification)); `make ci-local` itself runs nothing. GitHub runs no verification on `main`, and on a pull request only two small jobs that check the attestation. Actions minutes are billed per job, rounded up to the whole minute, and a PR's checks cost about two of them.

## What runs

`CI` (`.github/workflows/ci.yml`) runs on pull request edits alone (`pull_request_target`, [below](#trusted-code-only)). `make ci-local` posts the `local-ci` commit status and then fills the PR body, and that edit starts the run that checks both; a push or a new PR starts nothing. Until `make ci-local` claims a head (a `local-ci` status on it, or a section naming it: `verify.ClaimsHead`), an edit's run names its checking job `Awaiting make ci-local`, so the required check shows "Expected", never red, and merging stays blocked (a job skipped by `if` would count as passed, which is why it is a rename). Red is a real problem: a failed `local-ci`, or a hand-edited section for this head. Its jobs:

- `Secret scan` (required): the PR's changed files with gitleaks ([below](#secret-scan)). It does not depend on the agent's machine.
- `Local verification claim`: whether `make ci-local` has claimed the head (`cilocal check --claim`).
- `Local verification` (required; `Awaiting make ci-local` until claimed): the `local-ci` status and the PR body's section on the head (`cilocal check`).

### Trusted code only

The required checks run only the code on `main`, never the pull request's. On `pull_request_target` GitHub takes the workflow from the base branch and checks out the base, so a branch or fork that rewrites `scripts/cilocal` (a Go `init()` that exits 0), `.gitleaks.toml` or `ci.yml` itself changes nothing the jobs do until it merges. They read the head only as data: its body and statuses through the API, and its files from git's objects (`cilocal secret-scan --head <sha>`), never a checkout of it. A fork's head fails `Local verification`, `make pr-merge` and the staging gate: `make ci-local` attests only a branch pushed to this repository, and a fork's author writes its body. `verify.TrustProblems` holds `ci.yml` to this, and a verify test fails on a `pull_request` trigger, a checkout of a ref or repository, an expression inside a `run` script, or a scan or check that does not read the head as data. A change to `ci.yml`, the scanner or its config therefore takes effect on the pull requests after it merges, not on its own.

A newer edit cancels the PR's older run (`ci-pr-<n>`). The jobs run on the `CI_RUNNER` repository variable, a JSON `runs-on` value (`'["self-hosted","linux"]'`), or GitHub-hosted `ubuntu-latest` without it. They build `scripts/cilocal`, which imports only the standard library, so they download no modules.

A push to `main` runs only `Deploy staging` (`deploy-staging.yml`), which deploys a commit `make ci-local` attested to Railway staging ([Required checks and staging](#required-checks-and-staging)); production runs releases ([RELEASES.md](RELEASES.md)). `Post-merge report` comments on the merged PR when a push workflow fails on `main`. `Bench dev loop` still runs on `bench/**` pushes and by hand.

### What to run

Run what your change needs on the head you attest, and nothing more. **`make check-pr` is the default:** it runs the rows below that match the branch's changes (lint and `-race` tests on the affected packages, or `make ci` when a shared input changed), stops at the first failure, and prints the `make ci-local RAN="…"` line naming exactly what ran (`ARGS=--dry-run` only lists it). `scripts/affected.sh` (`make affected`) picks the Go dirs and packages: the changed packages and everything that imports them, or everything when go.mod, tooling, lint config, a migration or sqlc's config, queries or generated code changed. It fails, and so does everything built on it, when its selection does not match the diff: a changed Go file outside `deploy/` in no package, a changed package not selected, or Go changes and nothing selected. `make check-fast` is the seconds-long pre-push pass over the same packages.

| Changed | Run |
|---|---|
| Any file | `make secret-scan` (gitleaks on what the branch changed); `gofmt -l` on changed Go files |
| Go packages | `make lint LINT_DIRS=<dirs>` (gofmt, golangci-lint with the layering rules, namelint, the test-boundary and UI API-path checks) and `make test TEST_PKGS=<pkgs>` (`-race`, a throwaway Postgres, time budgets, the tested packages' coverage floors); `make check-fast` for a quick pass |
| sqlc queries, templ, mocks or other `go:generate` inputs | `make check-generated` |
| A migration | `make check-migrations` |
| `deploy/` or `deploy/platform.yaml` | `make deploy-check` (vet, lint and names in the `deploy/` module, then `make deploy-vars`) |
| `.github/workflows/` or `.github/actions/` | `make actionlint` |
| A `go.mod`/`go.sum` (any module), `package.json` or `pnpm-lock.yaml` | `make vulncheck` ([Known vulnerabilities](#known-vulnerabilities)) |
| A Dockerfile, or code a Dockerfile builds | `make release-smoke` |
| `apps/mobile-app`, or the route table its test reads (`apps/web/internal/config/routes.go`) | its pnpm checks in `apps/mobile-app`: `pnpm install --frozen-lockfile`, `tsc`, `expo lint`, `expo install --check`, `pnpm test` |
| Tooling, the Makefile, lint config, or everything | `make ci` (every check on the checkout, the probes `make lint-layering lint-tests` included) |

The probes (`make lint-layering`, `make lint-tests`) prove the layering and test rules still bite; run them when you change those rules or the scripts behind them. Say exactly what ran in the attestation: a reviewer reads the scope from it.

### Known vulnerabilities

`make vulncheck` (`scripts/vulncheck`) runs govulncheck, pinned in `tools/go.mod`, on every Go module (`.`, `deploy`, and the tool-only `tools` and `tools/actionlint` through their `tool` pattern), and `pnpm audit` on `apps/mobile-app/pnpm-lock.yaml`. It fails on every finding, called or not: govulncheck reports at symbol (our code calls it), package (imported) and module (required) level, and a fix that exists gets taken (`go get <module>@<fixed>` then `go mod tidy` in that module; a pnpm bump or `pnpm.overrides`). `make ci` always runs it; `make check-fast` runs it when a manifest or lockfile changed, and only warns when a scanner cannot run (exit 2: offline, or no pnpm). Lockpick's comment on each PR reports advisories published since against unchanged dependencies; fix those in their own PR off `main`.

`scripts/vulncheck/allowlist.txt` is the only way past a finding: an advisory with no fix, or none its consumer can take, that we cannot reach, or reach only in dev tooling (expo CLI, eslint) or on one client device; the reason says which. Each line is `<id> <package> <review-by> <reason>` (the GO- id for Go, the GHSA id for npm). The check fails on an entry past its review date, on an entry that no longer matches anything, and on a listed Go advisory that our code calls. Before adding a Go entry, prove the package is not imported (`go list -deps -test ./...` in each module).

### Nightly

A pull request runs only what its change affects, so `make nightly` runs everything on `main` once a day: `make ci`, `make test-flaky` (`go test -race -count=5 -shuffle=on` against Postgres) and `make secret-scan ARGS=--all`, in a detached checkout of `origin/main` (`.cache/nightly`, scripts/nightly.sh).

- **Green:** it attests the commit (`make ci-local COMMIT=<sha>`) when nothing has, so "Staging catch-up" can ship it.
- **Red:** it opens a "Nightly failed on main" issue (or comments on the open one) with the failing step and the log's tail, keeps the checkout for a look, and exits 1. It never posts a failed `local-ci`: that is final for the commit, and a flake would keep it off staging. A failure is fixed forward like any other on `main`.

It runs from cron on the dev box (the machine with Docker, `gh` and the `CI_RUNNER` runner), not as a workflow, so it needs no Actions minutes and runs where Actions is off:

    0 3 * * * cd ~/src/go-fast-stack && make nightly >> ~/.cache/nightly.log 2>&1

`NIGHTLY_REPORT=0 make nightly` runs it with no fetch and no GitHub calls (`NIGHTLY_COMMIT=<rev>` picks the commit).

## Local verification

`make ci-local` (`scripts/cilocal run`) runs no checks: it tells the PR you ran what CI would have checked, and says what that was. Run what the change needs ([What to run](#what-to-run)), commit, push, then attest:

    make ci-local RAN="make lint test LINT_DIRS=./apps/web/... TEST_PKGS=./apps/web/internal/services"

- **Full checks in a box.** `make box-ci COMMIT=<sha>` (scripts/box-ci.sh) runs `check-generated`, `lint`, `build` and `test` on exactly that commit's tracked files in a Davit box, deleting the box's stale files itself (it keeps `.env`, `bin/`, `tmp/`, `node_modules/`, `.cache/`), and ends with one `box-ci: PASS <sha>` or `box-ci: FAIL <sha> (<steps>)` line: attest what it printed. Never hand-write the `davit box run -- bash -lc "... rm ..."` it replaces.
- **What it checks.** A clean worktree whose HEAD is the pushed branch head (`Head.Refusal`), and no failed `local-ci` ever posted on that head. It reads the checkout again just before it posts, and posts only for exactly that clean, pushed head and tree (`verify.PublishRefusal`). It takes seconds: no worktree checkout, no slot, no queue.
- **What it posts.** The `local-ci` status on that exact SHA, whose description carries a digest of the section (`<result> · body <digest> · by <author> on <host>`), then the body's "Local verification" section (`scripts/cilocal/verify`, template [.github/pull_request_template.md](../.github/pull_request_template.md)): head, tree, who attested where, and the statement (`RAN` may span lines; they are joined with `; `). The edit starts the PR run that checks both; when the body was already current it re-runs that run (its failed jobs, or all of it when it ran only as awaiting). Before the PR exists it posts the status only: open the PR from the template and run it again.
- **A failure.** `make ci-local FAILED="<what failed>"` posts `failure`, final for that head. A failure in what the branch changed is fixed in the PR; any other is fixed forward in its own PR off `main` with a regression test that fails before the fix, and the branch rebases on it. Never attest a head until green.
- **Enforced.** The `Local verification` job (`cilocal check`), `make pr-merge` and the staging gate (`cilocal attest`) refuse a PR unless `local-ci` is `success` on its exact head, the section names that head and says what ran, it matches the status digest, and the head never failed (`verify.LocalVerificationProblems`). A new push or rebase needs a new attestation.
- **Main commits.** `make ci-local COMMIT=<sha> RAN="…"` in a clean detached checkout attests a `main` commit after the fact (an admin merge, a direct push). It reads the commit's first-parent ancestors for the last one the staging gate accepts (at most 50) and says the range the statement covers.
- **A new project's first attestation.** A repository made from this template has no attested commit in its own statuses, so nothing narrows what changed: run `make ci` (every check, the whole tree) on `main`'s head, then `make ci-local RAN="make ci"` on `main` (or `COMMIT=<sha>` in a detached checkout of it). With no attested main commit before the one it attests, `make ci-local` refuses a statement that does not name `make ci` (`verify.FirstAttestationProblem`); a root commit is fine. From then on each merge is attested on its PR head.
- **Trust.** It is a signoff: someone with write access says what they ran. Nothing on GitHub runs the checks again, so a wrong one shows up on staging, which is why the merging agent checks the deploy.

## Secret scan

`cilocal secret-scan` copies the files a change touches (the branch's diff since the merge-base in `make secret-scan`; the pull request's file list in the `Secret scan` job, or every file of the head when GitHub cannot list them, read from the head's objects: `--head`) and scans them with gitleaks 8.30.1, downloaded once into the user cache directory and checked against its release SHA-256. A finding fails the step or job, printing the rule, file and line with the value redacted.

[.gitleaks.toml](../.gitleaks.toml) keeps gitleaks' own rules and allows only the dev-loop bench script's known synthetic values: a rule-scoped allowlist for `generic-api-key` that pairs the script's exact path with those exact values (`condition = "AND"`), never a path on its own. Tests, `testdata` and example env files get no exemption, so a real credential in one fails the scan. The one file skipped is templ's generated Go: `x_templ.go` beside an `x.templ` (`verify.GeneratedTempl`). Its strings come from the `.templ`, which is scanned, and its generated identifiers read to gitleaks as API keys; a `_templ.go` with no `.templ` beside it is scanned, and `make check-generated` fails one that templ did not generate. Over the whole tree it reports nothing (`make secret-scan ARGS=--all`). A new deliberate fixture gets its own obviously fake value and the same kind of exact entry; `scripts/cilocal/verify`'s `TestScannerPolicyAllowlistsValuesOnly` fails a blanket path allowlist. A real secret is removed and rotated, because history keeps it. **The scan's policy is never the change's.** `.gitleaks.toml` and `.gitleaksignore` (`verify.ScannerPolicy`) come only from the trusted commit: `main` in the `Secret scan` job ([Trusted code only](#trusted-code-only)), the merge-base in `make secret-scan`, with gitleaks' default rules and an empty ignore list where it has none. They are written outside the scanned directory, because gitleaks loads `.gitleaksignore` from the root of what it scans whatever `--gitleaks-ignore-path` says. The change's own copies of those files are scanned as content under another name, inline `gitleaks:allow` comments are ignored (`--ignore-gitleaks-allow`), and so are `GITLEAKS_*` variables. A change that adds, edits or removes a policy file fails the scan even when it finds nothing (`verify.PolicyProblem`): the new policy was never checked, so a maintainer reviews it and merges it on its own, past the failed check.

## Required checks and staging

The main ruleset should require `Secret scan` and `Local verification` from GitHub Actions on every pull request, and nothing else from CI. A pull request can add a workflow of its own whose job takes a required check's name; requiring the `local-ci` status as well closes that, because only someone with write access can post it, and `make pr-merge`, which runs on the merger's machine, makes the same checks itself. It need not require a head to be up to date with its base: `make pr-merge` refuses one that is behind (GitHub's compare), so the merged tree is the verified one. A repository whose Actions cannot run (billing, or turned off) requires the `local-ci` status itself instead ([below](#when-actions-cannot-run)).

**Staging deploys only what `make ci-local` attested.** Railway's GitHub auto-deploy is off (`repo: ""` in `deploy/platform.yaml`): `Deploy staging` runs on every push to `main`, and its plan job (`cilocal attest <sha>`) lets the rollout start only when either:

- the pushed commit itself has a successful `local-ci` status (`make ci-local COMMIT=<sha> RAN="…"` in a clean worktree of it posts it), or
- the pull request whose squash made it (by GitHub's commit-to-PR index, or its `(#NNN)` squash title) passed local verification on the merged head.

A direct push, or the squash of an unattested head, deploys nothing, and the hourly `Staging catch-up` keeps refusing it until it is attested. Production takes only a release staging already ran ([RELEASES.md](RELEASES.md#what-is-gated)). **Only a verified tree merges**: `make pr-merge` refuses a head behind its base, because GitHub would squash it into a tree nobody built (the base's newer commits plus the PR). Two green PRs broke a downstream `main` that way: one removed a function the other still called. Rebase onto the base, run what the change needs and attest again; a burst of PRs therefore lands one after another, each rebased onto the last. An unattested `main` is the top priority: `git worktree add --detach <dir> <sha>`, run what the commits since the last attested one need (`make ci` when there is none, as in a [new project](#local-verification)), then `make ci-local COMMIT=<sha> RAN="…"` there; then dispatch `Deploy staging` (or wait for the catch-up).

## Merging on local verification

Pull requests open ready and stack. Each merges the moment it is attested, conflict-free and green, with `make pr-merge PR="<number>..."` (`cilocal merge`; the skill [github-prs](../.claude/skills/github-prs/SKILL.md)), which squashes exactly the attested head with `--match-head-commit` and never waits. Per PR it decides (`verify.Decide`): merged is done; closed, draft (`gh pr ready <n>`), stacked on another branch (unless `ARGS=--any-base`), conflicting, behind its base, not attested, a failed check, a hollow green (`Local verification` passed but `Secret scan` skipped) or a pending one (including `Local verification` not reported yet) is skipped with its next step, and the others still merge. Only the newest run of each workflow, and the newest attempt of each job, counts. Before merging it retargets the open PRs stacked on the branch to `main` and prints the `git rebase --onto` for each. `ARGS=--dry-run` says what it would do.

- **Never wait** on another PR, its verification or a staging deploy. A head behind `main`, or one that conflicts, is refused with "rebase onto origin/main and re-attest": rebase, push, run what it needs, attest. `make pr-merge` reads the base once more right before the merge, so only GitHub's own instant is left for a race.
- **After the merge, check its deploy.** `make pr-merge` prints the squash commit and `make pr-merge ARGS="--check-deploy <sha>"`. Once `Deploy staging` has run for it, run that once: one read of that commit's `Deploy staging` runs and of `<STAGING_WEB_URL>/healthz` (200 with `data.status` `ok`; `STAGING_WEB_URL` from the env, else the repository variable). A failed or refused deploy is the top priority, fixed forward; one cancelled by a newer `main` commit is checked on main's head (`ARGS=--check-staging`); a dry run (`RAILWAY_DEPLOY` is not `true`) says nothing was deployed. It is a follow-up, not a gate: keep merging. `ARGS=--then-check-staging` follows main's deploy (bounded) after a burst. `Post-merge report` comments on the PR when its deploy fails.
- **Ruleset first.** `make pr-merge` calls `gh pr merge`, so the ruleset (required reviews, required checks) still applies on top of it. Never `--delete-branch`: it closes the PRs stacked on the branch.

### When Actions cannot run

On a repository whose GitHub Actions do not start (a spent or blocked billing budget, or Actions turned off), no workflow runs: not `CI`, and not `Deploy staging` or `Deploy production`. Then:

- `make pr-merge` merges on `local-ci` alone when the head has no CI job that ran, either no CI run at all or only jobs GitHub never started, and says so (`ARGS=--require-actions` refuses instead). A blocked account still gets runs: GitHub fails each job within seconds, with no runner and the annotation "The job was not started because recent account payments have failed or your spending limit needs to be increased". Those jobs are read as not run, never as a verdict. A job that did run and failed still refuses. The checks it makes on the attestation are the ones `Local verification` would have made, from the same code.
- Require the `local-ci` commit status in the ruleset (it is posted by `make ci-local`, not by Actions), not `Secret scan` or `Local verification`, which would wait forever.
- Nothing deploys on its own. Deploy by hand with `make railway-up PLATFORM_ENV=staging` from a clean checkout of `main` after `go run ./scripts/cilocal attest <sha>` passes, and to production only a release staging ran ([deploy.md](deploy.md)). Do not turn Railway's GitHub auto-deploy back on to work around it: that deploys unattested commits.
- `CI_RUNNER` pointed at a self-hosted runner brings every workflow back without hosted minutes (`RELEASE_RUNNER` overrides it for the release jobs).

## Watching CI

One read per question: `gh pr checks <n>` or `gh run view <id> --json jobs` after its expected completion, never a polling loop. Fetch logs only for a concrete failure. `Post-merge report` comments on the merged PR when a push workflow fails on `main`.
