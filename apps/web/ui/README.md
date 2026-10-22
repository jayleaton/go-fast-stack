# apps/web/ui — HTML transport

Server-rendered [templ](https://templ.guide) pages, [Datastar](https://data-star.dev) 1.x actions over SSE, Tailwind v4.
Ported from the original `apps/web-app` (same pages, same Tailwind classes, same messages).

| Package | What |
|---|---|
| `ui` | `ui.New(deps)` (`Routes(mux)` / `Handler()`), the `Auth`/`Users` interfaces, handlers, auth, dashboard and Account pages |
| `ui/view` | layout shell (header: logout, theme, language), shared inputs/buttons/alerts/skeletons |
| `ui/welcome` | template welcome landing + `/why` (feature-flagged, deletable; see its README) |
| `ui/i18n` | `messages/{en,ja}.json`, locale negotiation, `i18n.T(ctx, key)` |
| `ui/routes` | the UI's own action endpoints (logout, locale, live stream, settings, delete, static); pages use `config.AppRoutes` |
| `ui/static` | embedded `app.css` (generated), pinned `datastar-1.0.4.js`, favicon |
| `ui/mocks` | mockgen mocks of `ui.Auth` and `ui.Users` (`go generate`, committed, CI diff-checked) |
| `ui/cmd/preview` | all pages on `0.0.0.0:3001` over an in-memory backend (`backend.go`) with the auth and service rules |

## Commands (for the Makefile)

Run from the repo root. Both outputs are committed; CI regenerates them and fails on a diff.

```sh
# templ -> *_templ.go (templ is a go.mod tool directive)
go tool templ generate -path apps/web/ui

# Tailwind standalone CLI v4.3.3, downloaded once into apps/web/ui/bin/ (gitignored), SHA-256 verified
TAILWIND="$(apps/web/ui/scripts/tailwind.sh)"
"$TAILWIND" -i apps/web/ui/styles/input.css -o apps/web/ui/static/app.css --minify

# order matters: templ first, then CSS (Tailwind scans the .templ and .go files for classes)

# preview every page over the in-memory backend (demo@example.com / password123; email links are logged)
go run ./apps/web/ui/cmd/preview            # dev behavior: no email verification
go run ./apps/web/ui/cmd/preview -verify    # production behavior

go test -race ./apps/web/ui/...
```

Watch mode while editing: `go tool templ generate -path apps/web/ui -watch` and `"$TAILWIND" ... --watch`.

## Mounting (server wiring)

```go
ui := ui.New(ui.Deps{
    Auth:          authService,        // *auth.Auth satisfies ui.Auth as is
    Users:         userService,        // *services.UserService satisfies ui.Users as is
    Features:      cfg.Features,
    SecureCookies: cfg.IsProduction,
    Logger:        log,
    // UserFrom defaults to auth.UserFrom (the session auth.Middleware loaded).
})
deps.UI = ui.Handler() // server.Deps.UI, mounted at "/" inside auth.Middleware
```

`Routes(mux)` registers `GET /{$}`, the pages in `config.AppRoutes`, the UI actions in `ui/routes`, `GET /static/` and
`GET /favicon.ico`; `Handler()` returns a mux with them. POST actions are wrapped in `http.CrossOriginProtection`
(stdlib); keep that if a global CSRF layer is added. Compile-time assertions in `deps.go` keep both interfaces in step
with `packages/auth` and the user service.

`ui.Auth` uses the auth types directly (`ui.User` = `auth.User`, ...) and the handlers map `auth.Err*` values to
messages (`AuthErrors`); unknown errors show "Something went wrong" and are logged. Sign-up and sign-in pass
`CallbackURL: config.AppRoutes.VerifyEmailSuccess`; password reset passes `config.AppRoutes.ResetPassword` as
`redirectTo`. Auth joins app paths with its `BaseURL`, so no origin is hardcoded here. The verify link may land after
auth's own `GET /api/auth/verify-email` already set the cookie (the page then checks the session) or carry `?token=`
(the page POSTs it to `VerifyEmail`, so link scanners doing GETs cannot spend it).

`ui.Users` is the method set of `*services.UserService`: profile, email change, password, sessions, delete and data
summary. The Account page (`config.AppRoutes.Account`) is built only on it; the existing auth flows (sign-in, sign-up,
verify, reset, sign-out) keep calling `*auth.Auth`. Forms decode into the `validate` types (`UpdateUserSettings`,
`UpdateUserEmail`, `UpdateUserPassword`) and call `Validate()` before the service. Errors are translated in
`account_errors.go`: the named service errors (`services.ErrInvalidPassword`, `ErrSessionNotFresh`, ...) first, then
the kind sentinels (`services.ErrNotFound`, ...), then validator fields; anything else is logged and shown as
"Something went wrong". The actor carries the request's session ID, so the page can mark "this device" and revoke
the others. Revocations are pushed to open dashboard tabs, and a revoked tab goes to the login page.

## Tests

Handler tests (`*_test.go` in this directory, the UI's test boundary) run the real router with mockgen mocks of
`ui.Auth` and `ui.Users` and a mock session (`Deps.UserFrom`); they never import `packages/auth` or the database. Auth
errors come from `ui.AuthErrors`. Each Account action is tested anonymously (redirect), on its happy path, and with
every service error it can show. `guards_test.go` holds the two non-handler guards (every template key exists in every
locale; the Datastar bundle matches its pin) and `render_test.go` renders every page and state in en and ja.

## Patterns

- **Forms**: real `<form method="post" action=...>` plus `data-on:submit="@post(path, {contentType: 'form'})"`.
  Datastar requests (`Datastar-Request: true`) get SSE: patched fragments by id (`#form-error`, `#settings-status`,
  ...) or a redirect. Plain posts (no JS) get the full page with the error, or a 303. Same handler, `u.patch`/`u.redirect`.
- **Loading states**: `data-indicator:_loading` on the form drives the button label and `disabled`; this replaces
  `loading.tsx`/Suspense fallbacks. The dashboard live card shows a pulse skeleton until its stream connects; the
  verify-email success page shows the spinner until its POST returns.
- **Server push**: `GET /dashboard/live` streams the clock and session expiry every second, and patches the account
  card whenever the user changes from any tab (notice shown only in the other tabs). Fan-out is an in-process hub;
  swap for Postgres `LISTEN/NOTIFY` when running more than one instance.
- **Client JS**: only Datastar attributes. Local UI state uses `_`-prefixed signals (not sent to the server). The theme
  toggle writes a `theme` cookie; the server renders `<html class="dark|light">`, and with no cookie the CSS follows
  `prefers-color-scheme`, so there is no flash.
- **i18n**: messages are flat strings with no plurals or placeholders, so `i18n` is a map lookup (no library; add
  go-i18n when a message needs plural rules). Locale = `locale` cookie, then the highest-weighted supported
  `Accept-Language`, then `en`. Keys must be string literals: `TestUsedKeysExistInEveryLocale` scans the templates
  and fails when any locale is missing one, and the render tests fail on any key that renders as itself.
  New keys vs the original (same structure, additions only): `common.home`, `components.themeToggle.*`,
  `pages.home.*`, `pages.dashboard.{settingsForm,deleteAccount,live}.*`,
  `errors.{emailTaken,invalidInput,passwordTooLong,tooManyRequests}`.

## Updating Datastar

Download `bundles/datastar.js` for the new tag from `github.com/starfederation/datastar`, save it as
`static/datastar-<version>.js`, and update `DatastarVersion`, `DatastarFile`, `DatastarSHA256` and the `go:embed` line in
`static/static.go`. `TestDatastarPin` checks the hash and the version header. Keep `datastar-go` on a matching 1.x.
