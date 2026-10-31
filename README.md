# Go Fast Stack

<p align="center"><img src="docs/cover.png" alt="Go Fast Stack" width="100%"></p>

<p align="center"><a href="https://x.com/jayleaton"><img src="https://img.shields.io/badge/Follow-%40jayleaton-000000?logo=x&logoColor=white" alt="Follow @jayleaton"></a> <a href="https://buymeacoffee.com/jayleaton"><img src="https://img.shields.io/badge/Buy_Me_a_Coffee-Support-FFDD00?logo=buymeacoffee&logoColor=black" alt="Buy Me a Coffee: Support"></a></p>

A starter template for web apps with a mobile companion. One small Go program serves the website, the JSON API for the mobile app, and sign-in, all backed by Postgres. It is designed to be fast to run, cheap to host, and safe for AI coding agents to work on.

Go Fast Stack succeeds [jayleaton/ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo), the Next.js version of the same patterns. This repo was previously named `ai-patterns-go`.

**What you get:** email and password sign-in, account pages, an Expo mobile app, an optional admin dashboard (with two-factor sign-in), nightly encrypted backups, and a one-command deploy to Railway.

## How it fits together

```
browser ─── HTML ──▶ UI pages and server actions
                          │ API client (in-process)
                          ▼
mobile app ─ JSON ──▶ JSON API ──▶ services ──▶ database layer ──▶ Postgres
                     (access,       (business     (sqlc queries,
                      validation)    rules)        migrations)
```

- **One program, two front doors, one gate.** The website's pages and form actions call the JSON API in-process, the same endpoints the mobile app calls over HTTP, so access rules and validation live in one place. Nothing calls its own API over the network, and the browser never calls the API at all.
- **The browser only sees HTML.** It never gets JSON, tokens or internal errors ([security](docs/security.md)).
- **Each layer talks only to the one below it.** Lint enforces this (see [Built for AI agents](#built-for-ai-agents)).
- **Few moving parts.** Postgres is the only service it needs. Email goes through Resend in production, and secrets come from env vars or Doppler.

## Performance

Pages are rendered on the server in Go, and [Datastar](https://data-star.dev) adds interactivity by swapping in HTML from the server. There is no JavaScript bundler and no client framework; CSS comes from the Tailwind standalone CLI.

| | Measured |
|---|---|
| Web server | one static binary, **24.5 MB** (no runtime, no `node_modules`) |
| Client JavaScript | one file, Datastar: **34 KB** (13 KB gzipped) |
| Memory | **83 MB** right after start, 93 MB after 200 page loads, on a dev machine. On Railway the idle server uses about 75 MB. |
| Home page | 17 KB of HTML, rendered in about 0.4 ms |
| Production image | distroless, about 11 MB compressed (0.9 MB base + the binary) |

7.6 MB of the binary is the pure-Go image codecs used by the optional image uploads ([docs/storage.md](docs/storage.md#pure-go-static-binary)). The binary and image sizes come from a production build (`make build`: `-tags nodynamic -trimpath -ldflags='-s -w'`, Go 1.26.8, linux/amd64); the memory and page numbers come from the same kind of build before the codecs were added. The Railway figure is from the [deploy cost notes](docs/deploy.md#cost).

**Fast verification and fast tests.** The agent checks only the packages a change can affect, on its own machine, and `make ci-local` attests what it ran on the PR in seconds ([docs/CI.md](docs/CI.md)). The CI times below were measured on the GitHub job that ran the same checks before they moved there.

| | Time |
|---|---|
| CI for a docs-only PR | 14 s ([#86](https://github.com/jayleaton/go-fast-stack/pull/86)) |
| CI for a one-package change | **33 s**, down from 65 s ([#86](https://github.com/jayleaton/go-fast-stack/pull/86)) |
| CI for every package (warm cache) | 54 s ([#86](https://github.com/jayleaton/go-fast-stack/pull/86)) |
| Full test suite with the race detector, 2-vCPU runner | 29 s median ([#74](https://github.com/jayleaton/go-fast-stack/pull/74)) |
| `make check-fast` (local, before every push) | under 1 s for docs, under 10 s for a full warm run |

Every test runs in parallel, and none may sleep. Lint enforces both, and `make test` fails any package that takes over 60 s or any single test over 10 s.

## Cost

**Hosting.** Railway bills the memory and CPU a service actually uses: about $10 per GB of memory per month. The Go server and Postgres each idle at under 100 MB, so a typical setup costs less than the $5 that Railway's Hobby plan includes:

| Setup | Services | Idle compute per month |
|---|---|---:|
| production only | web + Postgres | ≈ $1.55 |
| staging + production | 4 | ≈ $3.10 |
| staging + production + admin dashboard | 8 | ≈ $6.00 |

These figures come from idle usage measured on Railway on 2026-10-22. Traffic, egress and backups come on top. Nightly backups of a template-sized database cost fractions of a cent. Details: [docs/deploy.md](docs/deploy.md#cost) and [docs/backups.md](docs/backups.md).

**CI minutes.** GitHub bills every job rounded up to a whole minute. The checks now run on the agent's machine before `make ci-local` attests them, so a PR costs two small jobs (`Secret scan` and `Local verification`, about a minute each) and a merge to `main` the staging deploy (`Deploy staging`: [docs/RELEASES.md](docs/RELEASES.md)). Before that, one job per PR:

| | Before | Now |
|---|---:|---:|
| Billed minutes for a docs-only PR | 9 | **1** |
| Projected minutes per 30 days at 200 PRs/day | ~84,000 (~$672) | **~18,500–22,100 (~$148–177)** |

Sources: [#54](https://github.com/jayleaton/go-fast-stack/pull/54) and [#67](https://github.com/jayleaton/go-fast-stack/pull/67). Those PRs measured real Actions runs and projected the month at $0.008 per minute.

## Built for AI agents

Agents do their best work when the rules are checked by machines, not by review. This template makes the right path the easy one and the wrong path fail fast:

- **Strict layers, enforced by lint.** Database → services → transports (JSON API and HTML pages). A handler that reaches for the database, or a service that imports HTTP, fails `make lint` (depguard). The agent can't route around the architecture, and `make lint-layering` proves the rules still catch planted violations.
- **Tests only where the business logic is.** Tests are allowed only in services, route handlers, the auth package and the rules of local verification. A script rejects a test file anywhere else, and each of those places has a coverage floor. Agents don't spend time writing tests for UI or wiring.
- **Generated mocks.** Mocks are generated from the interfaces (`make generate`), never written by hand. `make check-generated` fails when one is stale.
- **A pre-push check that takes seconds.** `make check-fast` runs format, vet, lint and tests on only what the change affects. Agents run it on every push, even on machines where a full run is too heavy, and `make ci-local` attests each pushed PR head before it merges.
- **Written instructions.** [AGENTS.md](AGENTS.md) holds the architecture, test rules and PR rules, and [CONTEXT.md](CONTEXT.md) holds the shared vocabulary. Agents and humans read the same rules and use the same words.
- **Parallel agents never collide.** Each clone or git worktree gets its own database and ports, derived from its path. Any number of agents can run `make db-up` and `make test` at once.
- **Errors that name the fix.** Lint messages say what to do instead ("transports call services; only packages/database talks to Postgres"). The server refuses to boot with bad config and lists every problem. `make railway-doctor` prints the fix for each missing deploy prerequisite.

**Coming soon** (in progress, not merged yet):
- **One place for route access.** A central route handler will declare each route's access (public, signed in, later roles) in one place, so no handler hand-rolls its own session check (`feat/route-access`).
- **Names that explain themselves.** Lint will ban single-letter names and unnamed magic numbers (`chore/naming-lint`).

## Quick start

Needs Go and Docker. [mise](https://mise.jdx.dev) installs the pinned Go version.

```sh
mise install    # the Go version pinned in mise.toml
make db-up      # Postgres for this checkout (creates .env)
make migrate
make dev        # http://<this-machine>:3000
```

Then open the site and sign up. In development, verification and reset links are printed in the server log instead of emailed. To try the API with `curl`, see [docs/development.md](docs/development.md#try-the-api).

A new project made from this template attests its first `main` commit once: `make ci`, then `make ci-local RAN="make ci"` on `main` ([docs/CI.md](docs/CI.md#local-verification)). After that every pull request is attested on its head.

To deploy, run `make railway-setup`. It creates the whole Railway project from [`deploy/platform.yaml`](deploy/platform.yaml) and checks that it is healthy ([docs/deploy.md](docs/deploy.md)).

## Learn more

- [Development](docs/development.md): the API, every `make` command, theming, the stack
- [Deploy to Railway](docs/deploy.md): environments, previews, cost detail, how it works
- [Secrets](docs/secrets.md): Doppler, live rotation, what to do after a provider incident
- [Backups and restore](docs/backups.md): nightly encrypted backups and the restore runbook
- [Security](docs/security.md): what the browser can and can't reach, headers, cookies
- [Google and Apple sign-in (optional)](docs/sign-in.md): provider credentials, turning it on, deleting it
- [Admin dashboard (optional)](docs/admin.md): user management and the audit log
- [Object storage (optional)](docs/storage.md): S3/R2/Railway buckets, safe image uploads and imports by URL
- [Mobile app](apps/mobile-app/README.md) · [Auth](packages/auth/README.md) · [UI kit](packages/uikit/README.md)
- [AGENTS.md](AGENTS.md) (rules for contributors and agents) · [CONTEXT.md](CONTEXT.md) (vocabulary)

## License

[MIT](LICENSE). Use it for anything, including commercial projects; keep the copyright notice.
