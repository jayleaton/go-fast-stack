> **Rules.** This PR merges only after the checks its change selects (secret scan, build, lint, generated code, tests, migration order, release smoke, `deploy/`, mobile) ran and passed locally on its exact head. `make ci-local` runs them in a clean worktree of the pushed head and fills the "Local verification" checklist from its run report (command, exit code, duration, head SHA). Never tick a box or edit that section by hand. The `Local verification` check and `make pr-merge` refuse the PR while the section is missing, an item is unticked, it names another head than the PR's, or it differs from what the `local-ci` status attested. A new push or rebase needs a new run: commit, push, `make ci-local` (docs/CI.md, "Local verification"). A failed run is resolved by a new commit, never re-run until green: fix it here if this PR touched the failing code, otherwise fix it forward in its own PR off `main` with a regression test, then rebase.

## Problem

<!-- One or two sentences. -->

## Fix

<!-- How you fixed it. UI changes: before/after images. -->

## Local verification

<!-- local-verification:start -->

Not run yet: run `make ci-local` on the pushed head.

- [ ] **secrets**
- [ ] **build**
- [ ] **lint**
- [ ] **generated**
- [ ] **tests**
- [ ] **migrations**
- [ ] **smoke**
- [ ] **deploy**
- [ ] **mobile**

<!-- local-verification:end -->
