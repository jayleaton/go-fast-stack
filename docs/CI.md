# CI

Every check runs in `make ci-local`, on the agent's machine, before a pull request merges ([Local verification](#local-verification)). GitHub runs no verification on `main`, and on a pull request only two small jobs, once `make ci-local` has filled its body. Actions minutes are billed per job, rounded up to the whole minute, and a PR's checks now cost about two of them.

## What runs

`CI` (`.github/workflows/ci.yml`) runs on `pull_request` edits alone. `make ci-local` posts the `local-ci` commit status and then fills the PR body, and that edit starts the run that checks both; a push or a new PR starts nothing. Until `make ci-local` runs on a head, the required checks show as waiting ("Expected"), never red: red means a real problem (a failed `local-ci`, a section edited by hand, or one for another head). It has to be the run that does not start, because a job skipped by `if` counts as passed. Its two jobs are the two checks a ruleset requires:

- `Secret scan`: the PR's changed files with gitleaks ([below](#secret-scan)). `make ci-local` runs it as well; this one does not depend on the agent's machine.
- `Local verification`: the `local-ci` status and the PR body's section on the head (`cilocal check`).

A newer edit cancels the PR's older run (`ci-pr-<n>`). Both jobs run on the `CI_RUNNER` repository variable, a JSON `runs-on` value (`'["self-hosted","linux"]'`), or GitHub-hosted `ubuntu-latest` without it. They build `scripts/cilocal`, which imports only the standard library, so they download no modules.

A push to `main` runs only `Deploy staging` (`deploy-staging.yml`), which deploys a commit `make ci-local` attested to Railway staging ([Required checks and staging](#required-checks-and-staging)); production runs releases ([RELEASES.md](RELEASES.md)). `Bench dev loop` still runs on `bench/**` pushes and by hand.

`make ci-local` selects from the diff against the merge-base with the PR's base branch (`origin/main`, [scripts/branches.sh](../scripts/branches.sh)), in `scripts/cilocal/verify` (`Classify`, `Plan`):

| Checklist item | Steps | Selected when |
|---|---|---|
| secrets | `cilocal secret-scan --base <merge-base>` (gitleaks) | always |
| build | `go build <affected packages>` | Go inputs changed and `scripts/affected.sh` maps them to packages |
| lint | `make lint LINT_DIRS=<affected dirs>` (golangci-lint, names, the test-boundary check); `make lint-layering` on a full run; `make actionlint`; `scripts/check-test-boundaries.sh` alone | the same; the probes on a full run; actionlint when `.github/workflows/` or `.github/actions/` changed; the boundary check alone whenever `make lint` is not selected, so a test file a `deploy/`-only or docs change adds is still refused |
| generated | `make check-generated` | affected.sh reports sqlc, templ or `go:generate` inputs or outputs changed |
| tests | `make test TEST_PKGS=<affected packages>` (`-race`, its own throwaway Postgres, time budgets, the tested packages' coverage floors); `make lint-tests` and `check-coverage.sh --exist` on a full run | Go inputs map to packages |
| migrations | `scripts/check-migration-order.sh` against the tip of `origin/main` | a migration (`*/migrations/*.sql`) or the check changed |
| smoke | `make release-smoke` | affected.sh's `smoke`: a Dockerfile, or a package one builds |
| deploy | `make deploy-check`: vet, golangci-lint and names in the `deploy/` module, then `make deploy-vars` | `deploy/`, `.env.example`, `.golangci.yml`, `tools/` or `scripts/namelint/` |
| mobile | `pnpm install --frozen-lockfile`, `tsc`, `expo lint`, `expo install --check`, `pnpm test` in `apps/mobile-app` | `apps/mobile-app/` or the route table its test reads (`apps/web/internal/config/routes.go`) |

Go inputs are every file except `docs/`, root Markdown, `apps/mobile-app/`, `deploy/` (its own checks), `.github/`, `.gitignore`, `.dockerignore` and `.gitleaks.toml`. A docs-only change runs only the secret scan and the test-boundary check. `scripts/cilocal/`, the `Makefile` and `mise.toml` decide how every step runs, so they select every step but the mobile app's, with affected.sh's full run (`AFFECTED_FULL=1`); so do an empty diff and 3,000 changed files or more. affected.sh's own full-run inputs (go.mod, tools, scripts, lint config, migrations, sqlc config) make the Go steps cover `./...`. `make ci-local ARGS=--plan` prints the selection and every command.

`make ci` still runs every check on the checkout as it is, and `make check-fast` stays the seconds-long pre-push check; neither posts anything.

## Local verification

`make ci-local` (`scripts/cilocal`) runs every step the branch selects and records that it passed. The rules are in `scripts/cilocal/verify`, a test boundary (AGENTS.md "Test boundaries"); `scripts/cilocal` wires them to git, gh, make and Davit.

- **The exact head, clean.** Only a clean checkout whose HEAD is the pushed branch head is attested (`Head.Refusal`): commit and push first. The steps run in a fresh `git worktree` of that commit under the git directory (`.git/cilocal/wt-<sha>`), with their own compose project (`cilocal-<sha>`), and the variables of the caller's shell that would point a step at another database are dropped. If a step changes the checkout's files, the run is refused. The worktree and its containers are removed afterwards.
- **Sharing the machine.** At most `CI_LOCAL_SLOTS` (default 1) full runs at once on a machine, across every worktree, agent and repository built from this template; another waits, says so once a minute and starts when the slot frees, never failing for it. A slot is an flock in the user cache directory (`cilocal/slots`), so a run that dies frees it. An `--only` iteration takes no slot.
- **Memory before the steps.** With the slot taken, a run on Linux waits until `MemAvailable` is at least `CI_LOCAL_MIN_AVAILABLE_GIB` (default 4) and swap use is not climbing (256 MiB between two samples ten seconds apart), saying why, and never fails for it (`MemoryPressure`). A failed head is final, so a starved machine must delay a run, not fail it. macOS has no `/proc/meminfo` and does not wait.
- **Attesting.** It writes the report (head, tree, merge-base, areas, each step with exit code and duration, Go and Docker versions, where it ran) to `.git/cilocal/<sha>.json`, posts the `local-ci` commit status (`success` or `failure`) on that exact SHA, then fills the PR body's "Local verification" section (the template is [.github/pull_request_template.md](../.github/pull_request_template.md)). The status description carries a digest of the section, so a section edited afterwards no longer matches. When the body already holds the section (`--publish` again), it re-runs the failed jobs of the PR's newest CI run instead.
- **Reuse across rebases.** Each step's entry carries a hash of its inputs: the blobs of every file whose change alone would select that step (`InputOf`, the same rule as the selection), its command and environment, and where it ran (`local darwin/arm64`, `davit linux`). The section ends with the run's ledger of passed steps and hashes, and the status carries its digest. A later run of the same PR reuses, rather than runs, each step whose hash a ledger in the PR body holds, when that earlier head's `local-ci` succeeded with that ledger and never failed; the section names the step as reused, with that head and hash, and the check verifies all of it against that head's statuses (`ReuseProblems`). The secret scan and the migration order always run. A rebase over `main` commits that touched none of a step's inputs reruns only those two; a failed head lends nothing.
- **A failed step.** It prints the failing tests and files it recognises (Go tests and packages, compiler, vet and golangci-lint lines: `Failures`) and one line of triage: whether this branch changed those files. A failure in what the branch changed is fixed in the PR. Any other is fixed forward straight away in its own PR off `main`, with a regression test that fails before the fix; the branch then rebases on it and runs again. A failed head is never re-run until green: `make ci-local` refuses to run on or attest a head that already has a failed `local-ci`, and the `Local verification` job and `make pr-merge` refuse a PR whose head has one.
- **Enforced on every pull request.** The `Local verification` job and `make pr-merge` refuse a PR unless `local-ci` is `success` on its exact head and the body's section names that head, has no failed `local-ci` in its history, ticks every item (`Items`, which the template lists) and matches the digest the status carries (`Verification.Problems`). Each refusal says to run `make ci-local`. A new push or rebase leaves the new head without a status, so it needs a new run.
- **Trust.** This is a signoff, not an attestation: the status says that someone with write access ran the commands and they passed. Nothing on GitHub runs them again, so a run that passed wrongly (a machine that differs, or a status posted by hand) shows up only after merge.
- **Iterating.** `make ci-local ARGS="--only tests,deploy"` (items or jobs) runs a subset and posts nothing; `--keep-going` runs every step after a failure; `--plan` prints what would run; `--publish` posts the saved report of the current head again, for a PR opened after the run.

### Davit boxes

Where the `davit` CLI is set up, a run whose machine slot is taken runs its steps in a fresh Davit box of its own instead of waiting (`--davit` forces it, `--local` waits for the slot; `scripts/cilocal/davit.go`). So does a run on a machine whose `bash` is older than 4 (macOS ships 3.2; `scripts/affected.sh`, the release smoke and the probes need 4): the box also answers `scripts/affected.sh` for the selection, from the tree and the changed files. The diff, the head checks, reuse, the secret scan, the migration order (both read git history and origin's refs) and posting stay on this machine. Davit syncs the clean worktree of the head into the box `<repo>/cilocal-<sha>` (`CI_LOCAL_DAVIT_MEMORY`, default 4 GiB), `cilocal exec` there removes the files the head does not have (Davit's sync never deletes, so a box made from an image built at an older commit keeps files the head removed), then refuses to run anything unless the box's files are exactly the head's tree (`TreeDigest`), the steps are the same, the report names `davit:<repo>/cilocal-<sha>`, and the box is removed afterwards. A box's steps hash as `davit linux`, so they are reused only by another box run.

## Secret scan

`cilocal secret-scan` copies the files a change touches (the branch's diff since the merge-base in `make ci-local`; the pull request's file list in the `Secret scan` job, or every tracked file when GitHub cannot list them) and scans them with gitleaks 8.30.1, downloaded once into the user cache directory and checked against its release SHA-256. A finding fails the step or job, printing the rule, file and line with the value redacted.

[.gitleaks.toml](../.gitleaks.toml) keeps gitleaks' own rules and allows only the paths that hold credential-shaped fixtures on purpose: Go tests and `testdata`, `.env.example` and the bench script's synthetic secrets. Over the whole tree it reports nothing (`make secret-scan ARGS=--all`). A new deliberate fixture goes under a test or testdata path, or into that list; a real secret is removed and rotated, because history keeps it. `make secret-scan` scans what your branch changed.

## Required checks and staging

The main ruleset should require `Secret scan` and `Local verification` from GitHub Actions on every pull request, and nothing else from CI (the old single check, `ci-ok`, is gone). It need not require a head to be up to date with its base, and neither does `make pr-merge`: only a conflict stops it. A repository whose Actions cannot run (billing, or turned off) requires the `local-ci` status itself instead ([below](#when-actions-cannot-run)).

**Staging deploys only what `make ci-local` attested.** Railway's GitHub auto-deploy is off (`repo: ""` in `deploy/platform.yaml`): `Deploy staging` runs on every push to `main`, and its plan job (`cilocal attest <sha>`) lets the rollout start only when either:

- the pushed commit itself has a successful `local-ci` status (`make ci-local ARGS="--commit <sha>"` in a clean worktree of it posts it), or
- the pull request whose merge made it (by GitHub's commit-to-PR index, or its `(#NNN)` squash title) passed local verification on the merged head.

A direct push, or the merge of an unattested head, deploys nothing, and the hourly `Staging catch-up` keeps refusing it until it is attested. Production takes only a release staging already ran ([RELEASES.md](RELEASES.md#what-is-gated)). **The merged tree of two verified PRs is not itself verified**: a head behind its base squashes into a tree nobody ran; a clash surfaces after the deploy and is fixed forward. An unattested `main` is the top priority: `git worktree add --detach <dir> <sha>`, then `make ci-local ARGS="--commit <sha>"` there, which selects against the last commit the gate accepts (`SelectionBase`, at most 50 back) and posts `local-ci` on it; then re-run `Deploy staging` (or wait for the catch-up).

## Merging on local verification

A pull request merges once `make ci-local` passed on its exact head, GitHub can merge it without conflicts, and its own checks are green: `make pr-merge PR="<number>..."` (`cilocal merge`; `ARGS=--dry-run` says what it would do). It refuses a PR whose local verification does not hold for its head, a draft, a conflicting PR, and one whose newest CI run has a job that is still running or did not pass. It never waits: each refused PR is reported with the next step, and the others still merge. Before merging it retargets the open PRs stacked on the branch to its base, so the stack survives, then squash-merges with `--match-head-commit`.

- **Behind is fine; a conflict is not.** In a burst the second PR merges behind the first. Only a conflict needs a rebase, a push and `make ci-local` again (unchanged steps are reused).
- **Ruleset first.** `make pr-merge` calls `gh pr merge`, so the ruleset (required reviews, required checks) still applies on top of it.

### When Actions cannot run

On a repository whose GitHub Actions do not start (a spent or blocked billing budget, or Actions turned off), no workflow runs: not `CI`, and not `Deploy staging` or `Deploy production`. Then:

- `make pr-merge` merges on `local-ci` alone when the head has no CI run at all, or only jobs GitHub never started, and says so (`--require-actions` refuses instead). A blocked account still gets runs: GitHub fails each job within seconds, with no runner and the annotation "The job was not started because recent account payments have failed or your spending limit needs to be increased". Those jobs are read as not run, never as a verdict. A job that did run and failed still refuses. The checks it makes are the ones `Local verification` would have made, from the same code.
- Require the `local-ci` commit status in the ruleset (it is posted by `make ci-local`, not by Actions), not `Secret scan` or `Local verification`, which would wait forever.
- Nothing deploys on its own. Deploy by hand with `make railway-up PLATFORM_ENV=staging` from a clean checkout of `main` after `go run ./scripts/cilocal attest <sha>` passes, and to production only a release staging ran ([deploy.md](deploy.md)). Do not turn Railway's GitHub auto-deploy back on to work around it: that deploys unattested commits.
- `CI_RUNNER` pointed at a self-hosted runner brings every workflow back without hosted minutes (`RELEASE_RUNNER` overrides it for the release jobs).
