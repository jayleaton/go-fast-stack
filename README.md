# Go Fast Stack

<p align="center"><a href="https://x.com/jayleaton"><img src="https://img.shields.io/badge/Follow-%40jayleaton-000000?logo=x&logoColor=white" alt="Follow @jayleaton"></a> <a href="https://buymeacoffee.com/jayleaton"><img src="https://img.shields.io/badge/Buy_Me_a_Coffee-Support-FFDD00?logo=buymeacoffee&logoColor=black" alt="Buy Me a Coffee: Support"></a></p>

A starter template for web apps with a mobile companion. One small Go program
serves the website, the JSON API for the mobile app and sign-in, all backed by
Postgres. It is built to be fast to run, cheap to host and safe for AI coding
agents to work on. It succeeds
[jayleaton/ai-patterns-monorepo](https://github.com/jayleaton/ai-patterns-monorepo),
the Next.js version of the same patterns.

`make help` lists every command.

## What you get

| Area | What it does |
|---|---|
| Sign-in | Email and password, verification, reset, sessions, two-factor; optional Google and Apple sign-in (`packages/auth`, Better Auth's tables and endpoints in Go) |
| Web app | Server-rendered account pages and a dashboard; the pages call the JSON API in-process, so access and validation live in one place |
| JSON API | `/api/core/v1` for the mobile app and servers; the browser never calls it |
| Admin (optional) | User management with two-factor sign-in and an append-only audit log, on its own database |
| Storage (optional) | S3/R2/Railway buckets, safe image uploads and imports by URL |
| Operations | Live secret rotation (env or Doppler), nightly encrypted backups, one-command deploy to Railway |

## The apps

| Path | What it is |
|---|---|
| [`apps/web`](apps/web) | The web app: pages, the JSON API, auth at `/api/auth/` (`server serve`, `migrate`) |
| [`apps/admin`](apps/admin) | The optional admin dashboard, a separate binary, database and auth instance (`admin serve`, `migrate`, `invite`) |
| [`apps/backup`](apps/backup) | The nightly encrypted Postgres backup and restore |
| [`apps/mobile-app`](apps/mobile-app) | The Expo app, on the same JSON API with bearer tokens |
| [`packages/*`](packages) | Shared Go: `database` (sqlc, migrations), `auth`, `uikit`, `secrets`, `storage`, `backup` |

## Stack

- **Go** (version pinned in `go.mod` and `mise.toml`), one module, static binaries: the web server is about 25 MB and idles at about 75 MB on Railway.
- **templ + [Datastar](https://data-star.dev)** for server-rendered pages and SSE interactivity; no client framework and no JS bundler (34 KB of client JavaScript). CSS from the Tailwind standalone CLI.
- **Postgres 18** through `pgx`, `sqlc` and `goose`.
- **Railway** hosting, distroless images; see [deploy/platform.yaml](deploy/platform.yaml).

## Quick start

Needs Go and Docker. [mise](https://mise.jdx.dev) installs the pinned Go;
without it, `make` and the pre-push hook set `GOTOOLCHAIN` to the version in
`go.mod`. Every `make` command creates `.env` from `.env.example` on first use.

```sh
make db-up migrate dev
```

| | |
|---|---|
| Web | <http://localhost:3000> (binds `0.0.0.0`, so LAN and Tailscale work too) |
| Admin | `make admin-migrate admin-dev` → <http://localhost:3001> |
| Postgres | `127.0.0.1:<a per-checkout port>`; `make env` prints it, and pgweb next to it |

Development has email verification off and logs every email link to the
server log instead of sending it. Ports are derived from the checkout path, so
any number of clones and worktrees can run `make db-up` and `make test` at
once.

A new project made from this template attests its first `main` commit once:
`make ci`, then `make ci-local RAN="make ci"` on `main`.

## Checks

| Command | What it does |
|---|---|
| `make check-fast` | Before every push: format, vet, lint and plain tests for only what the change affects (seconds) |
| `make generate check-generated lint test build` | Regenerate sqlc/mocks/templ, prove the output is committed, lint, race tests with time budgets and coverage floors, build the server |
| `make ci` | Everything, on this checkout as it is: the above plus the layering probes and `make vulncheck` |
| `make release-smoke` | Build every Dockerfile's binary, migrate, boot web and admin in production mode |
| `make check-pr`, `make ci-local` | What a branch's change needs, and the attestation that records it ([local-ci skill](.claude/skills/local-ci/SKILL.md)) |

Tests live only at the boundaries described in [AGENTS.md](AGENTS.md#test-boundaries-mandatory).
Lint enforces the layers, and `make lint-layering` proves the rules still
catch planted violations.

## Deploy

`make railway-setup` creates the whole Railway project from
[deploy/platform.yaml](deploy/platform.yaml) and checks that it is healthy;
`make railway-doctor` prints the fix for each missing prerequisite. Secrets
are in [packages/secrets](packages/secrets/doc.go); the release and rollback
rules are in [docs/RELEASES.md](docs/RELEASES.md).

## Learn more

- [Releases](docs/RELEASES.md) · [Local CI](.claude/skills/local-ci/SKILL.md) · [Pull requests](.claude/skills/github-prs/SKILL.md)
- [Mobile app](apps/mobile-app/README.md) · [Auth](packages/auth/README.md) · [UI kit](packages/uikit/README.md)
- [AGENTS.md](AGENTS.md) (rules for agents and contributors) · [CONTEXT.md](CONTEXT.md) (shared vocabulary)

## License

[MIT](LICENSE). Use it for anything, including commercial projects; keep the copyright notice.
