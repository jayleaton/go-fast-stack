# apps/admin/ui — admin HTML transport

Server-rendered [templ](https://templ.guide) pages, [Datastar](https://data-star.dev) 1.x actions over SSE, Tailwind v4.
A self-contained copy of `apps/web/ui`'s patterns (same look, forms, i18n and asset pinning); it imports nothing from
`apps/web`, `packages/auth`, `packages/database*` or pgx.

| Package | What |
|---|---|
| `ui` | `ui.New(deps)` (`Routes(mux)` / `Handler()`), the ports (`Auth` in `deps.go`; `Users`, `Admins`, `Audit`, `Analytics` in `services.go`), handlers, pages |
| `ui/view` | layout (app name, section nav, signed-in admin, logout, theme, language), inputs/selects/badges/alerts/pagination |
| `ui/i18n` | `messages/{en,ja}.json`, locale negotiation, `i18n.T(ctx, key)` and `i18n.Tf(ctx, key, args...)` |
| `ui/routes` | the UI's own action endpoints (logout, locale, 2FA enable, user actions, invite, static); pages use `config.AppRoutes` |
| `ui/static` | embedded `app.css` (generated), pinned `datastar-1.0.4.js`, favicon |
| `ui/mocks` | mockgen output for the ports (tests only) |

## Commands

Run from the repo root. Outputs are committed; `make generate css` does all of it.

```sh
go tool templ generate -path apps/admin/ui
"$(apps/web/ui/scripts/tailwind.sh)" -i apps/admin/ui/styles/input.css -o apps/admin/ui/static/app.css --minify  # after templ
(cd apps/admin/ui && go generate ./...)   # mocks
go test -race ./apps/admin/ui/...
```

## Mounting

```go
h := ui.New(ui.Deps{
    Auth:      adminAuthAdapter, // adapts the admin *auth.Auth; translates auth errors to ui.Err*
    Users:     userService,      // *services.UserService, AdminService, AuditService, AnalyticsService as is
    Admins:    adminService,
    Audit:     auditService,
    Analytics: analyticsService,
    UserFrom:  adminFrom,        // required: the admin + session the auth middleware stored
    RequireTwoFactor: cfg.RequireTwoFactor,
    AppName:   "Admin",
    SecureCookies: cfg.IsProduction,
    Logger:    log,
}).Handler()
```

`SignIn` and `CompleteSetPassword` return a `*ui.TwoFactorChallenge` as the error when a second factor (or, with
`SetupRequired`, TOTP enrolment) is needed: the UI calls `SetTwoFactorCookie` and redirects to `/two-factor` or
`/two-factor/setup`. `VerifyTwoFactor` and `StartTwoFactorSetup` read that challenge from the request's cookie, so
the adapter must see the request (for example through the context its middleware sets).

## Patterns

- **Forms**: real `<form method="post">` plus `data-on:submit="@post(path, {contentType: 'form'})"`. Datastar
  requests get SSE (patched fragments by id, or a redirect); plain posts get the full page or a 303. Same handler:
  `u.patch` / `u.redirect`. User actions patch `#user-profile`, `#user-ban`, `#user-sessions` and `#user-status`
  from a fresh `GetUser`; the invite patches `#admins-table` and `#invite-card`.
- **Loading states**: `data-indicator:_x` on every post form drives the button label and `disabled`; the GET filter
  forms (users, audit) flip a `_searching`/`_filtering` signal on submit.
- **Lists**: the query string drives search, filters, sort and paging (`validate.DecodeQuery`), so they work without JS.
- **Auth state**: pages without a session 303 to login; actions redirect over SSE. A service `ErrTwoFactorRequired`
  sends the admin to `/two-factor/setup`. Signed-in pages are `Cache-Control: no-store`.
- **Errors**: one table (`errorKeys` in `ui.go`) maps `ui.Err*` and `services.Err*` to messages; validation errors
  show `errors.invalidInput`; anything else is logged and shows `errors.somethingWentWrong`.
- **i18n**: like the web copy, except that messages may carry ordered placeholders (`{0}`), filled by `Tf`, because
  word order differs (`Invite sent to {0}` / `{0} に招待を送信しました`). `TestPlaceholdersMatch` keeps them aligned.
- **Tests** (`apps/admin/ui/**` is a test boundary): generated mocks of the ports plus a mock session through
  `Deps.UserFrom`. `TestRenderStopsWhenClientHangsUp` fails the connection at every write of every page.
