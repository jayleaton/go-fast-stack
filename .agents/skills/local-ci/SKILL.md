---
name: local-ci
description: Every CI check in this repo runs locally; GitHub Actions is never what you wait on. Use before any commit, push or PR, and whenever asked to "run CI" or check that a change passes.
---

# Local CI

Every check runs on your machine (or a Davit box). GitHub only checks that you attested them.

1. **Before every push: `make check-fast`.** The pre-push hook runs it (`make hooks`). Fix what it finds; never skip the hook. A change adds no lint or format findings, warnings included.
2. **Run what the change needs: `make check-pr`.** It picks the checks from the diff, stops at the first failure, and prints the `make ci-local RAN="…"` line. `ARGS=--dry-run` only lists them. A user-visible UI change also gets one pass in a real browser against a running `make dev`.
3. **Attest the pushed head: `make ci-local RAN="…"`.** Commit, push, then run it in the clean worktree. It runs nothing: it posts `local-ci` on the exact SHA and fills the PR's "Local verification" section. Every push or rebase needs a new attestation; no PR yet, open it and attest again. Never edit that section by hand.
4. **A failed head is final.** Never attest a red head. `make ci-local FAILED="<what>"` marks it failed for good. Fix forward with a new commit: in this PR if this branch caused it, otherwise in its own PR off `main`.
5. **Never rerun remotely what you ran locally.** No GitHub Actions run, no `gh workflow run` and no "re-run checks" as verification. The only remote checks are the attestation and the secret scan, and they take seconds.
6. **Busy machine: `make box-ci COMMIT=<sha>`.** The one exception: when a heavy check reports every slot busy, or for a long suite, it runs check-generated, lint, build and test on that commit in a Davit box, instead of locally, never in addition. Attest the `PASS` line it prints. With no Docker, `make ci-pg` runs `make ci` on a throwaway Postgres.

**An unattested `main` commit:** `git worktree add --detach <dir> <sha>`, run what the commits since the last attested one need (`make ci` when there is none), then `make ci-local COMMIT=<sha> RAN="…"` there.

Tests only at the test boundaries (`AGENTS.md`, "Test boundaries").

Opening, stacking and merging PRs: the [github-prs](../github-prs/SKILL.md) skill.
