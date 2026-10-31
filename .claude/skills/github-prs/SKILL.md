---
name: github-prs
description: How every agent verifies and attests locally (`make ci-local RAN="…"`), opens, stacks and merges pull requests in this repo, fast. Use before any `git commit`, `gh pr create`, `gh pr merge`, retargeting or rebasing a stacked PR, or re-running a check, and whenever you are about to wait on CI, write a merge script, or merge PRs "one at a time".
---

# Pull requests: run what you need, attest, stack, merge at once

Run the checks your change needs (lint, tests, build), then attest with `make ci-local RAN="…"` and merge right away. `make ci-local` runs nothing: it tells the PR you checked, and what. GitHub runs no verification on `main`, and on a PR only two small jobs that check the attestation. Stack PRs and merge each the moment it is attested; after the merge, check its staging deploy.

The rules on what may merge are in `AGENTS.md` ("Verifying", "Pull requests", "Environments: staging is always current"); the detail is in `docs/CI.md` ("What runs", "What to run", "Local verification", "Merging on local verification"). This skill is how you do it.

## 1. Before every commit

- **Never skip the hooks.** `make hooks` installs the pre-push hook (`make check-fast`); when it fails, fix what it found and push again; never skip it.
- **Default: `make check-pr`.** It runs what the branch's changes need (the table is `docs/CI.md`, "What to run"), fails when the affected-package selection does not match the diff, and prints the `make ci-local RAN="…"` line to attest with. By hand: `make lint LINT_DIRS=<dirs>` and `make test TEST_PKGS=<pkgs>` for the Go packages it can affect (`scripts/affected.sh` prints them; `make check-fast` is the seconds-long pass), `make check-generated` for sqlc, templ or mock inputs, `make check-migrations` for a migration, `make deploy-check` for `deploy/`, `make actionlint` for workflows, `make release-smoke` for a Dockerfile or what it builds, `make ci` for a change to the tooling. Tests only at the test boundaries (`AGENTS.md`, "Test boundaries"); never a test in `deploy/` or the UI.
- A change adds no lint or format findings, warnings included.
- Add the paths you changed; never `git add -A` in an old worktree.

## 2. Attest: `make ci-local RAN="…"`

After **every** push or rebase, once you have run what the change needs:

```bash
git push                                     # pushed head, clean worktree
make check-pr                                # runs what the change needs, prints the RAN line
make ci-local RAN="make lint LINT_DIRS='./apps/web/internal/services ...'; make test TEST_PKGS='...'; make secret-scan"
```

- It runs nothing and takes seconds. It refuses a dirty worktree, a detached HEAD (except `COMMIT=<sha>` of a `main` commit), a head that is not the pushed branch head, or a head that already has a failed `local-ci`.
- It posts `local-ci` on that exact SHA and fills the PR body's **Local verification** section (head, who, and your statement). **Never edit the section by hand:** the status carries its digest.
- **One attestation, one commit.** Any push, amend or rebase needs a new one. No PR yet? Open it (section 3) and run it again.
- **Something failed?** Fix it before attesting. If you attest a failure (`make ci-local FAILED="<what>"`), that head is final: fix forward with a new commit. Code this PR touched: fix it here. Anything else: its own PR off `main` with a test that fails before the fix, then rebase. Add the prevention.
- Say exactly what ran; a reviewer reads the scope from it.

## 3. Open and stack

- `gh pr create --base <base>` with `.github/pull_request_template.md`, open and ready, no labels: problem, fix, and the Local verification section. Conventional commit title; the body ends with the attribution line.
- **Base per layer.** Needs nothing unmerged: base `main`. Needs an open PR: base that PR's branch. Independent work gets its own PR on `main`.
- One concern per PR; push follow-ups to the same branch. **Never close and reopen a PR.**

## 4. What PR CI and the merge script refuse

PR CI runs only `Secret scan` and `Local verification`, the two checks the main ruleset requires, on the `CI_RUNNER` runner. It runs only on a PR body edit, and until `make ci-local` claims the head (posts `local-ci` or fills the section for it) the check runs as `Awaiting make ci-local`, so the required one shows **Expected**, not red; red is a real problem. `Local verification` and `make pr-merge` share one rule (`scripts/cilocal/verify`, `LocalVerificationProblems`):

| Refused because | Fix |
|---|---|
| no `local-ci` status on the head (never attested, or a new push) | run what it needs, `make ci-local RAN="…"` |
| `local-ci` is `failure`, or failed on this head before | fix forward with a new commit, push, attest |
| no Local verification section, it names another head or no "Ran:" | `make ci-local RAN="…"` |
| the section is not the attested one (edited by hand) | `make ci-local RAN="…"` again restores it |
| `Secret scan` found a credential | remove it (rotate it if real), push, attest |
| it conflicts with `main` | rebase onto `main`, push, attest |

`make ci-local` posts the status before it edits the body, and that edit starts the PR run, so you never re-run `Local verification` by hand; when the body was already current, `make ci-local` re-runs the run itself.

## 5. Merge

```bash
make pr-merge PR="<PR> [<PR> ...]"     # merges every green PR now, reports the rest
make pr-merge PR=<PR> ARGS=--dry-run   # says what it would do
```

- **Green** means `local-ci` and the section attest the exact head, GitHub can merge it without conflicts, and the checks passed. It squash-merges with `--match-head-commit <head>`, so only the attested head lands. A head behind `main` is refused ("rebase onto origin/main and re-attest"): the merged tree must be the one you verified. Where Actions cannot run (no CI job ran, or GitHub never started them), it merges on `local-ci` alone and says so.
- Anything else (draft, unattested, pending, failing, conflicting, stacked) is reported with its next step; the script never waits. A head behind `main` or a conflict needs rebase, push, run what it needs, attest; after each merge in a burst, rebase the next PR.
- **After each merge, check its deploy** (a follow-up, never a gate; merge the next PR now): it prints `make pr-merge ARGS="--check-deploy <squash sha>"`. Run it once "Deploy staging" has run for that commit. Failed or refused: top priority, fix forward. Cancelled by a newer main deploy: `make pr-merge ARGS=--check-staging` checks main's head. A dry run (`RAILWAY_DEPLOY` not `true`) deployed nothing. After a burst, `ARGS=--then-check-staging` follows main's deploy (bounded).
- **Stacked children** are retargeted to `main` before their parent merges. Then `git rebase --onto origin/main <old parent head> <child>`, `git push --force-with-lease`, attest.
- Integration branches (`feat/<x>`): `make pr-merge PR=<PR> ARGS=--any-base`.
- Never `gh pr merge --delete-branch`: it closes every PR stacked on the branch and deletes any worktree holding it. Never write your own merge or queue script; change `scripts/cilocal` (rules and tests in `scripts/cilocal/verify`).
- **Admin bypass** only when the developer authorized it and a GitHub check is red for a reason outside the code, and only after `make ci-local RAN="…"` attested the head: `gh pr merge <n> --squash --admin --match-head-commit <sha>`, then a PR comment "Authorized by <who>, merged by <agent>" with the attestation.

## 6. Checks that fail outside the code

- **Lost runner** ("lost communication with the server", "received a shutdown signal", "The operation was canceled.", a timeout with no log): `gh run rerun <run id> --failed`, once, by hand. Nothing re-runs it automatically.
- **Actions blocked** (billing or a spending limit: "The job was not started because…"): those jobs count as not run; `make pr-merge` merges on `local-ci` alone (`docs/CI.md`, "When Actions cannot run").
- A real failure belongs to the PR's owner; the rest of the burst doesn't wait for it.
- One read per question (`gh pr checks <n>`, `gh run view <id> --json jobs`), never a polling loop.

## 7. Staging deploys itself

`main` runs no CI. "Deploy staging" deploys a push to Railway staging only when `make ci-local` attested it (`cilocal attest`): the squash of an attested PR head, or a commit with its own `local-ci`. `make pr-merge` refuses a head behind `main`, so every squash is a tree someone verified. The hourly "Staging catch-up" deploys `main` whenever staging is behind and `main` is attested. Never wait for a deploy or for another PR to settle: merge each PR when its own head is attested and conflict-free, and stack the next at once; check its deploy afterwards (section 5); "Post-merge report" comments on the PR when a push workflow on its commit fails. A failed deploy is fixed forward in parallel and blocks no unrelated merge.

An unattested `main` is the top priority: both workflows fail and say why. Fix it forward: `git worktree add --detach <dir> <sha>`, run what the commits since the last attested one need (`make ci` when there is none, as in a new project's first attestation), then `make ci-local COMMIT=<sha> RAN="…"` there. It refuses a dirty tree, a commit off `origin/main` or one that already failed, and posts `local-ci` on the commit; then dispatch "Deploy staging".

## 8. Waiting without losing the work

- **Never end a turn while your own wait, watcher or helper still runs.** Register it (`t3_background_wait`) or wait for it.
- **Never `pgrep -f`, `pkill -f` or kill a process found by name**; wait on a PID captured at spawn or the background task's ID.
- A background command from a T3 tool call dies with that tool's scope and has a two-hour limit. A merge never needs one.

## Rules and what enforces them

| Rule | Enforced by |
|---|---|
| Every PR is attested on its exact head | `Local verification` (`ci.yml`) and `verify.Decide`, through `verify.LocalVerificationProblems`; a skipped `Secret scan` is never green |
| The section is the script's, never hand-edited | the section digest in the `local-ci` status |
| Merge only the verified head, never a conflict | `--match-head-commit` and the conflict refusal in `make pr-merge` |
| Staging deploys only an attested PR head or commit | `cilocal attest` in Deploy staging's plan, `verify.Attestation` and its test |
| Merge every green PR at once; stacks survive | `make pr-merge` (no wait path, retargets children), `scripts/cilocal/verify/merge_test.go` |
| A push workflow that fails on main is reported | `Post-merge report`; a verify test fails when a push-to-main workflow is missing from its list |
| Never skip the pre-push hook, no reopen, no `pgrep -f` | this skill only; add a check if an agent repeats one |

## Done means

- Every PR you were asked to land is merged, or reported with its blocker and next step.
- Every PR you pushed to is attested on its current head, and each merge's staging deploy was checked.
- No PR was closed and reopened, and no hooks were skipped.
