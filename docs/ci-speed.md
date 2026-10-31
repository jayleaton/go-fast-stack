# CI speed

What makes the checks a change needs ([CI.md](CI.md#what-to-run)) slow or fast. The ideas come from Owen Gretzinger's "How I made our CI 20x faster": a faster lint, a pre-push hook, testing only what a change affects, a nightly full suite, test parallelism and caches. Each was tried in its Go form, measured, and kept only when the numbers held. Re-measure with `make bench-devloop` (tables per section) and the timings below.

## Where the time goes

Measured on 2026-10-31 at `ad7b6b1`. The Mac is a 14-core Apple laptop. The boxes are Davit VMs (4 or 2 vCPUs, 8 GiB) on a shared host. Box timings vary up to 2x between runs because other boxes share the host. Compare numbers within one table, not across tables.

**Cold** means `go clean -cache` and `golangci-lint cache clean` first. **Warm** means the same command run again.

| Command | Mac cold | Mac warm | 4 vCPU cold | 4 vCPU warm | 2 vCPU cold | 2 vCPU warm |
|---|--:|--:|--:|--:|--:|--:|
| `go build ./...` | 8.9s | 1.2s | 35.9s | 7.2s | 42.9s | 6.7s |
| `go vet ./...` | 2.0s | 0.3s | | | | |
| `golangci-lint run ./...` | 21.2s | 2.0s | | 1.7s | | |
| `make generate` | | | 154.8s | 17.3s | 144.4s | 15.1s |
| `make check-generated` | | | 15.2s | 18.0s | 5.3s | 15.9s |
| `make build` | | | 47.5s | 1.3s | 56.7s | 0.9s |
| `make lint` | | | 38.6s | 8.9s | 31.0s | 7.5s |
| `make test` (`-race`, Postgres, coverage) | | | 169.1s | 16.7s | 198.1s | 16.1s |

Heavy commands such as `make test` and `make ci` run in boxes, never on the Mac. That is why the Mac row covers only build, vet and lint.

The cold `make check-generated` row ran right after a cold `make generate`, so its caches were already warm.

**golangci-lint per linter** (`--verbose`, lint cache cleared, build cache warm, 4 vCPU):
- The whole run took 46s.
- Analyzer CPU time is spread across many cores. The top analyzers were `buildir` 66s (shared SSA for several linters), `goimports` 33s, `wastedassign` 29s, `unconvert` 11s, `buildssa` 10s and `printf` 9s.
- With the lint cache warm, the whole run takes 1.7s.

**`make check-fast` after a one-line edit to a service file** (11 affected packages, first run after the edit):

| Step | 4 vCPU | 2 vCPU |
|---|--:|--:|
| affected packages | 4.0s | 2.3s |
| generated code | 4.4s | 1.4s |
| `go vet` | 23.9s | 16.6s |
| golangci-lint | 11.1s | 5.6s |
| `go test` (no `-race`) | 21.6s | 10.2s |
| total | 68.2s | 37.3s |

**Tests:**
- `packages/auth` is the longest package: 7.2s (4 vCPU) and 9.3s (2 vCPU) with `-race`. The next is under 2.1s.
- Every top-level test is already parallel; `paralleltest` and `tparallel` enforce it.

## Adopted

### Caches

The biggest cost is a cold cache. A box made from the plain project image starts with no Go or lint cache. [.davit/setup.sh](../.davit/setup.sh) builds the image instead. It downloads the modules, builds everything, compiles the race test binaries, runs golangci-lint once and builds the generators. The image takes about 7 minutes to build, once.

A new box's first runs (4 vCPU):

| First run in a new box | Plain image | Warmed image |
|---|--:|--:|
| `go build ./...` | 43.5s | 16.0s |
| `make lint` | 88.6s | 16.1s |
| `make generate` | 91.3s | 12.8s |
| `make test` | 125.6s | 64.7s |
| total | 349.0s | 109.6s |

**Why the cache carries over:**
- The box works in the same directory as the image builder (`/home/davit/work`), so the cached entries match.
- `GOCACHE` keys are content hashes. A box whose tree has moved on still reuses every package that did not change.

**Other caching options:**
- `make build` already uses `-trimpath`, which keeps its cache entries independent of the checkout path.
- Persisting caches between boxes (a shared volume) was not needed: the image does it.

### `go vet` folded into golangci-lint

golangci-lint's `govet` (part of `default: standard`) runs the same `go vet` analyzers. To prove no rule was lost, I made a probe file with one violation each of `printf`, `copylocks` (2), `errorsas`, `lostcancel`, `unmarshal`, `assign`, `timeformat`, `bools`, `unusedresult` and `unreachable`. `go vet` and `golangci-lint run --enable-only govet` flagged the same 11 lines, with no difference either way.

`make check-fast` now runs golangci-lint alone, after an edit, 3 rounds alternating:

| | 4 vCPU | 2 vCPU |
|---|--:|--:|
| golangci-lint alone | 5.7s, 6.2s | 7.5s, 8.5s |
| `go vet` then golangci-lint | 6.3s, 6.1s | 8.0s, 12.2s |

The first round of each was a cold lint cache (92s and 153s) and is left out.

### Generators in parallel

`go generate ./...` runs the `mockgen` directives one at a time. `make generate` and `make check-fast` now run one `go generate` per package, in parallel with as many jobs as there are CPUs, next to sqlc and templ.

| `go generate` | Serial | Parallel |
|---|--:|--:|
| 4 vCPU | 7.1s, 5.5s | 2.8s, 2.7s |
| 2 vCPU | 6.2s, 3.6s | 3.4s, 3.2s |

### Affected tests, with a guard

[scripts/affected.sh](../scripts/affected.sh) already mapped changed files to packages and their reverse dependencies (`go list -test -deps`). It runs everything for:
- `go.mod`
- tooling
- lint config
- migrations
- the sqlc config

**Added:**
- sqlc queries and sqlc's generated code also run everything. Their effect reaches the database tests through the schema, not only through imports.
- A guard fails the selection when it does not match the diff:
  - a changed Go file outside `deploy/` that maps to no package;
  - a changed package that is missing from the selection;
  - Go changes with an empty selection.

On sample diffs:
- one service file selects its 11 importers;
- a sqlc query runs everything;
- a deleted package with no importers selects nothing and passes;
- a Go file outside every package fails with exit 3.

`make check-pr` is now the default answer to "what to run" ([CI.md](CI.md#what-to-run)). It runs what the diff needs (lint and `-race` tests on the affected packages, `check-generated`, `deploy-check`, `actionlint`, `release-smoke`, or `make ci` for a full run), then prints the `make ci-local RAN="…"` line to attest with.

### Pre-push hook

`make hooks` already installed a pre-push hook that runs `make check-fast`. With `go vet` gone and the generators in parallel, here are the same one-line service edits (11 affected packages, 4 vCPU):

| `make check-fast` | Before | After |
|---|--:|--:|
| first edit | 68.2s (vet 23.9s) | 12.6s |
| next edits (test cache warm for unchanged results) | | 6.7s, 6.6s |

The after steps were: affected packages 0.9s, generated code 0.6s, golangci-lint 4.0s, namelint 0.4s, `go test` 0.6s.

The hook and the docs no longer say how to skip it.

**No lint rule lost.** I seeded one service file with a `printf` misuse, a declaration that should be merged, a magic number and an unused function. Full `golangci-lint run ./...` and `make check-fast` reported the same four findings: `govet`, `staticcheck` S1021, `mnd` and `unused`. check-fast also printed the `printf` finding a second time, from `go test`'s built-in vet.

### Nightly full suite

`make nightly` (scripts/nightly.sh) checks out `origin/main` detached in `.cache/nightly`. It runs:
- `make ci`;
- a flake hunt: `go test -race -count=5 -shuffle=on ./...` against Postgres;
- `make secret-scan ARGS=--all`.

On success, it attests the commit with `make ci-local COMMIT=<sha>` when nothing has attested it yet. On failure, it opens a "Nightly failed on main" issue, or comments on the open one, and exits non-zero. It does not post a failed `local-ci`, which would be final for the commit, so a flake cannot block its staging deploy.

It runs from cron on the dev box, the machine that hosts the `CI_RUNNER` runner and has `gh` and Docker. That needs no Actions, which matters because sparkrack's workflows are disabled while billing blocks them:

    0 3 * * * cd ~/src/go-fast-stack && make nightly >> ~/.cache/nightly.log 2>&1

Today's run: `-race -count=5 -shuffle=on` passed with no failure (31.1s at 4 vCPU, 62.6s at 2 vCPU). `NIGHTLY_REPORT=0 make nightly` on this stack's head passed in 382s at 4 vCPU (a new box, plain image).

## Rejected

- **`--new-from-rev` / `--whole-files` for golangci-lint.** With the lint cache warm, the whole run takes 1.7s; the cost is the cold cache, which the image fixes. Diff-only lint also misses a finding in an unchanged file caused by a changed one (an unused function whose last caller was deleted). `make check-fast` already limits lint to the affected packages.
- **Dropping or merging slow linters.** `buildir` and `buildssa` are shared by several linters, so dropping one saves little. `goimports`, `wastedassign` and `unconvert` each enforce a rule nothing else does. Warm, the whole run takes 1.7s.
- **Tuning `-p` / `-parallel`.**

  | `go test -race -count=1 ./...` | `-p 1` | `-p 2` | `-p 4` | `-p 8` | `-parallel 32` |
  |---|--:|--:|--:|--:|--:|
  | 4 vCPU | 16.5s | 11.5s | 8.9s | 8.9s | 9.2s |
  | 2 vCPU | 20.4s | 14.9s | 13.3s | 13.1s | 13.4s |

  The default (`-p` = CPUs) is already at the floor, and `packages/auth` bounds the run.
- **More `t.Parallel()`.** Every top-level test already has it (`paralleltest`).
- **"Shared packages" forcing a full run.** The reverse-dependency closure already includes every package that imports a shared one. A full run would add only packages that cannot be affected.
- **A GitHub workflow for the nightly run.** Cron needs no Actions minutes or runner registration and works where Actions is off. AGENTS.md also asks for make targets instead of new jobs.
