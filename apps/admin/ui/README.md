# apps/admin/ui — admin HTML transport

Server-rendered [templ](https://templ.guide) pages, [Datastar](https://data-star.dev) 1.x actions over SSE, Tailwind v4.
A self-contained copy of `apps/web/ui`'s patterns (forms, i18n and asset pinning); it imports nothing from
`apps/web`, `packages/auth`, `packages/database*` or pgx. Both apps share their look through `packages/uikit`
(components and design tokens): pages use its semantic utilities (`bg-surface`, `text-muted-foreground`, ...), never
palette classes or `dark:` variants, so re-theming is `packages/uikit/theme.css` alone (the TOTP QR code keeps a
white background on purpose).

| Package | What |
|---|---|
| `ui` | `ui.New(deps)` (`Routes(mux)` / `Handler()`), the ports (`Auth` in `deps.go`; `Users`, `Admins`, `Audit`, `Analytics` in `services.go`), handlers, pages |
| `ui/view` | layout: `uikit.Page` + `uikit.AppShell` with the app name, section nav, signed-in admin, sign out, theme and language; pages build the rest from `packages/uikit` (`AuthLayout`, `PageHeader`, `Card`, fields, `Alert`, `Badge`, `TableWrap`, `Pagination`, `ConfirmForm`, ...) |
| `ui/i18n` | `messages/{en,ja}.json`, locale negotiation, `i18n.T(ctx, key)` and `i18n.Tf(ctx, key, args...)` |
| `ui/routes` | the UI's own action endpoints (logout, locale, 2FA enable, user actions, invite, admin disable/enable/remove, static); pages use `config.AppRoutes` |
| `ui/static` | embedded `app.css` (generated), pinned `datastar-1.0.4.js`, favicon |

## Commands

Run from the repo root. Outputs are committed; `make generate css` does all of it.

```sh
go tool templ generate -path apps/admin/ui
"$(apps/web/ui/scripts/tailwind.sh)" -i apps/admin/ui/styles/input.css -o apps/admin/ui/static/app.css --minify  # after templ
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
- **Admin management**: each row of the Admins page shows its state (active, invite pending, disabled) and the
  actions another admin may take: disable (active or pending), enable (disabled), remove (any). The signed-in
  admin's row shows a "you" badge and no actions; the service still refuses self and last-active-admin changes
  (`ErrAdminSelf`, `ErrLastAdmin`). Remove asks first in an accessible `<dialog>` (`uikit.ConfirmForm`): with JS
  the trigger opens it and its confirm button posts through Datastar; without JS the trigger posts the form
  directly. Banning a user and revoking all their sessions ask the same way. Actions patch `#admins-table` and
  `#admins-status`.
- **TOTP QR code**: the 2FA setup page (both the signed-out `SetupRequired` step and signed-in enrolment) draws
  the otpauth URI as an inline SVG on the server (`qr.go`, `github.com/boombuler/barcode/qr`): one `<path>` of the
  dark modules with a 4-module quiet zone on a white background, so it scans in dark mode. No QR service and no
  image URL ever sees the secret, and it is never logged. Anything that is not an `otpauth://` URI (or fails to
  encode) gets no QR code; the manual key and the "open in app" link stay.
- **Loading states**: `data-indicator:_x` on every post form drives the button label and `disabled`; the GET filter
  forms (users, audit) flip a `_searching`/`_filtering` signal on submit.
- **Lists**: the query string drives search, filters, sort and paging (`validate.DecodeQuery`), so they work without JS.
  Tables sit in `uikit.TableWrap` (they scroll inside it on small screens); an empty list shows `uikit.EmptyState`.
- **Auth state**: pages without a session 303 to login; actions redirect over SSE. A service `ErrTwoFactorRequired`
  sends the admin to `/two-factor/setup`. Signed-in pages are `Cache-Control: no-store`.
- **Audit log**: `actionKeys` in `helpers.go` labels every `services.Action*` (table and filter); unknown actions
  show raw. `auditDetails` translates the sign-in method and the 2FA failure count. The Status column
  (`auditStatus`) badges each entry: done, failed (with its reason, translated when it is the message of an
  `errorKeys` error, otherwise as recorded), outcome unknown (`pending`: the change may or may not have been
  applied; the reconciler resolves it) and unknown (the reconciler could not tell). Reconciled entries get a
  "reconciled" marker, and their reason is translated by status, never shown as the reconciler's English.
- **Errors**: one table (`errorKeys` in `ui.go`) maps `ui.Err*` and `services.Err*` to messages; validation errors
  show `errors.invalidInput`; anything else is logged and shows `errors.somethingWentWrong`. Mutations fail
  closed: `services.ErrAuditUnavailable` (the audit intent could not be written, nothing changed) shows
  `errors.auditUnavailable` and is logged with its cause; a change applied but whose outcome was not recorded
  is an internal error (something went wrong; the entry stays "outcome unknown").
- **i18n**: like the web copy, except that messages may carry ordered placeholders (`{0}`), filled by `Tf`, because
  word order differs (`Invite sent to {0}` / `{0} に招待を送信しました`). Keep them aligned across locales.
- **Tests**: none; the UI is not a test boundary (AGENTS.md "Test boundaries"). Its rules live in the services.

## Audit log: two ways of recording

- **Admin changes** (the services' `mutate`) fail closed: a pending row before the change, the outcome after. No row, no change (`ErrAuditUnavailable`, translated alert). A pending row whose outcome was never written shows as "Outcome unknown" until the reconciler resolves it.
- **Authentication events** (sign-in, sign-out, refused 2FA code) come from packages/auth's event hook (`server.NewAuditEvents`) after the fact, on auth's event worker. They are best-effort by design: they never block or undo a sign-in, a failed write is logged at error level, and auth drops events (with a counted warning) if its queue is full. They have no status beyond "Done".

The page says this in one line under the title (`pages.audit.howRecorded`).
