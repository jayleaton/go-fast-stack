> **Rules.** Before merge, run what this change needs on its exact head (docs/CI.md, "What to run"): for Go, `make lint test` scoped with `LINT_DIRS`/`TEST_PKGS` (`scripts/affected.sh` prints them) or `make check-fast`, plus the checks of what you touched. Then `make ci-local RAN="<what you ran>"` posts `local-ci` and fills "Local verification" with that statement and the head SHA. Never edit that section by hand. The `Local verification` check, `make pr-merge` and the staging gate refuse the PR while it is missing, names another head, or differs from what `local-ci` attested. A new push or rebase needs a new attestation. A reported failure (`FAILED=`) is final for that head: fix it in a new commit (docs/CI.md, "Local verification").

## Problem

<!-- One or two sentences. -->

## Fix

<!-- How you fixed it. UI changes: before/after images. -->

## Local verification

<!-- local-verification:start -->

Not attested yet: run what the change needs, then `make ci-local RAN="<what you ran>"` on the pushed head.

<!-- local-verification:end -->
