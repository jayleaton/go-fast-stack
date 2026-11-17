---
name: github-prs
description: How every agent opens, stacks and merges pull requests in this repo, after verifying with the local-ci skill. Use before any `gh pr create`, `gh pr merge`, retargeting or rebasing a stacked PR, and whenever you are about to wait on CI, write a merge script, or merge PRs "one at a time".
---

# Pull requests

Verify and attest first: the [local-ci](../local-ci/SKILL.md) skill.

## Open

- `gh pr create --base <base>` from `.github/pull_request_template.md`, ready (never draft), no labels.
- Title: a conventional commit in plain language (`fix(api): PATCH /users no longer drops the image`). Body: the problem in a sentence or two, then the fix; UI changes need before/after images. The body ends with the attribution line.
- One concern per PR; push follow-ups to the same branch. Never close and reopen a PR.

## Stack

- Needs nothing unmerged: base `main`. Needs an open PR: base that PR's branch.
- `make pr-merge` retargets children to `main` before their parent merges. Then `git rebase --onto origin/main <old parent head> <child>`, `git push --force-with-lease`, attest.

## Merge

```bash
make pr-merge PR="<PR> [<PR> ...]"     # merges every attested, green PR now, reports the rest
make pr-merge PR=<PR> ARGS=--dry-run   # says what it would do
make land PR=<PR>                      # rebase, make ci-pg, attest and merge inside one hold of the CI lock
```

- Merge only with `make pr-merge` (or `make land`). Never `gh pr merge` (`--delete-branch` closes every PR stacked on the branch) and never your own merge or queue script.
- It never waits: anything not mergeable is reported with its next step. Merge each PR the moment it is attested; never wait for another PR or a deploy.
- Integration branches (`feat/<x>`): `ARGS=--any-base`.
- Admin bypass only when the developer authorized it, after `make ci-local` attested the head: `gh pr merge <n> --squash --admin --match-head-commit <sha>`, then comment "Authorized by <who>, merged by <agent>".

## After the merge

- Once "Deploy staging" has run, check it once with the command `make pr-merge` prints: `make pr-merge ARGS="--check-deploy <squash sha>"`. One read, never a polling loop.
- **A failed staging deploy, or an unattested `main` staging refuses, is the top priority.** Fix it forward before merging anything else: attest `main` (local-ci, "An unattested `main` commit"), then `gh workflow run deploy-staging.yml --ref main`.
