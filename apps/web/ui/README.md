# apps/web/ui — HTML transport

Server-rendered [templ](https://templ.guide) pages, [Datastar](https://data-star.dev) 1.x actions over SSE, Tailwind v4.
Pages render through the shared component set in [`packages/uikit`](../../../packages/uikit/README.md) (shell, cards,
fields, buttons, alerts, badges, skeletons) and its design tokens, so there are no palette classes or `dark:` variants
here: re-theme in `packages/uikit/theme.css`.

| Package | What |
|---|---|
| `ui` | `ui.New(deps)` (`Routes(mux)` / `Handler()`), the `Auth`/`Users` interfaces, handlers, auth, dashboard and Account pages |
| `ui/view` | the layout (`Layout` for public pages, `SectionLayout(title, Section…)` to highlight the signed-in nav: `uikit.Page` + `uikit.AppShell` with Dashboard/Account, theme toggle, language menu, sign out), the layout state, and app-local components uikit lacks (`Avatar`, `CardFooter`, `AltLink`) |
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
`GET /favicon.ico`; `Handler()` serves them behind the security headers (`security.go`: nonce CSP, `no-store`, and the
rest) with a bare 404 for anything else. POST actions are wrapped in `http.CrossOriginProtection` (stdlib), the UI's
only CSRF layer. Compile-time assertions in `deps.go` keep both interfaces in step
with `packages/auth` and the user service.

`ui.Auth` uses the auth types directly (`ui.User` = `auth.User`, ...) and the handlers map `auth.Err*` values to
messages (`AuthErrors`); unknown errors show "Something went wrong" and are logged. Sign-up and sign-in pass
`CallbackURL: config.AppRoutes.VerifyEmailSuccess`; password reset passes `config.AppRoutes.ResetPassword` as
`redirectTo`. Auth joins app paths with its `BaseURL`, so no origin is hardcoded here, and emails links to those pages
directly (`auth.Config.VerifyEmailPath`): the reset link carries `?token=`, the verify link `?token=&callbackURL=`.
Opening a link spends nothing (mail scanners and link previews fetch links): each page renders a form whose only copy
of the token is a hidden field, sent with `Referrer-Policy: no-referrer` behind https, and the person's POST spends
it. The verify page then continues to the callback when it is a path on this site or `CheckCallbackURL` trusts it
(the mobile app's deep link).

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
  verify-email page's confirm button shows "Verifying…" until its POST returns.
- **Server push**: `GET /dashboard/live` streams the clock and session expiry every second, and patches the account
  card whenever the user changes from any tab (notice shown only in the other tabs). Fan-out is an in-process hub;
  swap for Postgres `LISTEN/NOTIFY` when running more than one instance.
- **Client JS**: only Datastar attributes. Local UI state uses `_`-prefixed signals (not sent to the server).
- **Theme**: colors are uikit tokens defined with `light-dark()`. The theme toggle writes a `theme` cookie and the
  server renders `<html class="dark|light">` from it; with no cookie the system preference applies. The first paint is
  right without a pre-paint script.
- **App name**: the header brand, `<title>`, auth pages and the welcome footer show `Deps.AppName` (`APP_NAME`, default
  `config.DefaultAppName`), read in templates with `view.AppName(view.StateFrom(ctx))`.
- **i18n**: messages are flat strings with no plurals or placeholders, so `i18n` is a map lookup (no library; add
  go-i18n when a message needs plural rules). Locale = `locale` cookie, then the highest-weighted supported
  `Accept-Language`, then `en`. Keys must be string literals: `TestUsedKeysExistInEveryLocale` scans the templates
  and fails when any locale is missing one, and the render tests fail on any key that renders as itself.
  Keys beyond the original app's: `components.{themeToggle,nav,toast}.*`, `pages.home.*`,
  `pages.{login,signup}.description`, `pages.dashboard.live.*`, `pages.account.*`,
  `errors.{emailTaken,invalidInput,passwordTooLong,tooManyRequests}`.

## Updating Datastar

Download `bundles/datastar.js` for the new tag from `github.com/starfederation/datastar`, save it as
`static/datastar-<version>.js`, and update `DatastarVersion`, `DatastarFile`, `DatastarSHA256` and the `go:embed` line in
`static/static.go`. `TestDatastarPin` checks the hash and the version header. Keep `datastar-go` on a matching 1.x.
