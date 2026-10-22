# CONTEXT.md: shared language

Use these words when talking about this repo, in code review, commits and agent output.

| Term | Means | Lives in |
|---|---|---|
| you | the agent reading this and changing the template | |
| we, maintainers | the people building this template | |
| developer | the person who cloned the template and directs agents | |
| user | a person signed in to the app (row in `"user"`) | |
| app | a product surface: the **web app** (`apps/web`) or the **mobile app** (`apps/mobile-app`) | `apps/` |
| package | shared Go code under `packages/` (`database`, `auth`) | `packages/` |
| migration | a goose SQL file; auth and app migrations are tracked separately (`goose_auth`, `goose_app`) | `packages/*/migrations/` |
| query | a sqlc-annotated SQL statement; `make generate` turns it into Go | `packages/database/queries/` |
| repository | data access over sqlc; returns `model` types and `model.ErrNotFound` | `packages/database/*.go` |
| model | plain data types repositories return; services re-export them | `packages/database/model/` |
| service | business rules; returns `*services.Error` with a `Kind` | `apps/web/internal/services/` |
| validator | an input type with `Validate()`, named by CRUD verb (`UpdateUserSettings`) | `apps/web/internal/validate/` |
| transport | code that speaks HTTP: the **JSON API** or the **UI** | `apps/web/internal/api/`, `apps/web/ui/` |
| envelope | every JSON response: `{"data": T\|null, "error": string\|null}` | `api.WriteData`, `api.WriteError` |
| route wrapper | `api.Public` / `api.Authenticated`: auth check, body limit, envelope, error mapping | `apps/web/internal/api/route.go` |
| caller | the signed-in user + session for a request, read from context (`auth.UserFrom`) | `api.Caller` |
| actor | the caller as a service sees it (`services.Actor`) | `apps/web/internal/services/` |
| routes config | `AppRoutes` (pages), `APIRoutes` (endpoints) | `apps/web/internal/config/routes.go` |
| feature toggles | `config.Features`: email verification, template welcome, Expo dev origins | `apps/web/internal/config/features.go` |
| session | a row in `"session"`; the signed `better-auth.session_token` cookie (web) or `Authorization: Bearer <token>` (mobile) points at it | `packages/auth/` |
| dev mailer | logs auth email links instead of sending them (no `RESEND_API_KEY`) | `packages/auth/mailer.go` |
| sandbox database | the Docker Postgres from `make db-up`; the only database you may break | `docker-compose.yml` |
| Datastar | the SSE-first hypermedia library the UI uses (core only, no Pro) | `apps/web/ui/` |
