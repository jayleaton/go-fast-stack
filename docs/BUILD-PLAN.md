# Build plan (v0) — shared contract for parallel work

Successor to `jayleaton/ai-patterns-monorepo` (reference clone: `/home/sireggy/Documents/Projects/ai-patterns-monorepo`).
Same architecture patterns, same feature set, rebuilt in Go for simpler maintenance, CI speed and cheap hosting.

## Decisions (JJ, 2026-10-22)

- Language: Go (toolchain pinned in `mise.toml`, currently 1.26.8). **One Go module** at the repo root: `github.com/jayleaton/ai-patterns-go`.
- UI: server-rendered `templ` + **Datastar** (SSE-first; core library only, no Datastar Pro features) + Tailwind via the **standalone CLI** (no Node in the web toolchain).
- Auth: **Go-native**, its own package `packages/auth`, modelled on Better Auth (table names, endpoint paths, flows) so Better Auth features can be ported later.
- Database: Postgres 18, `pgx/v5`, `sqlc` for typed queries, `goose` migrations embedded in the binary.
- Mobile: the existing Expo app stays for now and talks to the **JSON API** with bearer tokens (native Swift/Kotlin later).
- i18n: English + Japanese, messages ported from the original `apps/web-app/messages/*.json`.
- Hosting: Railway, single static binary in a distroless image, migrations as the Railway pre-deploy command.
- CI: fast by default (see "CI" below). JJ runs up to ~200 PRs/day; CI time is the bottleneck.

## Layout and ownership (one owner per path to avoid merge conflicts)

| Path | Owner branch | Contents |
|---|---|---|
| root (`Makefile`, `.golangci.yml`, `.github/`, `Dockerfile`, `railway.json`, `docker-compose.yml`, `AGENTS.md`, `CLAUDE.md`, `CONTEXT.md`, `README.md`) | `feat/foundation` | tooling, CI, deploy, agent docs |
| `apps/web/cmd/server`, `apps/web/internal/{server,config,api,services,validate}` | `feat/foundation` | HTTP server, routes config, envelope, route wrapper, services, JSON API v1 |
| `packages/database/**` | `feat/foundation` | pgx pool, app migrations, sqlc queries, repositories |
| `packages/auth/**` | `feat/auth` | auth package, its own migrations + queries + tests |
| `apps/web/ui/**` | `feat/web-ui` | templ pages/components, Datastar handlers, i18n, static assets, Tailwind input |
| `apps/mobile-app/**` | later step (`feat/mobile`) | Expo app ported to the Go API |

`go.mod`/`go.sum` are shared: add only what you need; conflicts there are resolved by `go mod tidy` at merge time.

## Layering (same as the original, enforced by `depguard` in golangci-lint + tests)

`packages/database` (schema → sqlc → repository) → `apps/web/internal/services` → transports (`apps/web/internal/api` JSON handlers, `apps/web/ui` HTML/Datastar handlers).

- Both transports call **services in-process**. The original's "server action → HTTP → own API" hop is intentionally removed.
- Transports never import `pgx`, `sqlc` output or repositories. Services never import `net/http` transport packages or templ.
- JSON responses always use the envelope `{"data": T|null, "error": string|null}`.
- Input validation lives in `apps/web/internal/validate` (CRUD names, e.g. `UpdateUserSettings`), never inline.
- No hardcoded origins (`localhost`); origins come from config (`PUBLIC_URL`, `INTERNAL_URL`, allowed hosts). Dev server binds `0.0.0.0:3000`.

## Auth contract (implemented by `feat/auth`; others code against it, use a fake until merged)

Package `github.com/jayleaton/ai-patterns-go/packages/auth`:

```go
type Config struct {
    Secret            string        // AUTH_SECRET
    BaseURL           string        // public origin used in email links
    RequireEmailVerification bool
    SessionTTL        time.Duration // 7 days
    SessionUpdateAge  time.Duration // 1 day
    TrustedOrigins    []string      // incl. mobile scheme betterstack://
}
type Mailer interface {
    SendVerificationEmail(ctx context.Context, to, url string) error
    SendPasswordResetEmail(ctx context.Context, to, url string) error
}
func Migrate(ctx context.Context, pool *pgxpool.Pool) error   // goose, version table "goose_auth"
func New(cfg Config, pool *pgxpool.Pool, mailer Mailer) (*Auth, error)

func (a *Auth) Middleware(next http.Handler) http.Handler     // loads session from cookie or "Authorization: Bearer <token>"
func (a *Auth) RequireUser(next http.Handler) http.Handler    // 401 envelope (JSON) or redirect (HTML, when Accept: text/html)
func (a *Auth) Handler() http.Handler                         // JSON endpoints, mounted at /api/auth/
func UserFrom(ctx context.Context) (User, Session, bool)

// Service methods for the HTML transport (same rules as the JSON endpoints):
func (a *Auth) SignUpEmail(ctx, SignUpInput) (User, Session, error)
func (a *Auth) SignInEmail(ctx, SignInInput) (User, Session, error)
func (a *Auth) SignOut(ctx, sessionToken string) error
func (a *Auth) RequestPasswordReset(ctx, email, redirectTo string) error
func (a *Auth) ResetPassword(ctx, token, newPassword string) error
func (a *Auth) VerifyEmail(ctx, token string) (User, Session, error)
func (a *Auth) SetSessionCookie(w http.ResponseWriter, s Session)
func (a *Auth) ClearSessionCookie(w http.ResponseWriter)
```

Tables (Better Auth names and columns): `user`, `session`, `account`, `verification`. `packages/auth` owns them.
`packages/database` may add app tables with FKs to `"user"(id)` and may read/update non-credential user columns (`name`, `image`).
App migrations run after auth migrations (version table `goose_app`).

JSON endpoints mirror Better Auth paths: `POST /api/auth/sign-up/email`, `POST /api/auth/sign-in/email`, `POST /api/auth/sign-out`,
`GET /api/auth/get-session`, `GET /api/auth/verify-email?token=`, `POST /api/auth/request-password-reset`, `POST /api/auth/reset-password`.

## Feature parity checklist (from the original)

Pages: home, `/why`, login, signup, forgot-password, reset-password, verify-email (+ success), dashboard (user settings, delete account), template welcome (feature-flagged), language switcher.
API: `/api/core/v1/users/me` (GET, PATCH, DELETE) plus `/me/email`, `/me/password`, `/me/sessions[/{id}|/others]`, `/me/data`; `PATCH /api/core/v1/users/{id}` (403 when not self). The pre-account `GET/PATCH/DELETE /api/core/v1/users` routes were replaced by `/users/me`.
Email: Resend (verification, password reset); dev mode logs links instead of sending.
Feature toggles: email verification off in dev, on in production.
Every page has a loading state (Datastar indicator), all text translated in every locale.

## CI (default for the new project)

- One workflow, `concurrency` cancel-in-progress per ref, path filters (Go vs mobile vs docs), Go module + build cache, `golangci-lint` with cache.
- Checks: `go build ./...`, `templ generate` + `sqlc generate` diff check, `golangci-lint run`, `go test -race ./...` against a Postgres service container, mobile checks only when `apps/mobile-app/**` changes.
- Merge queue ready (`merge_group` trigger). Target: warm PR run under 90 s.
<!-- scratch: CI billing shape -->
